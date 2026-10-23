// Command dialout places outbound phone calls with a voice agent, and serves them to AI agents
// over MCP.
//
//	dialout call -to <number or label> -objective "…"   place one call and follow it in the terminal
//	dialout serve                                       MCP over Streamable HTTP, with a bearer token
//	dialout mcp                                         MCP over stdio, for a client that starts it
//	dialout healthcheck                                 probe a running server (for Docker)
package main

import (
	"fmt"
	"log/slog"
	"os"
	_ "time/tzdata" // the timezone setting works without the system's zone database

	"github.com/mfbonfigli/dialout/internal/server"
)

// version is set at build time: -ldflags "-X main.version=…".
var version = "dev"

func main() {
	server.Version = version
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	args := os.Args[2:]
	switch os.Args[1] {
	case "call":
		os.Exit(cmdCall(args))
	case "serve":
		os.Exit(cmdServe(args, false))
	case "mcp":
		os.Exit(cmdServe(args, true))
	case "healthcheck":
		os.Exit(cmdHealthcheck(args))
	case "version", "-version", "--version":
		fmt.Println("dialout", version)
	case "-h", "-help", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "dialout: unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `Usage:
  dialout call -to <number or label> -objective "…" [flags]
      Place one call from this machine and follow it in the terminal.
  dialout serve [-config file]
      Serve MCP over Streamable HTTP at server.mcp_listen, path /mcp, with server.mcp_token.
  dialout mcp [-config file]
      Serve MCP over stdio, for an MCP client that starts dialout itself.
  dialout healthcheck [-addr 127.0.0.1:8090]
      Exit 0 if a running server's stream endpoint answers.
  dialout version

The configuration is dialout.yaml: -config, else $DIALOUT_CONFIG, else dialout/dialout.yaml in
the user's configuration folder, else /etc/dialout/dialout.yaml. Run "dialout <command> -h" for
a command's flags.
`)
}

func newLogger(verbose bool, level slog.Level) *slog.Logger {
	if verbose {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}
