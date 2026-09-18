package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// useInstalledResetDir points the managed unit at dir so setup/config/reset
// resolve the installed service's data directory instead of a CLI override or
// the default.
func useInstalledResetDir(t *testing.T, dir string) {
	t.Helper()
	oldPath := userUnitPath
	unit := filepath.Join(t.TempDir(), "cortex.service")
	content := buildCortexUnit("/usr/local/bin/cortex", serviceOptions{listen: "127.0.0.1:7331", data: dir, root: "/"})
	if err := os.WriteFile(unit, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	userUnitPath = func(string) string { return unit }
	t.Cleanup(func() { userUnitPath = oldPath })
	t.Setenv("CORTEX_DATA_DIR", "")
}

func TestRunResetAuthUsesInstalledServiceData(t *testing.T) {
	installed := t.TempDir()
	for _, name := range []string{"users.json", "roles.json"} {
		if err := os.WriteFile(filepath.Join(installed, name), []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	useInstalledResetDir(t, installed)

	if code := runReset([]string{"--auth", "--confirm", "CORTEX AUTH"}); code != 0 {
		t.Fatalf("reset --auth exit = %d, want 0", code)
	}
	if _, err := os.Stat(filepath.Join(installed, "users.json")); !os.IsNotExist(err) {
		t.Fatalf("installed users.json survived auth reset: %v", err)
	}
	if _, err := os.Stat(filepath.Join(installed, "roles.json")); err != nil {
		t.Fatalf("roles.json removed by auth reset: %v", err)
	}
	backups, err := filepath.Glob(filepath.Join(installed, "reset-backups", "*", "users.json"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("auth backups = %v, err = %v; want one", backups, err)
	}
}

func TestRunResetAllUsesInstalledServiceData(t *testing.T) {
	installed := t.TempDir()
	if err := os.WriteFile(filepath.Join(installed, "marker"), []byte("state"), 0600); err != nil {
		t.Fatal(err)
	}
	useInstalledResetDir(t, installed)

	if code := runReset([]string{"--all", "--confirm", "CORTEX ALL"}); code != 0 {
		t.Fatalf("reset --all exit = %d, want 0", code)
	}
	if _, err := os.Stat(filepath.Join(installed, "marker")); !os.IsNotExist(err) {
		t.Fatalf("recreated data directory retained old marker: %v", err)
	}
	backups, err := filepath.Glob(installed + ".reset-*")
	if err != nil || len(backups) != 1 {
		t.Fatalf("full backups = %v, err = %v; want one", backups, err)
	}
	if got, err := os.ReadFile(filepath.Join(backups[0], "marker")); err != nil || string(got) != "state" {
		t.Fatalf("backup marker = %q, err = %v", got, err)
	}
}

// TestRunResetFailsClosedOnLegacyUnit verifies a managed unit that predates
// the data-dir marker cannot silently fall back to a different directory.
func TestRunResetFailsClosedOnLegacyUnit(t *testing.T) {
	oldPath := userUnitPath
	unit := filepath.Join(t.TempDir(), "cortex.service")
	body := "# cortex-listen: 127.0.0.1:7331\n# cortex-health: /api/health\n" + renderCortexUnitBody("/usr/local/bin/cortex", serviceOptions{listen: "127.0.0.1:7331", data: "/var/lib/cortex", root: "/"})
	sum := sha256.Sum256([]byte(body))
	content := cortexUnitMarker + "\n" + cortexManagedPrefix + "v1 sha256=" + hex.EncodeToString(sum[:]) + "\n" + body
	if err := os.WriteFile(unit, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	userUnitPath = func(string) string { return unit }
	t.Cleanup(func() { userUnitPath = oldPath })
	t.Setenv("CORTEX_DATA_DIR", "")

	if code := runReset([]string{"--auth", "--confirm", "CORTEX AUTH"}); code != 1 {
		t.Fatalf("reset exit = %d, want 1 (legacy unit must fail closed)", code)
	}
	if _, err := os.Stat(filepath.Join("/var/lib/cortex")); !os.IsNotExist(err) {
		t.Fatalf("reset must not operate on an inferred directory; /var/lib/cortex exists: %v", err)
	}
}

func TestResetCortexAuthPreservesRoles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"users.json", "roles.json", "settings.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	if err := resetCortex(dir, false); err != nil {
		t.Fatalf("auth reset: %v", err)
	}

	// Role definitions are configuration, not authentication state: they must
	// survive an auth-only reset.
	if _, err := os.Stat(filepath.Join(dir, "roles.json")); err != nil {
		t.Fatalf("auth reset removed roles.json: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "settings.json")); err != nil {
		t.Fatalf("auth reset removed settings.json: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "users.json")); !os.IsNotExist(err) {
		t.Fatalf("auth reset left users.json: %v", err)
	}
	backups, err := filepath.Glob(filepath.Join(dir, "reset-backups", "*", "users.json"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("users.json backups = %v, err = %v; want one", backups, err)
	}
}

func TestResetCortexAllMovesWholeDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marker"), []byte("state"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := resetCortex(dir, true); err != nil {
		t.Fatalf("all reset: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "marker")); !os.IsNotExist(err) {
		t.Fatalf("recreated directory retained old marker: %v", err)
	}
	backups, err := filepath.Glob(dir + ".reset-*")
	if err != nil || len(backups) != 1 {
		t.Fatalf("backups = %v, err = %v; want one", backups, err)
	}
	if got, err := os.ReadFile(filepath.Join(backups[0], "marker")); err != nil || string(got) != "state" {
		t.Fatalf("backup marker = %q, err = %v", got, err)
	}
}
