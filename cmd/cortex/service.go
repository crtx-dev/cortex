package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// cortexUnitMarker marks unit files written by `cortex service`.
const cortexUnitMarker = "# Managed by cortex. Do not edit manually."

// cortexManagedPrefix introduces the versioned integrity header. The header is
// followed by a SHA-256 of everything below it (managed metadata plus the unit
// body), so any hand edit is detected on the next write, action or uninstall.
const cortexManagedPrefix = "# cortex-managed: "

// cortexHealthPath is the public, read-only liveness endpoint the service
// health check targets. /api/status is intentionally session-protected.
const cortexHealthPath = "/api/health"

var (
	errNotManaged = errors.New("not a managed unit")
	errMalformed  = errors.New("malformed managed unit header")
	errModified   = errors.New("managed unit body no longer matches its recorded checksum")
)

// serviceRunner abstracts systemctl/journalctl so the CLI is testable without
// touching a real systemd user manager. Run returns the captured combined
// output, the process exit code (0 on success, -1 when the command could not
// be launched) and a launch error only.
type serviceRunner interface {
	Run(name string, args ...string) (string, int, error)
	Stream(name string, args ...string) (int, error)
}

type execRunner struct{}

func (execRunner) Run(name string, args ...string) (string, int, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return string(out), ee.ExitCode(), nil
	}
	return string(out), -1, err
}

func (execRunner) Stream(name string, args ...string) (int, error) {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	return -1, err
}

type serviceManager struct {
	unitName string
	unitPath string
	exe      string
	run      serviceRunner
}

type unitMeta struct {
	listen string
	health string
	data   string
}

type serviceOptions struct {
	host         string
	port         string
	listen       string // legacy single-address form (alternative to host/port)
	root         string
	data         string
	publicOrigin string
	trustProxy   bool
	ghDir        string
}

// listener returns the resolved HTTP listen address recorded in the unit: the
// legacy listen address when set, otherwise the trimmed host/port pair joined
// safely (so IPv6 hosts are bracketed). Values are canonicalized before being
// written so surrounding whitespace can never leak into the unit.
func (o serviceOptions) listener() string {
	if o.listen != "" {
		return o.listen
	}
	return net.JoinHostPort(strings.TrimSpace(o.host), strings.TrimSpace(o.port))
}

// userUnitPath returns the installed per-user systemd unit path. It is a var
// seam so tests can redirect the managed unit without touching the host.
var userUnitPath = func(unitName string) string {
	base, err := os.UserConfigDir()
	if err != nil {
		base = os.Getenv("HOME")
	}
	return filepath.Join(base, "systemd", "user", unitName)
}

func (m *serviceManager) systemctl(args ...string) (string, int, error) {
	return m.run.Run("systemctl", append([]string{"--user"}, args...)...)
}

// svcState is a deliberately resolved systemd state category that separates
// command-result validation from the lifecycle meaning uninstall needs:
// definitely running, reloading, refreshing, transitioning, safely-stopped,
// unknown, enabled, not-enabled and masked.
type svcState string

const (
	stateActive     svcState = "active"
	stateReloading  svcState = "reloading"
	stateRefreshing svcState = "refreshing"
	stateTransition svcState = "transitioning"
	stateInactive   svcState = "inactive"
	stateUnknown    svcState = "unknown"
	stateEnabled    svcState = "enabled"
	stateNotEnabled svcState = "not-enabled"
	stateMasked     svcState = "masked"
)

func stateName(s svcState) string { return string(s) }

// exitExpect describes how strongly an output word's exit code is fixed by the
// systemd contract across the supported range (systemd 252 through current).
type exitExpect int

const (
	exitZero    exitExpect = iota // the state must exit 0
	exitNonzero                   // the state must exit nonzero
	exitEither                    // the exit code varies across versions
)

// classifyActive maps an is-active output word to a lifecycle category and its
// exit expectation. Per systemctl-is-active.c: only active and reloading are
// exit 0 in systemd 252-256; refreshing joins them at exit 0 in systemd 257+.
// Inactive, failed, activating, deactivating and maintenance exit 3; not-found
// exits 3 (<=254) or 4 (>=255).
func classifyActive(word string) (svcState, exitExpect, bool) {
	switch word {
	case "active":
		return stateActive, exitZero, true
	case "reloading":
		return stateReloading, exitZero, true
	case "refreshing":
		return stateRefreshing, exitEither, true
	case "inactive", "dead", "failed":
		return stateInactive, exitNonzero, true
	case "activating", "deactivating", "maintenance":
		return stateTransition, exitNonzero, true
	case "not-found", "unknown":
		// not-found exits 3 (<=254) or 4 (>=255); both are nonzero.
		return stateUnknown, exitNonzero, true
	}
	return "", 0, false
}

// classifyEnabled maps an is-enabled output word to a lifecycle category and
// its exit expectation. Per systemctl-is-enabled.c, enabled, enabled-runtime,
// static, alias, indirect and generated exit 0, but only enabled and
// enabled-runtime have enablement links that `systemctl disable` removes; the
// others are lifecycle not-enabled. Disabled, linked, linked-runtime,
// transient, masked, masked-runtime and not-found exit nonzero (not-found 4).
func classifyEnabled(word string) (svcState, exitExpect, bool) {
	switch word {
	case "enabled", "enabled-runtime":
		return stateEnabled, exitZero, true
	case "static", "alias", "indirect", "generated":
		return stateNotEnabled, exitZero, true
	case "disabled", "linked", "linked-runtime", "transient":
		return stateNotEnabled, exitNonzero, true
	case "masked", "masked-runtime":
		return stateMasked, exitNonzero, true
	case "not-found":
		return stateNotEnabled, exitNonzero, true
	case "unknown":
		return stateUnknown, exitNonzero, true
	}
	return "", 0, false
}

// queryState resolves an is-active or is-enabled state, validating the
// output/exit pair against the supported systemd contract: exit-0 states must
// exit 0, exit-nonzero states must exit nonzero, and version-varying states
// accept either. Launch failures, unrecognized output and inconsistent pairs
// surface as errors.
func (m *serviceManager) queryState(verb string) (svcState, error) {
	out, code, err := m.systemctl(verb, m.unitName)
	if err != nil {
		return "", fmt.Errorf("cannot run systemctl %s %s: %w", verb, m.unitName, err)
	}
	word := strings.TrimSpace(out)
	var st svcState
	var expect exitExpect
	var ok bool
	switch verb {
	case "is-active":
		st, expect, ok = classifyActive(word)
	case "is-enabled":
		st, expect, ok = classifyEnabled(word)
	}
	if !ok {
		return "", fmt.Errorf("systemctl %s %s returned unrecognized state %q (exit %d)", verb, m.unitName, word, code)
	}
	switch expect {
	case exitZero:
		if code != 0 {
			return "", fmt.Errorf("systemctl %s %s reported %q but exited %d; inconsistent state result", verb, m.unitName, word, code)
		}
	case exitNonzero:
		if code == 0 {
			return "", fmt.Errorf("systemctl %s %s reported %q but exited 0; inconsistent state result", verb, m.unitName, word)
		}
	}
	return st, nil
}

// rawState returns the exact systemctl is-enabled/is-active output word. The
// install transaction snapshots these raw words rather than the resolved
// lifecycle categories so rollback can reproduce the precise prior state.
func (m *serviceManager) rawState(verb string) (string, error) {
	out, _, err := m.systemctl(verb, m.unitName)
	if err != nil {
		return "", fmt.Errorf("cannot run systemctl %s %s: %w", verb, m.unitName, err)
	}
	word := strings.TrimSpace(out)
	if word == "" {
		return "", fmt.Errorf("systemctl %s %s returned no state", verb, m.unitName)
	}
	return word, nil
}

// restorableEnabledWord reports whether a prior is-enabled raw word can be
// restored exactly by the rollback sequence. Persistent and runtime enablement
// links (enabled, enabled-runtime) and their absence (disabled) are
// restorable. Masked, masked-runtime and not-found are not: a masked unit can
// never be enabled by the install itself, and disabling a loaded unit yields
// disabled rather than not-found. Unit-file states that enable/disable cannot
// reproduce (static, alias, indirect, generated, linked, linked-runtime,
// transient, unknown) are not restorable.
func restorableEnabledWord(word string) bool {
	switch word {
	case "enabled", "enabled-runtime", "disabled":
		return true
	}
	return false
}

// restorableActiveWord reports whether a prior is-active raw word can be
// restored exactly by the rollback sequence. Running and stopped are
// restorable (restart/stop). A start-limited "failed" state is restorable
// because systemctl stop leaves a failed unit reporting failed, so a rollback
// over it reproduces the exact prior word; the install also clears it
// deterministically with reset-failed before starting. Dead, unknown and
// not-found are not restorable, because stop produces inactive rather than
// those words, and transient/failed-from-crash states cannot be reproduced
// deterministically.
func restorableActiveWord(word string) bool {
	switch word {
	case "active", "inactive", "failed":
		return true
	}
	return false
}

// restorablePriorState reports whether the enablement/active pair can be
// reproduced exactly by the rollback ordering (enablement restored first, then
// active state), and whether the install itself can reach the documented
// enabled-and-active state. Masked units are refused because `systemctl
// enable` fails while a unit is masked, so no accepted prior state is masked.
func restorablePriorState(enabledWord, activeWord string) bool {
	return restorableEnabledWord(enabledWord) && restorableActiveWord(activeWord)
}

// enableRestoreSteps returns the systemctl calls that reproduce a prior
// is-enabled word exactly. Enablement is normalized first: the persistent
// enablement link created by the attempted install is removed with disable,
// then the intended persistent or runtime link is recreated, so a runtime-only
// prior never leaves a persistent enablement behind.
func enableRestoreSteps(word, unit string) [][]string {
	switch word {
	case "enabled":
		return [][]string{{"disable", unit}, {"enable", unit}}
	case "enabled-runtime":
		return [][]string{{"disable", unit}, {"enable", "--runtime", unit}}
	default: // disabled
		return [][]string{{"disable", unit}}
	}
}

// activeRestoreArgs returns the systemctl call that reproduces a prior
// is-active word exactly.
func activeRestoreArgs(word, unit string) []string {
	if word == "active" {
		return []string{"restart", unit}
	}
	return []string{"stop", unit}
}

// systemctlTolerantMissing runs a systemctl operation that must exit zero,
// tolerating systemd's "not loaded"/"not found"/"does not exist" results that
// signal the unit was already absent. "does not exist" is the wording systemd
// uses when a unit file fails to load because of a fatal configuration error,
// so a rollback over such a unit is still reported as clean.
func (m *serviceManager) systemctlTolerantMissing(args ...string) error {
	out, code, err := m.systemctl(args...)
	if err != nil {
		return fmt.Errorf("cannot run systemctl %s: %w", strings.Join(args, " "), err)
	}
	if code != 0 {
		lower := strings.ToLower(strings.TrimSpace(out))
		if strings.Contains(lower, "not loaded") || strings.Contains(lower, "not found") || strings.Contains(lower, "no such") || strings.Contains(lower, "does not exist") {
			return nil
		}
		return fmt.Errorf("systemctl %s exited %d: %s", strings.Join(args, " "), code, bounded(strings.TrimSpace(out)))
	}
	return nil
}

// rollbackInstall restores the pre-install state after a failed publish or
// lifecycle step. For a reinstall it restores the prior unit bytes, reloads
// systemd, then reproduces the exact prior enablement and active states. For a
// failed fresh install it stops and disables the newly installed unit while it
// is still loaded, then removes the unit file and reloads systemd, so no
// enablement link or active service is left behind. It returns a "rollback
// incomplete" description when any restoration step fails.
func (m *serviceManager) rollbackInstall(priorUnit []byte, hadUnit bool, priorEnabledWord, priorActiveWord string) string {
	var errs []string
	if hadUnit {
		if err := writeManagedUnit(m.unitPath, string(priorUnit)); err != nil {
			errs = append(errs, fmt.Sprintf("restore unit: %v", err))
		}
	} else {
		if err := m.systemctlTolerantMissing("stop", m.unitName); err != nil {
			errs = append(errs, fmt.Sprintf("stop new unit: %v", err))
		}
		if err := m.systemctlTolerantMissing("disable", m.unitName); err != nil {
			errs = append(errs, fmt.Sprintf("disable new unit: %v", err))
		}
		if err := os.Remove(m.unitPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Sprintf("remove new unit: %v", err))
		}
	}
	if err := m.systemctlSuccess("daemon-reload"); err != nil {
		errs = append(errs, fmt.Sprintf("reload systemd: %v", err))
	}
	if hadUnit {
		for _, args := range enableRestoreSteps(priorEnabledWord, m.unitName) {
			if err := m.systemctlSuccess(args...); err != nil {
				errs = append(errs, fmt.Sprintf("restore enablement %q: %v", priorEnabledWord, err))
				break
			}
		}
		if err := m.systemctlSuccess(activeRestoreArgs(priorActiveWord, m.unitName)...); err != nil {
			errs = append(errs, fmt.Sprintf("restore active state %q: %v", priorActiveWord, err))
		}
	}
	if len(errs) == 0 {
		return ""
	}
	return "; rollback incomplete: " + strings.Join(errs, "; ")
}

// bounded caps captured command output used in errors.
func bounded(s string) string {
	const max = 2000
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

// systemctlSuccess runs a systemctl operation that must exit zero; launch
// failures and nonzero exits are both errors. Call sites must never discard
// the exit code at strict-operation sites.
func (m *serviceManager) systemctlSuccess(args ...string) error {
	out, code, err := m.systemctl(args...)
	if err != nil {
		return fmt.Errorf("cannot run systemctl %s: %w", strings.Join(args, " "), err)
	}
	if code != 0 {
		return fmt.Errorf("systemctl %s exited %d: %s", strings.Join(args, " "), code, bounded(strings.TrimSpace(out)))
	}
	return nil
}

// validateNoControl rejects CR, LF, NUL and other control characters so no
// user-supplied value can inject directives into a systemd unit or confuse
// status metadata.
func validateNoControl(v, what string) error {
	for i := 0; i < len(v); i++ {
		if v[i] < 0x20 || v[i] == 0x7f {
			return fmt.Errorf("%s %q contains a control character", what, v)
		}
	}
	return nil
}

// systemdExecQuote quotes a single token for an ExecStart= command line.
// Command lines are parsed with shell-like quoting, so double quotes group
// whitespace. Per systemd.exec, complete ${VAR} sequences are expanded at
// runtime, so every literal '$' is doubled ('$$') and every '%' is doubled
// ('%%') so it is never read as a specifier prefix. Double quotes and
// backslashes are backslash-escaped. Backticks and single quotes are literal
// characters in systemd command lines and must not be escaped (a backslash
// would be preserved as a stray character).
func systemdExecQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '%', '$':
			b.WriteByte(c)
			b.WriteByte(c)
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// systemdEnvValue quotes the value of an Environment= assignment. Environment
// lines are parsed like command lines (double quotes group whitespace and
// backslash escapes quote/backslash), but environment values are never
// dollar-expanded, so '$' and backticks are literal and must not be escaped.
// '%' is doubled so it is never read as a specifier prefix.
func systemdEnvValue(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '%':
			b.WriteString("%%")
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// systemdUnitPath escapes a value for a single-token systemd directive such as
// WorkingDirectory=. Such directives are not shell-parsed: quote characters,
// backslashes, spaces and other punctuation are literal path characters and
// must not be quoted or escaped (surrounding quotes would become part of the
// path). The only unit-file escape that applies is doubling '%' so a path
// containing '%' is never misread as a specifier prefix.
func systemdUnitPath(s string) string {
	return strings.ReplaceAll(s, "%", "%%")
}

// classifySystemdFailure recognizes common systemd failure messages so the
// install transaction can report the failure category instead of a generic
// "systemctl exited 1" line. The categories distinguish an invalid generated
// unit, a start-rate-limit refusal, and an unavailable systemd bus.
func classifySystemdFailure(msg string) string {
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "bad unit file setting"),
		strings.Contains(lower, "fatal error"),
		strings.Contains(lower, "not absolute"),
		strings.Contains(lower, "unknown key"),
		strings.Contains(lower, "invalid argument"):
		return "generated unit is invalid"
	case strings.Contains(lower, "start request repeated too quickly"):
		return "start rate limit exceeded"
	case strings.Contains(lower, "failed to connect to bus"), strings.Contains(lower, "connection refused"):
		return "systemd bus unavailable"
	}
	return ""
}

// hostGHConfigDir resolves the host GitHub CLI configuration directory when
// hosts.yml exists. It is used to preserve gh access for the user service.
func hostGHConfigDir() string {
	if v := strings.TrimSpace(os.Getenv("GH_CONFIG_DIR")); v != "" {
		if _, err := os.Stat(filepath.Join(v, "hosts.yml")); err == nil {
			return v
		}
	}
	if v := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); v != "" {
		d := filepath.Join(v, "gh")
		if _, err := os.Stat(filepath.Join(d, "hosts.yml")); err == nil {
			return d
		}
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		d := filepath.Join(home, ".config", "gh")
		if _, err := os.Stat(filepath.Join(d, "hosts.yml")); err == nil {
			return d
		}
	}
	return ""
}

// renderCortexUnitBody renders the systemd directives (no managed header).
// Every value is escaped according to the directive it appears in: ExecStart
// tokens use command-line quoting, Environment values use assignment quoting,
// and single-token path directives such as WorkingDirectory use unquoted path
// escaping (no shell quoting ever applies to a path value). The finite restart
// policy mirrors the approved crash-loop contract: StartLimitIntervalSec and
// StartLimitBurst live in [Unit] and Restart/RestartSec in [Service].
func renderCortexUnitBody(exe string, opts serviceOptions) string {
	var b strings.Builder
	b.WriteString("[Unit]\n")
	b.WriteString("Description=Cortex coding agent\n")
	b.WriteString("After=network-online.target\n")
	b.WriteString("Wants=network-online.target\n")
	b.WriteString("StartLimitIntervalSec=60\n")
	b.WriteString("StartLimitBurst=5\n\n")
	b.WriteString("[Service]\n")
	b.WriteString("Type=simple\n")
	b.WriteString("ExecStart=" + systemdExecQuote(exe))
	if opts.listen != "" {
		b.WriteString(" " + systemdExecQuote("--listen") + " " + systemdExecQuote(opts.listen))
	} else {
		b.WriteString(" " + systemdExecQuote("--host") + " " + systemdExecQuote(strings.TrimSpace(opts.host)))
		b.WriteString(" " + systemdExecQuote("--port") + " " + systemdExecQuote(strings.TrimSpace(opts.port)))
	}
	b.WriteString(" " + systemdExecQuote("--root") + " " + systemdExecQuote(opts.root))
	b.WriteString(" " + systemdExecQuote("--data") + " " + systemdExecQuote(opts.data))
	if opts.publicOrigin != "" {
		b.WriteString(" " + systemdExecQuote("--public-origin") + " " + systemdExecQuote(opts.publicOrigin))
	}
	if opts.trustProxy {
		b.WriteString(" " + systemdExecQuote("--trust-proxy"))
	}
	b.WriteString("\n")
	b.WriteString("WorkingDirectory=" + systemdUnitPath(filepath.Dir(exe)) + "\n")
	b.WriteString("Restart=on-failure\n")
	b.WriteString("RestartSec=3\n")
	b.WriteString("Environment=HOME=%h\n")
	// A systemd user service does not read the user's interactive shell
	// profile. Include OpenCode's official per-user install directory and the
	// usual per-user/system binary directories explicitly so the service sees
	// the same separately-installed opencode binary after login or reboot.
	// %h is intentionally left as a systemd specifier here (not escaped as a
	// literal percent) and the value contains no whitespace requiring quoting.
	b.WriteString("Environment=PATH=%h/.opencode/bin:%h/.local/bin:/usr/local/bin:/usr/bin:/bin\n")
	if opts.ghDir != "" {
		b.WriteString("Environment=GH_CONFIG_DIR=" + systemdEnvValue(opts.ghDir) + "\n")
	}
	b.WriteString("\n[Install]\n")
	b.WriteString("WantedBy=default.target\n")
	return b.String()
}

// buildCortexUnit renders the full managed unit: a marker line, a versioned
// integrity header carrying the SHA-256 of the managed content below it, the
// runtime metadata (listen/health) used by `service status`, and the body.
func buildCortexUnit(exe string, opts serviceOptions) string {
	content := "# cortex-listen: " + opts.listener() + "\n# cortex-data: " + opts.data + "\n# cortex-health: " + cortexHealthPath + "\n" + renderCortexUnitBody(exe, opts)
	sum := sha256.Sum256([]byte(content))
	header := cortexUnitMarker + "\n" + cortexManagedPrefix + "v1 sha256=" + hex.EncodeToString(sum[:]) + "\n"
	return header + content
}

// readManagedUnit validates a unit's managed integrity and returns its runtime
// metadata. It returns errNotManaged when the marker is absent, errMalformed
// for duplicate/malformed headers or metadata, and errModified when the body
// below the header no longer matches the recorded checksum. Metadata must
// appear exactly once, contain no control characters, and the health path must
// be the application-owned health endpoint.
func readManagedUnit(path string) (unitMeta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return unitMeta{}, err
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) < 3 || lines[0] != cortexUnitMarker {
		return unitMeta{}, errNotManaged
	}
	count := 0
	for _, ln := range lines {
		if strings.HasPrefix(ln, cortexManagedPrefix) {
			count++
		}
	}
	if count != 1 || !strings.HasPrefix(lines[1], cortexManagedPrefix) {
		return unitMeta{}, errMalformed
	}
	sm := regexp.MustCompile(`^# cortex-managed: v1 sha256=([0-9a-f]{64})$`).FindStringSubmatch(lines[1])
	if sm == nil {
		return unitMeta{}, errMalformed
	}
	content := strings.Join(lines[2:], "\n")
	sum := sha256.Sum256([]byte(content))
	if hex.EncodeToString(sum[:]) != sm[1] {
		return unitMeta{}, errModified
	}
	meta := unitMeta{}
	listenSeen, healthSeen, dataSeen := 0, 0, 0
	for _, ln := range lines[2:] {
		switch {
		case strings.HasPrefix(ln, "# cortex-listen: "):
			listenSeen++
			if listenSeen > 1 {
				return unitMeta{}, errMalformed
			}
			meta.listen = strings.TrimSpace(strings.TrimPrefix(ln, "# cortex-listen: "))
		case strings.HasPrefix(ln, "# cortex-data: "):
			dataSeen++
			if dataSeen > 1 {
				return unitMeta{}, errMalformed
			}
			meta.data = strings.TrimSpace(strings.TrimPrefix(ln, "# cortex-data: "))
		case strings.HasPrefix(ln, "# cortex-health: "):
			healthSeen++
			if healthSeen > 1 {
				return unitMeta{}, errMalformed
			}
			meta.health = strings.TrimSpace(strings.TrimPrefix(ln, "# cortex-health: "))
		}
	}
	if listenSeen != 1 || healthSeen != 1 || meta.listen == "" || meta.health == "" {
		return unitMeta{}, errMalformed
	}
	if meta.health != cortexHealthPath {
		return unitMeta{}, errMalformed
	}
	if err := validateNoControl(meta.listen, "listen"); err != nil {
		return unitMeta{}, errMalformed
	}
	// The data-dir marker is additive: older managed units predate it and stay
	// valid for lifecycle operations, but destructive commands that need the
	// installed data directory must fail closed when it is absent.
	if dataSeen == 1 {
		if meta.data == "" {
			return unitMeta{}, errMalformed
		}
		if err := validateNoControl(meta.data, "data-dir"); err != nil {
			return unitMeta{}, errMalformed
		}
	}
	return meta, nil
}

// InstalledDataDir returns the data directory recorded by the installed
// managed service unit. The boolean is false when Cortex is not installed. A
// present but foreign, malformed or modified unit is an error so destructive
// CLI operations never silently operate on a different directory. An existing
// managed unit that predates the data-dir marker is likewise an error: its
// data directory cannot be known, so the caller must supply --data explicitly
// rather than fall back.
func InstalledDataDir() (string, bool, error) {
	meta, err := readManagedUnit(userUnitPath("cortex.service"))
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("cannot use installed service configuration: %w", err)
	}
	if strings.TrimSpace(meta.data) == "" {
		return "", false, fmt.Errorf("installed service unit predates data-directory metadata; pass --data explicitly")
	}
	return meta.data, true, nil
}

// writeManagedUnit writes a unit atomically. An existing file is only replaced
// when it is a valid unmodified managed unit; hand-edited or foreign units are
// refused rather than silently overwritten.
func writeManagedUnit(path, content string) error {
	if _, err := os.Stat(path); err == nil {
		if _, err := readManagedUnit(path); err != nil {
			return fmt.Errorf("refusing to overwrite %s: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".cortex-unit-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// resolveExecutable validates the executable path used in a unit. The supplied
// path must already be absolute; empty, relative, transient (build-cache,
// temp) paths and paths containing control characters are rejected so we never
// install a broken or ephemeral unit. systemd refuses to load a unit whose
// ExecStart executable path contains a double quote, single quote, backslash,
// dollar sign, or leading/trailing whitespace ("Executable path contains
// special characters"), and those characters cannot be encoded into a valid
// ExecStart, so they are rejected up front rather than emitting a unit systemd
// will refuse to start. The executable's parent directory feeds WorkingDirectory,
// so this restriction also keeps that path free of unsafe forms.
func resolveExecutable(exe string) (string, error) {
	if strings.TrimSpace(exe) == "" {
		return "", errors.New("empty executable path")
	}
	if !filepath.IsAbs(exe) {
		return "", fmt.Errorf("executable path %q is not absolute", exe)
	}
	if err := validateNoControl(exe, "executable path"); err != nil {
		return "", err
	}
	abs := filepath.Clean(exe)
	if strings.TrimSpace(abs) != abs {
		return "", fmt.Errorf("executable path %q must not have leading or trailing whitespace", abs)
	}
	for _, c := range abs {
		switch c {
		case '"', '\'', '\\', '$':
			return "", fmt.Errorf("executable path %q contains %q, which systemd rejects in an ExecStart executable path", abs, c)
		}
	}
	if strings.HasPrefix(abs, os.TempDir()) {
		return "", fmt.Errorf("executable path %q is transient; install cortex somewhere stable first", abs)
	}
	if strings.Contains(abs, string(filepath.Separator)+"go-build"+string(filepath.Separator)) {
		return "", fmt.Errorf("executable path %q looks like a Go build cache path", abs)
	}
	if st, err := os.Stat(abs); err != nil || st.IsDir() {
		return "", fmt.Errorf("executable %q is not a file", abs)
	}
	return abs, nil
}

// healthCheck requires the Cortex health contract: a 2xx response whose body
// is a JSON object with ok:true. Any other 2xx JSON object (an empty object,
// ok:false, an array, malformed JSON) is rejected so a foreign JSON-speaking
// process occupying the listener cannot impersonate Cortex on the install
// health gate.
func healthCheck(url string) error {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("expected 2xx, got HTTP %d", resp.StatusCode)
	}
	if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "json") {
		return fmt.Errorf("expected a JSON response, got %q", resp.Header.Get("Content-Type"))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		return fmt.Errorf("expected a JSON object response: %v", err)
	}
	if v["ok"] != true {
		return fmt.Errorf("expected the Cortex health contract {\"ok\":true}, got %s", strings.TrimSpace(string(body)))
	}
	return nil
}

// installHealthDeadline and installHealthPollInterval bound the post-start
// readiness verification of `service install`. They are overridable so tests
// and the lifecycle exercise can drive the poll deterministically.
var (
	installHealthPollInterval = 250 * time.Millisecond
	installHealthDeadline     = 15 * time.Second
	// healthProbe is the HTTP liveness probe used by waitReady; tests override
	// it so installs in the sandbox do not require a live listener.
	healthProbe = healthCheck
)

// waitReady verifies, within a bounded deadline, that the installed unit
// reaches the active state and answers its /api/health endpoint with a valid
// JSON 2xx response. This is the install-time transactional health gate: a
// systemctl start/restart can return zero before the process finishes
// starting, and the process can then fail immediately, so install does not
// report success until the service is demonstrably alive. It distinguishes an
// immediate failed/inactive/not-found state, a state-query failure, a
// deadline timeout, and an invalid health response, so the caller can roll
// back the install transaction for every failure class.
func (m *serviceManager) waitReady(listen string) error {
	start := time.Now()
	var lastHealthErr error
	for {
		st, err := m.queryState("is-active")
		if err != nil {
			return fmt.Errorf("cannot verify %s state after start: %w", m.unitName, err)
		}
		switch st {
		case stateActive:
			// A forked simple service reports active before its listener is
			// bound, so a transient health failure is retried until the
			// deadline rather than failing the install on the first poll.
			if err := healthProbe("http://" + listen + cortexHealthPath); err != nil {
				lastHealthErr = err
			} else {
				return nil
			}
		case stateInactive, stateUnknown:
			return fmt.Errorf("%s is %q immediately after start; install failed", m.unitName, stateName(st))
		default:
			// Transitional states (activating, reloading, refreshing) are not
			// terminal: keep polling until the deadline.
		}
		if time.Since(start) >= installHealthDeadline {
			if lastHealthErr != nil {
				return fmt.Errorf("service is active but its health check failed: %w", lastHealthErr)
			}
			return fmt.Errorf("%s did not become active and healthy within %s", m.unitName, installHealthDeadline)
		}
		time.Sleep(installHealthPollInterval)
	}
}

// requireManaged verifies the unit file at the expected path is a valid
// unmodified managed unit before any lifecycle operation.
func (m *serviceManager) requireManaged(verb string) error {
	if _, err := readManagedUnit(m.unitPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("refusing to %s %s: unit is not installed", verb, m.unitName)
		}
		return fmt.Errorf("refusing to %s %s: %w", verb, m.unitName, err)
	}
	return nil
}

// installStepDef describes one lifecycle step of the install transaction. A
// tolerant step (reset-failed) is allowed to report a missing/unloaded unit.
type installStepDef struct {
	verb     string
	args     []string
	tolerant bool
}

// installStep runs one install lifecycle step. Strict steps must exit zero;
// tolerant steps accept systemd's absent-unit results. Recognizable failures
// are reported with their category (invalid generated unit, start-rate-limit
// refusal, unavailable bus) so the error distinguishes why the step failed.
func (m *serviceManager) installStep(step installStepDef) error {
	var err error
	if step.tolerant {
		err = m.systemctlTolerantMissing(step.args...)
	} else {
		err = m.systemctlSuccess(step.args...)
	}
	if err == nil {
		return nil
	}
	if cat := classifySystemdFailure(err.Error()); cat != "" {
		return fmt.Errorf("%s %s: %s: %w", step.verb, m.unitName, cat, err)
	}
	return fmt.Errorf("%s %s: %w", step.verb, m.unitName, err)
}

func (m *serviceManager) install(opts serviceOptions, out io.Writer) error {
	for _, v := range []struct{ val, name string }{
		{opts.listener(), "listen"}, {opts.root, "root"}, {opts.data, "data"}, {opts.publicOrigin, "public-origin"}, {opts.ghDir, "gh config dir"},
	} {
		if err := validateNoControl(v.val, v.name); err != nil {
			return err
		}
	}
	unit := buildCortexUnit(m.exe, opts)
	priorUnit, hadUnit := []byte(nil), false
	if b, err := os.ReadFile(m.unitPath); err == nil {
		hadUnit = true
		priorUnit = b
		if _, err := readManagedUnit(m.unitPath); err != nil {
			return fmt.Errorf("refusing to reinstall %s: %w", m.unitName, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Snapshot the exact prior enablement and active states before any
	// mutation so rollback can reproduce them precisely. States that cannot be
	// restored exactly are refused up front rather than flattened.
	priorEnabledWord, priorActiveWord := "", ""
	if hadUnit {
		var err error
		if priorEnabledWord, err = m.rawState("is-enabled"); err != nil {
			return err
		}
		if !restorableEnabledWord(priorEnabledWord) {
			return fmt.Errorf("refusing to reinstall %s: prior enablement state %q cannot be restored exactly; disable or unmask it first", m.unitName, priorEnabledWord)
		}
		if priorActiveWord, err = m.rawState("is-active"); err != nil {
			return err
		}
		if !restorableActiveWord(priorActiveWord) {
			return fmt.Errorf("refusing to reinstall %s: prior active state %q cannot be restored exactly; stop or restart it first", m.unitName, priorActiveWord)
		}
		if !restorablePriorState(priorEnabledWord, priorActiveWord) {
			return fmt.Errorf("refusing to reinstall %s: prior state %s+%s cannot be restored exactly; unmask it first", m.unitName, priorEnabledWord, priorActiveWord)
		}
		// True no-op: a byte-identical unit that is already enabled and active
		// needs no rewrite, reload or restart, but it still must pass the same
		// bounded Cortex health verification before success is reported. An
		// active-but-wedged process or an occupied/incorrect listener is an
		// honest failure; because this path makes no mutation it needs no
		// rollback.
		if string(priorUnit) == unit && priorEnabledWord == "enabled" && priorActiveWord == "active" {
			if err := m.waitReady(opts.listener()); err != nil {
				return err
			}
			fmt.Fprintf(out, "%s is already installed, enabled and active; nothing to do.\n", m.unitName)
			return nil
		}
	}
	changed := !hadUnit || string(priorUnit) != unit
	if changed {
		if err := writeManagedUnit(m.unitPath, unit); err != nil {
			return err
		}
		// A changed unit must restart (not merely start) so the new
		// configuration takes effect on an already-running process. reset-failed
		// runs immediately before the start so a prior crash-loop that left the
		// unit start-limited cannot refuse the restart; it is tolerant because a
		// fresh unit is not loaded yet.
		for _, step := range []installStepDef{
			{"reloading systemd", []string{"daemon-reload"}, false},
			{"enabling", []string{"enable", m.unitName}, false},
			{"clearing previous failed state", []string{"reset-failed", m.unitName}, true},
			{"starting", []string{"restart", m.unitName}, false},
		} {
			if err := m.installStep(step); err != nil {
				if rb := m.rollbackInstall(priorUnit, hadUnit, priorEnabledWord, priorActiveWord); rb != "" {
					return fmt.Errorf("%w%s", err, rb)
				}
				return err
			}
		}
	} else {
		// Unit bytes are unchanged: only perform the lifecycle work required
		// to reach the documented installed state (enabled and active). A prior
		// inactive unit that is still start-limited (for example after a
		// crash-loop) must have its failed state cleared before start.
		steps := []installStepDef{}
		if priorEnabledWord != "enabled" {
			steps = append(steps, installStepDef{"enabling", []string{"enable", m.unitName}, false})
		}
		if priorActiveWord != "active" {
			steps = append(steps, installStepDef{"clearing previous failed state", []string{"reset-failed", m.unitName}, true})
			steps = append(steps, installStepDef{"starting", []string{"start", m.unitName}, false})
		}
		for _, step := range steps {
			if err := m.installStep(step); err != nil {
				if rb := m.rollbackInstall(priorUnit, hadUnit, priorEnabledWord, priorActiveWord); rb != "" {
					return fmt.Errorf("bringing %s to the installed state: %w%s", m.unitName, err, rb)
				}
				return fmt.Errorf("bringing %s to the installed state: %w", m.unitName, err)
			}
		}
	}
	// Install-time transactional health gate: a start/restart job can return
	// zero before the process finishes starting, so install must not report
	// success until the unit is active and answers /api/health. Every failure
	// class (immediate failure, state-query failure, timeout, invalid health
	// response) triggers the same rollback as any other failed lifecycle step.
	if err := m.waitReady(opts.listener()); err != nil {
		if rb := m.rollbackInstall(priorUnit, hadUnit, priorEnabledWord, priorActiveWord); rb != "" {
			return fmt.Errorf("%w%s", err, rb)
		}
		return err
	}
	active, err := m.queryState("is-active")
	if err != nil {
		return fmt.Errorf("cannot confirm %s active state after install: %w", m.unitName, err)
	}
	fmt.Fprintf(out, "unit:   %s\n", m.unitName)
	fmt.Fprintf(out, "file:   %s\n", m.unitPath)
	fmt.Fprintf(out, "state:  %s\n", active)
	fmt.Fprintf(out, "url:    http://%s\n", opts.listener())
	return nil
}

func (m *serviceManager) action(verb string, out io.Writer) error {
	if err := m.requireManaged(verb); err != nil {
		return err
	}
	o, code, err := m.systemctl(verb, m.unitName)
	if out != nil && strings.TrimSpace(o) != "" {
		fmt.Fprintln(out, strings.TrimSpace(o))
	}
	if err != nil {
		return fmt.Errorf("cannot run systemctl %s %s: %w", verb, m.unitName, err)
	}
	if code != 0 {
		return fmt.Errorf("systemctl %s %s exited %d: %s", verb, m.unitName, code, bounded(strings.TrimSpace(o)))
	}
	return nil
}

func (m *serviceManager) status(out io.Writer, version string) error {
	meta, err := readManagedUnit(m.unitPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s is not installed (no unit at %s)", m.unitName, m.unitPath)
		}
		return fmt.Errorf("%s unit is not valid: %w", m.unitName, err)
	}
	enabled, err := m.queryState("is-enabled")
	if err != nil {
		return fmt.Errorf("cannot determine %s enablement state: %w", m.unitName, err)
	}
	active, err := m.queryState("is-active")
	if err != nil {
		return fmt.Errorf("cannot determine %s service state: %w", m.unitName, err)
	}
	pid, _, _ := m.systemctl("show", "-p", "MainPID", "--value", m.unitName)
	fmt.Fprintf(out, "unit:    %s\n", m.unitName)
	fmt.Fprintf(out, "file:    %s\n", m.unitPath)
	fmt.Fprintf(out, "enabled: %s\n", enabled)
	fmt.Fprintf(out, "active:  %s\n", active)
	fmt.Fprintf(out, "pid:     %s\n", strings.TrimSpace(pid))
	fmt.Fprintf(out, "version: %s\n", version)
	fmt.Fprintf(out, "listen:  %s\n", meta.listen)
	fmt.Fprintf(out, "url:     http://%s\n", meta.listen)
	if active != stateActive {
		return fmt.Errorf("%s is %q; expected active", m.unitName, active)
	}
	if err := healthCheck("http://" + meta.listen + meta.health); err != nil {
		fmt.Fprintf(out, "health:  unreachable (%v)\n", err)
		return fmt.Errorf("service is active but its health check failed: %v", err)
	}
	fmt.Fprintln(out, "health:  ok")
	return nil
}

func (m *serviceManager) logs(follow bool, out io.Writer) error {
	if err := m.requireManaged("view logs for"); err != nil {
		return err
	}
	args := []string{"--user-unit", m.unitName}
	if follow {
		args = append(args, "-f")
		code, err := m.run.Stream("journalctl", args...)
		if err != nil {
			return err
		}
		if code != 0 {
			return fmt.Errorf("journalctl exited with status %d", code)
		}
		return nil
	}
	o, code, err := m.run.Run("journalctl", args...)
	if err != nil {
		return fmt.Errorf("cannot run journalctl: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("journalctl exited %d: %s", code, bounded(strings.TrimSpace(o)))
	}
	fmt.Fprint(out, o)
	return nil
}

func syncDir(dir string) {
	if f, err := os.Open(dir); err == nil {
		_ = f.Sync()
		_ = f.Close()
	}
}

var (
	linkFile     = os.Link
	removeFile   = os.Remove
	randomSuffix = func() (string, error) {
		b := make([]byte, 8)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		return hex.EncodeToString(b), nil
	}
)

// backupManagedUnit moves the managed unit aside to a unique hidden backup name
// in the same directory. It uses an exclusive hard link so an existing retained
// backup is never overwritten; the original is unlinked only after the backup
// link exists, and on any failure the original stays intact with no backup
// artifact left behind.
func backupManagedUnit(path string) (string, error) {
	dir := filepath.Dir(path)
	for i := 0; i < 32; i++ {
		suffix, err := randomSuffix()
		if err != nil {
			return "", fmt.Errorf("cannot generate a backup name: %w", err)
		}
		backup := filepath.Join(dir, "."+filepath.Base(path)+".unit-backup-"+suffix)
		if err := linkFile(path, backup); err != nil {
			if errors.Is(err, os.ErrExist) {
				continue // candidate already exists; try another name
			}
			return "", err
		}
		if err := removeFile(path); err != nil {
			_ = os.Remove(backup)
			return "", fmt.Errorf("cannot remove the original after backing it up: %w", err)
		}
		syncDir(dir)
		return backup, nil
	}
	return "", errors.New("could not allocate a unique backup name")
}

// restoreFromBackup atomically restores the managed unit at its original path
// after a failed final daemon-reload. It uses a hard link so a concurrently
// created replacement is never overwritten; on conflict the backup is retained
// at its recovery path and that path is reported.
func restoreFromBackup(orig, backup string) error {
	if err := os.Link(backup, orig); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("refusing to overwrite a concurrently created unit at %s; the original unit is preserved at %s", orig, backup)
		}
		return err
	}
	if err := os.Remove(backup); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	syncDir(filepath.Dir(orig))
	return nil
}

func (m *serviceManager) uninstall(out io.Writer) error {
	if _, err := readManagedUnit(m.unitPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s is not installed", m.unitName)
		}
		return fmt.Errorf("refusing to uninstall %s: %w", m.unitName, err)
	}
	active, err := m.queryState("is-active")
	if err != nil {
		return fmt.Errorf("cannot determine %s state before uninstall: %w", m.unitName, err)
	}
	if active == stateActive || active == stateReloading || active == stateRefreshing || active == stateTransition {
		if err := m.systemctlSuccess("stop", m.unitName); err != nil {
			return fmt.Errorf("stop %s failed: %w", m.unitName, err)
		}
		after, err := m.queryState("is-active")
		if err != nil {
			return fmt.Errorf("cannot verify %s stopped after stop: %w", m.unitName, err)
		}
		if after != stateInactive {
			return fmt.Errorf("%s still reports %q after stop; not removing the unit", m.unitName, stateName(after))
		}
	} else if active == stateInactive {
		fmt.Fprintf(out, "note: %s is inactive; nothing to stop\n", m.unitName)
	} else {
		return fmt.Errorf("%s is in %q; cannot confirm it is safely stopped before uninstall", m.unitName, stateName(active))
	}
	enabled, err := m.queryState("is-enabled")
	if err != nil {
		return fmt.Errorf("cannot determine %s enablement before uninstall: %w", m.unitName, err)
	}
	if enabled == stateEnabled {
		if err := m.systemctlSuccess("disable", m.unitName); err != nil {
			return fmt.Errorf("disable %s failed: %w", m.unitName, err)
		}
		after, err := m.queryState("is-enabled")
		if err != nil {
			return fmt.Errorf("cannot verify %s disabled after disable: %w", m.unitName, err)
		}
		if after != stateNotEnabled && after != stateMasked {
			return fmt.Errorf("%s still reports %q after disable; not removing the unit", m.unitName, stateName(after))
		}
	} else if enabled == stateNotEnabled || enabled == stateMasked {
		fmt.Fprintf(out, "note: %s is %s; nothing to disable\n", m.unitName, stateName(enabled))
	} else {
		return fmt.Errorf("%s enablement is %q; cannot confirm it is disabled before uninstall", m.unitName, stateName(enabled))
	}
	// Move the managed unit aside, then reload: on failure the original inode
	// is atomically restored, so no partial managed file can ever exist at the
	// managed path.
	backup, err := backupManagedUnit(m.unitPath)
	if err != nil {
		return fmt.Errorf("cannot move %s aside for uninstall: %w", m.unitName, err)
	}
	if err := m.systemctlSuccess("daemon-reload"); err != nil {
		if restoreErr := restoreFromBackup(m.unitPath, backup); restoreErr != nil {
			return fmt.Errorf("reloading systemd after removing %s: %w; additionally failed to restore the unit: %v", m.unitName, err, restoreErr)
		}
		if reloadErr := m.systemctlSuccess("daemon-reload"); reloadErr != nil {
			return fmt.Errorf("reloading systemd after removing %s: %w; the managed unit was restored but the follow-up reload also failed: %v", m.unitName, err, reloadErr)
		}
		return fmt.Errorf("reloading systemd after removing %s: %w; the managed unit was restored", m.unitName, err)
	}
	if err := os.Remove(backup); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	syncDir(filepath.Dir(m.unitPath))
	fmt.Fprintf(out, "Removed %s. Application configuration, conversations and data were preserved.\n", m.unitName)
	return nil
}

func runService(args []string, version string) int {
	cmd := "status"
	rest := args
	for i, a := range args {
		if a != "" && !strings.HasPrefix(a, "-") {
			cmd = a
			rest = append(append([]string{}, args[:i]...), args[i+1:]...)
			break
		}
	}
	fs := flag.NewFlagSet("cortex service "+cmd, flag.ContinueOnError)
	system := fs.Bool("system", false, "install a system-wide unit (not yet supported; user mode is the default)")
	follow := fs.Bool("follow", false, "follow new journal output")
	host := fs.String("host", "", "HTTP bind host recorded in the unit (default 127.0.0.1; CORTEX_HOST overrides, CLI wins)")
	port := fs.String("port", "", "HTTP bind port recorded in the unit, 1-65535 (default 7331; CORTEX_PORT overrides, CLI wins)")
	listen := fs.String("listen", "", "HTTP listen address recorded in the unit (legacy; alternative to --host/--port)")
	root := fs.String("root", "", "workspace root recorded in the unit")
	data := fs.String("data", "", "data directory recorded in the unit")
	trustProxy := fs.Bool("trust-proxy", false, "trust forwarding headers from a direct loopback reverse proxy")
	publicOrigin := fs.String("public-origin", "", "canonical external origin recorded in the unit")
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	if *system {
		fmt.Fprintln(os.Stderr, "cortex: system-wide service mode is not yet supported; use user mode (default) or the foreground command")
		return 2
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		fmt.Fprintln(os.Stderr, "cortex: systemctl not found; is systemd installed?")
		return 1
	}
	m := &serviceManager{
		unitName: "cortex.service",
		unitPath: userUnitPath("cortex.service"),
		run:      execRunner{},
	}
	switch cmd {
	case "install":
		listenSet := flagProvided(fs, "listen")
		hostSet := flagProvided(fs, "host")
		portSet := flagProvided(fs, "port")
		listenVal, hostVal, portVal := "", "", ""
		if listenSet {
			if hostSet || portSet {
				fmt.Fprintln(os.Stderr, "cortex: --listen cannot be combined with --host or --port")
				return 2
			}
			listenVal = *listen
		} else {
			h, p, err := resolveHostPort(*host, *port, hostSet, portSet)
			if err != nil {
				fmt.Fprintln(os.Stderr, "cortex:", err)
				return 2
			}
			hostVal, portVal = h, p
		}
		if *root == "" {
			if home, err := os.UserHomeDir(); err == nil && home != "" {
				*root = home
			}
		}
		if *data == "" {
			if d, err := os.UserConfigDir(); err == nil {
				*data = filepath.Join(d, "cortex")
			}
		}
		if !filepath.IsAbs(*root) {
			fmt.Fprintln(os.Stderr, "cortex: --root must be an absolute path")
			return 2
		}
		if !filepath.IsAbs(*data) {
			fmt.Fprintln(os.Stderr, "cortex: --data must be an absolute path")
			return 2
		}
		exe, err := os.Executable()
		if err != nil {
			fmt.Fprintln(os.Stderr, "cortex:", err)
			return 1
		}
		exe, err = resolveExecutable(exe)
		if err != nil {
			fmt.Fprintln(os.Stderr, "cortex:", err)
			return 1
		}
		m.exe = exe
		addr := net.JoinHostPort(hostVal, portVal)
		if listenVal != "" {
			addr = listenVal
		}
		for _, v := range []struct{ val, name string }{
			{addr, "listen"}, {*root, "root"}, {*data, "data"}, {*publicOrigin, "public-origin"},
		} {
			if err := validateNoControl(v.val, v.name); err != nil {
				fmt.Fprintln(os.Stderr, "cortex:", err)
				return 2
			}
		}
		ghDir := hostGHConfigDir()
		if err := validateNoControl(ghDir, "gh config dir"); err != nil {
			fmt.Fprintln(os.Stderr, "cortex:", err)
			return 2
		}
		opts := serviceOptions{host: hostVal, port: portVal, listen: listenVal, root: *root, data: *data, publicOrigin: *publicOrigin, trustProxy: *trustProxy, ghDir: ghDir}
		if err := m.install(opts, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "cortex:", err)
			return 1
		}
		return 0
	case "start", "stop", "restart":
		if err := m.action(cmd, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "cortex:", err)
			return 1
		}
		fmt.Fprintln(os.Stdout, serviceLifecycleSuccess(cmd))
		return 0
	case "status":
		if err := m.status(os.Stdout, version); err != nil {
			fmt.Fprintln(os.Stderr, "cortex:", err)
			return 1
		}
		return 0
	case "logs":
		if err := m.logs(*follow, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "cortex:", err)
			return 1
		}
		return 0
	case "uninstall":
		if err := m.uninstall(os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "cortex:", err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(os.Stderr, "cortex: unknown service command %q\n\nUsage: cortex service <install|start|stop|restart|status|logs|uninstall> [flags]\n", cmd)
		return 2
	}
}

func serviceLifecycleSuccess(verb string) string {
	words := map[string]string{
		"start": "started", "stop": "stopped", "restart": "restarted",
	}
	return "cortex.service " + words[verb] + "."
}
