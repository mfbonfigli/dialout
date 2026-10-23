package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mfbonfigli/dialout/internal/app"
	"github.com/mfbonfigli/dialout/internal/config"
	"github.com/mfbonfigli/dialout/internal/server"
)

// cmdServe runs the MCP server: over Streamable HTTP, or over stdio for a client that starts
// dialout itself. Stopping it (Ctrl-C, SIGTERM, or the client closing stdio) hangs up the call in
// progress first.
func cmdServe(args []string, stdio bool) int {
	name := "serve"
	if stdio {
		name = "mcp"
	}
	fl := flag.NewFlagSet(name, flag.ContinueOnError)
	cfgPath := fl.String("config", "", "configuration file (default: $DIALOUT_CONFIG, then the usual places)")
	noTunnel := fl.Bool("no-tunnel", false, "start no cloudflared: tunnel.hostname already reaches tunnel.listen")
	verbose := fl.Bool("v", false, "debug logs (including cloudflared's)")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	// With stdio, stdout carries the protocol: everything else goes to stderr.
	log := newLogger(*verbose, slog.LevelInfo)

	path, err := config.Find(*cfgPath)
	if err != nil {
		log.Error(err.Error())
		return 2
	}
	cfg, err := config.Load(path)
	if err == nil && !stdio {
		err = cfg.CheckServer()
	}
	if err != nil {
		log.Error("configuration", "err", err)
		return 2
	}
	a, err := app.Build(cfg, app.Options{NoTunnel: *noTunnel, Log: log})
	if err != nil {
		log.Error("start-up", "err", err)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	streamsStop, err := a.Serve()
	if err != nil {
		log.Error(err.Error())
		return 1
	}
	defer streamsStop()
	calls, err := server.New(ctx, a)
	if err != nil {
		log.Error("start-up", "err", err)
		return 1
	}
	defer calls.Wait() // after ctx is cancelled: the call in progress hangs up and is saved
	log.Info("dialout "+version, "config", path, "engine", cfg.Engine.Name, "stream_endpoint", cfg.Tunnel.Listen)

	if stdio {
		err := calls.MCP().Run(ctx, &mcp.StdioTransport{})
		stop()
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Error("mcp", "err", err)
			return 1
		}
		return 0
	}

	srv := &http.Server{Addr: cfg.Server.MCPListen, Handler: calls.HTTPHandler(cfg.Server.MCPToken), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("MCP over HTTP", "listen", cfg.Server.MCPListen, "path", "/mcp")
	select {
	case err := <-errc:
		log.Error("mcp", "err", err)
		return 1
	case <-ctx.Done():
	}
	stop()
	log.Info("shutting down: hanging up the call in progress, if any")
	sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	return 0
}

// cmdHealthcheck probes a running server's stream endpoint, for Docker's HEALTHCHECK: the image
// has no curl.
func cmdHealthcheck(args []string) int {
	fl := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	addr := fl.String("addr", "127.0.0.1:8090", "the stream endpoint (tunnel.listen)")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get("http://" + *addr + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		fmt.Fprintln(os.Stderr, "healthz:", resp.Status)
		return 1
	}
	return 0
}
