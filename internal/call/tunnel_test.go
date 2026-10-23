package call

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestHelperCloudflared is not a test: it is the fake cloudflared the tunnel tests start, as the
// test binary re-run with fake-cloudflared=<mode> among its arguments (cloudflared gets only an
// allowlisted environment, so the mode can't come as a variable).
func TestHelperCloudflared(t *testing.T) {
	mode := ""
	for _, a := range os.Args {
		if m, ok := strings.CutPrefix(a, "fake-cloudflared="); ok {
			mode = m
		}
	}
	if mode == "" {
		return
	}
	args := strings.Join(os.Args, " ")
	if os.Getenv("AWS_SECRET_ACCESS_KEY") != "" || os.Getenv("TWILIO_AUTH_TOKEN") != "" || os.Getenv("MY_SECRET") != "" {
		fmt.Fprintln(os.Stderr, "ERR credentials leaked into cloudflared's environment")
		os.Exit(3)
	}
	switch mode {
	case "quick":
		if !strings.Contains(args, "tunnel --no-autoupdate --url http://127.0.0.1:8090") {
			fmt.Fprintln(os.Stderr, "ERR unexpected arguments:", args)
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, "INF Requesting new quick Tunnel on trycloudflare.com...")
		fmt.Fprintln(os.Stderr, "INF |  https://brave-otter-lamp-ring.trycloudflare.com                          |")
		fmt.Fprintln(os.Stderr, "INF Registered tunnel connection connIndex=0 location=mxp01 protocol=quic")
	case "quick-nourl":
		fmt.Fprintln(os.Stderr, "INF Registered tunnel connection connIndex=0")
	case "named":
		if os.Getenv("TUNNEL_TOKEN") != "tok" || !strings.Contains(args, "tunnel --no-autoupdate run") {
			fmt.Fprintln(os.Stderr, "ERR no token or wrong arguments")
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, "INF Registered tunnel connection connIndex=0")
	}
	time.Sleep(time.Minute) // until the tunnel is stopped
	os.Exit(0)
}

func fakeCloudflared(t *testing.T, mode string) (binary string, prefix []string) {
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("TWILIO_AUTH_TOKEN", "secret")
	t.Setenv("MY_SECRET", "secret")
	return os.Args[0], []string{"-test.run=^TestHelperCloudflared$", "--", "fake-cloudflared=" + mode}
}

func TestQuickTunnel(t *testing.T) {
	bin, prefix := fakeCloudflared(t, "quick")
	var probed string
	q := &QuickTunnel{
		Binary: bin, Origin: "http://127.0.0.1:8090", ReadyTimeout: 10 * time.Second, prefix: prefix,
		probe: func(_ context.Context, host string) error { probed = host; return nil },
	}
	host, stop, err := q.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if host != "brave-otter-lamp-ring.trycloudflare.com" || probed != host {
		t.Errorf("host %q, probed %q", host, probed)
	}
}

func TestQuickTunnelFailures(t *testing.T) {
	bin, prefix := fakeCloudflared(t, "quick")
	q := &QuickTunnel{
		Binary: bin, Origin: "http://127.0.0.1:8090", ReadyTimeout: 10 * time.Second, prefix: prefix,
		probe: func(context.Context, string) error { return errors.New("HTTP 530") },
	}
	if _, _, err := q.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "isn't reachable") {
		t.Errorf("unreachable name: %v", err)
	}

	bin, prefix = fakeCloudflared(t, "quick-nourl")
	q = &QuickTunnel{Binary: bin, Origin: "http://127.0.0.1:8090", ReadyTimeout: 10 * time.Second, prefix: prefix,
		probe: func(context.Context, string) error { return nil }}
	if _, _, err := q.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "no trycloudflare.com address") {
		t.Errorf("no address: %v", err)
	}
	if _, _, err := (&QuickTunnel{}).Start(context.Background()); err == nil {
		t.Error("quick tunnel without an origin started")
	}
}

func TestNamedTunnel(t *testing.T) {
	bin, prefix := fakeCloudflared(t, "named")
	tun := &Tunnel{Binary: bin, Token: "tok", Host: "calls.example.test", ReadyTimeout: 10 * time.Second, prefix: prefix}
	host, stop, err := tun.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	stop()
	if host != "calls.example.test" {
		t.Errorf("host = %q", host)
	}
	if _, _, err := (&Tunnel{Token: "tok"}).Start(context.Background()); err == nil {
		t.Error("named tunnel without a hostname started")
	}
	if _, _, err := (NoTunnel{}).Start(context.Background()); err == nil {
		t.Error("NoTunnel without a hostname accepted")
	}
	if h, stop, err := (NoTunnel{Host: "x.example"}).Start(context.Background()); err != nil || h != "x.example" {
		t.Errorf("NoTunnel = %q, %v", h, err)
	} else {
		stop()
	}
}

func TestWaitHealthy(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(530) // Cloudflare: the tunnel isn't routed yet
			return
		}
		(&Streams{}).ServeHTTP(w, r)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := waitHealthy(ctx, srv.Client(), srv.URL+healthPath); err != nil || calls.Load() != 3 {
		t.Errorf("waitHealthy = %v after %d requests", err, calls.Load())
	}

	ctx, cancel = context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	if err := waitHealthy(ctx, srv.Client(), srv.URL+"/nope"); err == nil {
		t.Error("a 404 counted as healthy")
	}
}
