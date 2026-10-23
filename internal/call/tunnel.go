package call

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Starter brings up the public ingress Twilio's media stream reaches, for the length of one call,
// and returns its hostname: Tunnel (your domain), QuickTunnel (a random trycloudflare.com name) or
// NoTunnel (an ingress that already exists).
type Starter interface {
	Start(ctx context.Context) (host string, stop func(), err error)
}

// Tunnel runs a named, remotely managed Cloudflare tunnel for the length of a call.
// Its route (Host → the stream endpoint) lives in Cloudflare, so the process needs only the token,
// which goes through the environment and never shows in the process list.
type Tunnel struct {
	Binary       string // cloudflared; found with FindCloudflared when empty
	Token        string
	Host         string        // the hostname the tunnel's route serves, e.g. calls.example.com
	ReadyTimeout time.Duration // 30 s when 0
	Log          *slog.Logger

	prefix []string // tests: arguments before cloudflared's
}

func (t *Tunnel) Start(ctx context.Context) (string, func(), error) {
	if t.Token == "" || t.Host == "" {
		return "", nil, errors.New("tunnel: a named tunnel needs its token and hostname")
	}
	args := append(append([]string{}, t.prefix...), "tunnel", "--no-autoupdate", "run")
	stop, err := runCloudflared(ctx, t.Binary, args, []string{"TUNNEL_TOKEN=" + t.Token}, t.ReadyTimeout, t.Log, nil)
	if err != nil {
		return "", nil, err
	}
	return t.Host, stop, nil
}

// QuickTunnel runs a Cloudflare quick tunnel for the length of a call: a random
// <words>.trycloudflare.com name, with no account, domain or token. Cloudflare offers quick tunnels
// for testing and development, without an uptime guarantee; the name changes on every call, which
// is harmless because the TwiML is written per call.
type QuickTunnel struct {
	Binary       string        // cloudflared; found with FindCloudflared when empty
	Origin       string        // the stream endpoint, e.g. http://127.0.0.1:8090
	ReadyTimeout time.Duration // 30 s when 0
	Log          *slog.Logger

	prefix []string                                     // tests: arguments before cloudflared's
	probe  func(ctx context.Context, host string) error // tests: replaces probeHealth
}

var quickURLRE = regexp.MustCompile(`https://([a-z0-9-]+\.trycloudflare\.com)`)

func (q *QuickTunnel) Start(ctx context.Context) (string, func(), error) {
	if q.Origin == "" {
		return "", nil, errors.New("tunnel: a quick tunnel needs the stream endpoint's address")
	}
	var mu sync.Mutex
	host := ""
	args := append(append([]string{}, q.prefix...), "tunnel", "--no-autoupdate", "--url", q.Origin)
	stop, err := runCloudflared(ctx, q.Binary, args, nil, q.ReadyTimeout, q.Log, func(line string) {
		if m := quickURLRE.FindStringSubmatch(line); m != nil {
			mu.Lock()
			if host == "" {
				host = m[1]
			}
			mu.Unlock()
		}
	})
	if err != nil {
		return "", nil, err
	}
	mu.Lock()
	h := host
	mu.Unlock()
	if h == "" {
		stop()
		return "", nil, errors.New("tunnel: cloudflared printed no trycloudflare.com address")
	}
	// The connection is registered; make sure the new name reaches the stream endpoint before
	// anyone is rung.
	probe := q.probe
	if probe == nil {
		probe = probeHealth
	}
	timeout := q.ReadyTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := probe(pctx, h); err != nil {
		stop()
		return "", nil, fmt.Errorf("tunnel: %s isn't reachable: %w", h, err)
	}
	return h, stop, nil
}

// probeHealth asks the stream endpoint's health path through the tunnel until it answers.
func probeHealth(ctx context.Context, host string) error {
	return waitHealthy(ctx, &http.Client{Timeout: 5 * time.Second}, "https://"+host+healthPath)
}

func waitHealthy(ctx context.Context, client *http.Client, url string) error {
	var last error
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusNoContent {
				return nil
			}
			last = fmt.Errorf("HTTP %d", resp.StatusCode)
		} else {
			last = err
		}
		select {
		case <-ctx.Done():
			return last
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// NoTunnel is an ingress that already exists (another cloudflared, a reverse proxy): nothing to
// start.
type NoTunnel struct {
	Host string
}

func (n NoTunnel) Start(context.Context) (string, func(), error) {
	if n.Host == "" {
		return "", nil, errors.New("tunnel: no hostname for the existing ingress")
	}
	return n.Host, func() {}, nil
}

// FindCloudflared returns the cloudflared to run: the configured path if any, else the one in
// PATH, else on Windows the installer's default location (a terminal opened before the install
// doesn't have it in PATH yet).
func FindCloudflared(configured string) (string, error) {
	if configured != "" {
		if _, err := os.Stat(configured); err != nil {
			return "", fmt.Errorf("tunnel: %w", err)
		}
		return configured, nil
	}
	if p, err := exec.LookPath("cloudflared"); err == nil {
		return p, nil
	}
	if runtime.GOOS == "windows" {
		for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles"} {
			if dir := os.Getenv(env); dir != "" {
				p := filepath.Join(dir, "cloudflared", "cloudflared.exe")
				if _, err := os.Stat(p); err == nil {
					return p, nil
				}
			}
		}
	}
	return "", errors.New("tunnel: cloudflared not found in PATH (install it or set its path)")
}

// runCloudflared starts cloudflared and returns once it has registered a connection with
// Cloudflare's edge. Each line it logs goes to onLine (may be nil). stop ends it.
func runCloudflared(ctx context.Context, binary string, args, env []string, timeout time.Duration,
	log *slog.Logger, onLine func(string)) (stop func(), err error) {
	bin, err := FindCloudflared(binary)
	if err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	cmd := exec.Command(bin, args...)
	cmd.Env = append(childEnv(os.Environ()), env...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("tunnel: %w", err)
	}

	ready := make(chan struct{})
	var readyOnce sync.Once
	var tail []string // the last lines, for the error if it never connects
	var tailMu sync.Mutex
	exited := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			line := sc.Text()
			log.Debug("cloudflared", "line", line)
			if onLine != nil {
				onLine(line)
			}
			tailMu.Lock()
			tail = append(tail, line)
			if len(tail) > 5 {
				tail = tail[1:]
			}
			tailMu.Unlock()
			if strings.Contains(line, "Registered tunnel connection") {
				readyOnce.Do(func() { close(ready) })
			}
		}
		exited <- cmd.Wait()
	}()

	stop = func() {
		// SIGTERM lets cloudflared unregister cleanly; Windows only has Kill.
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			_ = cmd.Process.Kill()
		}
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-exited
		}
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ready:
		log.Info("calls: tunnel connected")
		return stop, nil
	case err := <-exited:
		tailMu.Lock()
		defer tailMu.Unlock()
		return nil, fmt.Errorf("tunnel: cloudflared exited (%v): %s", err, strings.Join(tail, " | "))
	case <-timer.C:
		stop()
		return nil, fmt.Errorf("tunnel: no connection to Cloudflare within %v", timeout)
	case <-ctx.Done():
		stop()
		return nil, ctx.Err()
	}
}

// childEnv is the environment for cloudflared: only the parent's variables a program needs to run
// and reach the network. Secrets can come under any name (the configuration's ${VAR}), so the
// list says what passes rather than what doesn't.
func childEnv(env []string) []string {
	var out []string
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if childVars[strings.ToUpper(k)] {
			out = append(out, kv)
		}
	}
	return out
}

var childVars = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "TZ": true, "LANG": true, "LC_ALL": true,
	"TMPDIR": true, "TEMP": true, "TMP": true, "SSL_CERT_FILE": true, "SSL_CERT_DIR": true,
	"HTTPS_PROXY": true, "HTTP_PROXY": true, "NO_PROXY": true,
	// Windows
	"SYSTEMROOT": true, "WINDIR": true, "USERPROFILE": true, "APPDATA": true, "LOCALAPPDATA": true,
	"PROGRAMDATA": true, "COMSPEC": true, "PATHEXT": true,
}
