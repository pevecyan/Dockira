// Command docker-manager runs the web UI and API for the compose stacks under apps_root.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"docker-manager/internal/api"
)

var version = "dev"

const usage = `docker-manager — Dockira, a self-hosted Docker Compose manager

  serve     [--config FILE]   run the API + UI (default config: /etc/docker-manager/config.yaml)
  password                    ask for a UI password and print its bcrypt hash
                              for password_hash; reads one line from stdin when not on a terminal
  gen-key                     print a new random secret_key
  openapi                     print the OpenAPI document (used by make gen-api)
  version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "serve":
		err = cmdServe(args)
	case "password":
		err = cmdPassword()
	case "gen-key":
		err = cmdGenKey()
	case "openapi":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		err = enc.Encode(api.Spec(version))
	case "version", "--version", "-v":
		fmt.Println(version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
