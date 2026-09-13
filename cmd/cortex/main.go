package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/crtx-dev/cortex/internal/app"
)

var version = "0.1.1"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version":
			if len(os.Args) != 2 {
				fmt.Fprintln(os.Stderr, "cortex:", os.Args[1], "takes no arguments")
				os.Exit(2)
			}
			fmt.Fprintln(os.Stdout, version)
			return
		case "service":
			os.Exit(runService(os.Args[2:], version))
		case "reset":
			os.Exit(runReset(os.Args[2:]))
		case "config":
			os.Exit(runConfig(os.Args[2:]))
		case "setup":
			os.Exit(runSetup(os.Args[2:]))
		case "serve":
			os.Args = append(os.Args[:1], os.Args[2:]...)
		default:
			if os.Args[1][0] != '-' {
				fmt.Fprintln(os.Stderr, "cortex: unknown command", os.Args[1])
				os.Exit(2)
			}
		}
	}
	host := flag.String("host", "", "HTTP bind host (default 127.0.0.1; CORTEX_HOST overrides, CLI wins)")
	port := flag.String("port", "", "HTTP bind port, 1-65535 (default 7331; CORTEX_PORT overrides, CLI wins)")
	listen := flag.String("listen", "", "HTTP listen address (legacy; alternative to --host/--port)")
	root := flag.String("root", "", "workspace root (default: home directory)")
	data := flag.String("data", "", "Cortex data directory")
	trustProxy := flag.Bool("trust-proxy", false, "trust forwarding headers from a direct loopback reverse proxy")
	publicOrigin := flag.String("public-origin", "", "canonical external origin, for example https://cortex.example.com")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "cortex: unexpected arguments:", flag.Args())
		os.Exit(2)
	}
	addr, err := resolveListener(*host, *port, *listen, flagProvided(flag.CommandLine, "host"), flagProvided(flag.CommandLine, "port"), flagProvided(flag.CommandLine, "listen"))
	if err != nil {
		log.Fatal("cortex: " + err.Error())
	}
	cwd, err := os.Getwd()
	if err != nil {
		log.Fatal(err)
	}
	if *root == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			*root = home
		} else {
			*root = cwd
		}
	}
	if *data == "" {
		if d, err := os.UserConfigDir(); err == nil {
			*data = filepath.Join(d, "cortex")
		} else {
			*data = filepath.Join(cwd, ".cortex")
		}
	}
	srv, err := app.New(app.Options{Listen: addr, Root: *root, DataDir: *data, TrustProxy: *trustProxy, PublicOrigin: *publicOrigin})
	if err != nil {
		log.Fatal(err)
	}
	defer srv.Close()
	fmt.Printf("Cortex · http://%s\nWorkspace root · %s\n", addr, srv.Root())
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("cortex: %v (listener: %s)", err, addr)
	}
}
