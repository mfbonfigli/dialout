package call

import (
	"log/slog"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/mfbonfigli/dialout/internal/twilio"
)

// Streams is the WebSocket endpoint Twilio connects to through the tunnel:
// wss://<host>/twilio/stream/<token>. It accepts a connection only for a token a call
// registered with Expect, unused and not expired, and only with a valid X-Twilio-Signature for that
// call's public URL. Everything else gets a 404 and is logged, except the health path, which
// answers 204 so a quick tunnel can be probed. The call itself then checks the CallSid and
// AccountSid of the start message.
type Streams struct {
	AuthToken string // the account's Auth Token, which signs Twilio's requests
	Log       *slog.Logger

	mu      sync.Mutex
	pending map[string]*Expected
}

var streamPathRE = regexp.MustCompile(`^/twilio/stream/([A-Za-z0-9_-]{43})$`)

// healthPath answers 204 No Content: QuickTunnel probes it to know its new name reaches the
// endpoint. A named tunnel's route doesn't forward it.
const healthPath = "/healthz"

// Expected is a stream a call is waiting for.
type Expected struct {
	s       *Streams
	token   string
	host    string    // the public hostname in the call's TwiML: Twilio signs that URL
	expires time.Time // guarded by s.mu
	conns   chan *Stream
	gone    chan struct{} // closed by Cancel
	once    sync.Once
}

// Stream is an accepted Media Streams connection. The call owns it until Close.
type Stream struct {
	WS   *websocket.Conn
	done chan struct{}
	once sync.Once
}

// Close closes the WebSocket and releases the HTTP handler that accepted it.
func (st *Stream) Close(code websocket.StatusCode, reason string) {
	st.once.Do(func() {
		_ = st.WS.Close(code, reason)
		close(st.done)
	})
}

// Expect registers a single-use token for a stream reaching host. The connection, if one comes
// before expires, is delivered on C. Call Cancel when the call no longer waits for it.
func (s *Streams) Expect(token, host string, expires time.Time) *Expected {
	e := &Expected{s: s, token: token, host: host, expires: expires, conns: make(chan *Stream), gone: make(chan struct{})}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending == nil {
		s.pending = map[string]*Expected{}
	}
	s.pending[token] = e
	return e
}

// C delivers the accepted connection.
func (e *Expected) C() <-chan *Stream { return e.conns }

// SetExpiry moves the deadline, e.g. to 60 s after the call is answered.
func (e *Expected) SetExpiry(t time.Time) {
	e.s.mu.Lock()
	defer e.s.mu.Unlock()
	e.expires = t
}

// Cancel withdraws the token. A connection accepted but not yet taken is closed.
func (e *Expected) Cancel() {
	e.once.Do(func() {
		e.s.mu.Lock()
		if e.s.pending[e.token] == e {
			delete(e.s.pending, e.token)
		}
		e.s.mu.Unlock()
		close(e.gone)
	})
}

// host returns the public hostname a token was registered for, without consuming it.
func (s *Streams) host(token string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.pending[token]
	if e == nil {
		return "", false
	}
	return e.host, true
}

// take consumes a token: it works once, and only before the token expires.
func (s *Streams) take(token string, now time.Time) *Expected {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.pending[token]
	if e == nil {
		return nil
	}
	delete(s.pending, token)
	if now.After(e.expires) {
		return nil
	}
	return e
}

func (s *Streams) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *Streams) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == healthPath {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	m := streamPathRE.FindStringSubmatch(r.URL.Path)
	if r.Method != http.MethodGet || m == nil {
		s.reject(w, r, "unexpected request")
		return
	}
	host, ok := s.host(m[1])
	if !ok {
		s.reject(w, r, "unknown or used token")
		return
	}
	// The signature is checked before the token is consumed, so a forged request can't burn it.
	url := "wss://" + host + r.URL.Path
	if !twilio.ValidSignature(s.AuthToken, url, nil, r.Header.Get("X-Twilio-Signature")) {
		s.reject(w, r, "invalid X-Twilio-Signature")
		return
	}
	e := s.take(m[1], time.Now())
	if e == nil {
		s.reject(w, r, "used or expired token")
		return
	}
	// Twilio isn't a browser: requests are authenticated by the token and the signature, not by
	// their Origin.
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		s.log().Warn("calls: stream handshake failed", "err", err)
		return
	}
	st := &Stream{WS: ws, done: make(chan struct{})}
	select {
	case e.conns <- st:
		<-st.done // the call owns the connection until it closes it
	case <-e.gone:
		st.Close(websocket.StatusGoingAway, "call over")
	}
}

func (s *Streams) reject(w http.ResponseWriter, r *http.Request, why string) {
	s.log().Warn("calls: stream request refused", "why", why, "method", r.Method,
		"path", redactToken(r.URL.Path), "client", clientAddr(r))
	http.NotFound(w, r)
}

// redactToken keeps a token out of the logs: the first 6 characters are enough to correlate.
func redactToken(path string) string {
	if m := streamPathRE.FindStringSubmatch(path); m != nil {
		return "/twilio/stream/" + m[1][:6] + "…"
	}
	if len(path) > 64 {
		return path[:64] + "…"
	}
	return path
}

func clientAddr(r *http.Request) string {
	if ip := r.Header.Get("CF-Connecting-IP"); ip != "" { // set by Cloudflare's edge
		return ip
	}
	return r.RemoteAddr
}
