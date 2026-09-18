package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/crtx-dev/cortex/internal/app"
)

func cortexDataDir() string {
	if value := os.Getenv("CORTEX_DATA_DIR"); value != "" {
		return value
	}
	if value, err := os.UserConfigDir(); err == nil {
		return filepath.Join(value, "cortex")
	}
	return ".cortex"
}

// resolveCortexDataDir applies the canonical instance-resolution precedence
// shared by setup/config/reset: an explicit --data wins, then CORTEX_DATA_DIR,
// then the data directory recorded by the installed managed service, then the
// normal default. It fails closed rather than silently targeting a different
// directory when the installed unit exists but cannot be used safely.
func resolveCortexDataDir(fs *flag.FlagSet, explicit string) (string, error) {
	dir := strings.TrimSpace(explicit)
	if !flagProvided(fs, "data") && strings.TrimSpace(os.Getenv("CORTEX_DATA_DIR")) == "" {
		installed, installedOK, installedErr := InstalledDataDir()
		if installedErr != nil {
			return "", installedErr
		}
		if installedOK {
			dir = installed
		}
	}
	return dir, nil
}

func runSetup(args []string) int {
	fs := flag.NewFlagSet("cortex setup", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	data := fs.String("data", cortexDataDir(), "Cortex data directory")
	display := fs.String("display-name", "", "deprecated; ignored (display is derived from username)")
	username := fs.String("username", "", "login username")
	email := fs.String("email", "", "login email")
	passwordFile := fs.String("password-file", "", "file containing the password")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *passwordFile == "" || *username == "" {
		fmt.Fprintln(os.Stderr, "usage: cortex setup --username NAME --email EMAIL --password-file FILE [--data DIR]")
		return 2
	}
	password, err := os.ReadFile(*passwordFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cortex:", err)
		return 1
	}
	if strings.TrimSpace(*email) == "" {
		fmt.Fprintln(os.Stderr, "cortex: --email is required")
		return 2
	}
	dataDir, err := resolveCortexDataDir(fs, *data)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cortex:", err)
		return 1
	}
	displayName := *username
	if strings.TrimSpace(*display) != "" {
		displayName = *display
	}
	if err = os.MkdirAll(dataDir, 0700); err == nil {
		err = app.SetupAdministrator(dataDir, displayName, *username, *email, strings.TrimRight(string(password), "\r\n"))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "cortex:", err)
		return 1
	}
	fmt.Println("Cortex administrator configured.")
	return 0
}

func runConfig(args []string) int {
	fs := flag.NewFlagSet("cortex config", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	data := fs.String("data", cortexDataDir(), "Cortex data directory")
	jsonOutput := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 || fs.Arg(0) != "show" {
		fmt.Fprintln(os.Stderr, "usage: cortex config show [--data DIR] [--json]")
		return 2
	}
	dataDir, err := resolveCortexDataDir(fs, *data)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cortex:", err)
		return 1
	}
	value := map[string]string{"project": "cortex", "dataDir": dataDir}
	if *jsonOutput {
		_ = json.NewEncoder(os.Stdout).Encode(value)
	} else {
		fmt.Printf("Data directory: %s\n", dataDir)
	}
	return 0
}

func runReset(args []string) int {
	fs := flag.NewFlagSet("cortex reset", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	auth := fs.Bool("auth", false, "reset accounts and sessions only")
	all := fs.Bool("all", false, "reset all Cortex state (workspace files are preserved)")
	data := fs.String("data", cortexDataDir(), "Cortex data directory")
	confirm := fs.String("confirm", "", "non-interactive confirmation")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || (*auth == *all) {
		fmt.Fprintln(os.Stderr, "usage: cortex reset (--auth|--all) [--data DIR] [--confirm 'CORTEX AUTH|CORTEX ALL']")
		return 2
	}
	mode := "AUTH"
	if *all {
		mode = "ALL"
	}
	want := "CORTEX " + mode
	if !confirmReset(want, *confirm) {
		fmt.Fprintln(os.Stderr, "cortex: confirmation did not match; nothing changed")
		return 1
	}
	dataDir, err := resolveCortexDataDir(fs, *data)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cortex:", err)
		return 1
	}
	if err := resetCortex(dataDir, *all); err != nil {
		fmt.Fprintln(os.Stderr, "cortex:", err)
		return 1
	}
	fmt.Printf("Cortex %s reset complete. A timestamped backup was retained.\n", strings.ToLower(mode))
	return 0
}

func confirmReset(want, supplied string) bool {
	if supplied != "" {
		return supplied == want
	}
	fmt.Fprintf(os.Stderr, "Type %q to continue: ", want)
	got, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(got) == want
}

func resetCortex(dataDir string, all bool) error {
	stamp := time.Now().UTC().Format("20060102T150405Z")
	backup := filepath.Join(dataDir, "reset-backups", stamp)
	if all {
		if _, err := os.Stat(dataDir); os.IsNotExist(err) {
			return nil
		}
		backup = dataDir + ".reset-" + stamp
		if err := os.Rename(dataDir, backup); err != nil {
			return fmt.Errorf("back up data directory: %w", err)
		}
		return os.MkdirAll(dataDir, 0700)
	}
	if err := os.MkdirAll(backup, 0700); err != nil {
		return err
	}
	// Authentication reset clears only account state. roles.json holds role
	// definitions and must be preserved: it is configuration, not
	// authentication state, and deleting it would silently drop custom roles.
	// Cortex sessions are in-memory only, so no session file needs clearing.
	for _, name := range []string{"users.json"} {
		source := filepath.Join(dataDir, name)
		if _, err := os.Stat(source); os.IsNotExist(err) {
			continue
		}
		if err := os.Rename(source, filepath.Join(backup, name)); err != nil {
			return err
		}
	}
	return nil
}
