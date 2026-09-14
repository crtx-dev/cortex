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

func runSetup(args []string) int {
	fs := flag.NewFlagSet("cortex setup", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	data := fs.String("data", cortexDataDir(), "Cortex data directory")
	display := fs.String("display-name", "Administrator", "display name")
	username := fs.String("username", "admin", "login username")
	email := fs.String("email", "", "login email (optional)")
	passwordFile := fs.String("password-file", "", "file containing the password")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *passwordFile == "" {
		fmt.Fprintln(os.Stderr, "usage: cortex setup --password-file FILE [--username NAME] [--email EMAIL] [--display-name NAME] [--data DIR]")
		return 2
	}
	password, err := os.ReadFile(*passwordFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cortex:", err)
		return 1
	}
	if err = os.MkdirAll(*data, 0700); err == nil {
		err = app.SetupAdministrator(*data, *display, *username, *email, strings.TrimRight(string(password), "\r\n"))
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
	value := map[string]string{"project": "cortex", "dataDir": *data}
	if *jsonOutput {
		_ = json.NewEncoder(os.Stdout).Encode(value)
	} else {
		fmt.Printf("Data directory: %s\n", *data)
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
	if err := resetCortex(*data, *all); err != nil {
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
	for _, name := range []string{"users.json", "roles.json"} {
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
