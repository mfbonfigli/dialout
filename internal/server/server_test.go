package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mfbonfigli/dialout/internal/app"
	"github.com/mfbonfigli/dialout/internal/call"
	"github.com/mfbonfigli/dialout/internal/config"
	"github.com/mfbonfigli/dialout/internal/voice"
	"github.com/mfbonfigli/dialout/prompts"
)

const testConfig = `
owner: Marco
twilio: {account_sid: AC1, api_key_sid: SK1, api_key_secret: s, auth_token: t, from: "+353850000000"}
policy:
  enabled: true
  allowlist:
    - {number: "+39 333 000 0000", label: Marco (mobile), name: Marco, kind: personal}
    - {number: "02 1234 5678", label: Pizzeria}
  limits: {per_hour: 2}
`

// engine is a voice engine that only prepares: the calls themselves are faked.
type engine struct{ voices map[string]bool }

func (engine) Name() string { return "fake" }
func (e engine) Prepare(c voice.Config) (voice.Config, error) {
	if c.Voice != "" && !e.voices[c.Voice] {
		return c, &voiceErr{c.Voice}
	}
	return c.Render(prompts.WithGoodbye, true)
}
func (engine) Dial(context.Context, voice.Config) (voice.Session, error) {
	panic("no dialling in tests")
}

type voiceErr struct{ v string }

func (e *voiceErr) Error() string { return "unknown voice " + e.v }

type harness struct {
	t      *testing.T
	s      *Server
	client *mcp.ClientSession
	cancel context.CancelFunc

	mu      sync.Mutex
	params  []call.Params
	configs []call.Config
	release chan struct{} // closed: the fake calls end
}

func newHarness(t *testing.T) *harness { return newHarnessWith(t, testConfig) }

func newHarnessWith(t *testing.T, text string) *harness {
	t.Helper()
	cfg, err := config.Parse([]byte(text), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	idx, err := OpenIndex(filepath.Join(t.TempDir(), "calls.json"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{t: t, cancel: cancel, release: make(chan struct{})}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	h.s = &Server{
		App:   &app.App{Config: cfg, Engine: engine{voices: map[string]bool{"beatrice": true, "lorenzo": true}}, Log: quiet},
		Index: idx,
		Load:  func() (*config.Config, error) { return cfg, nil },
		Run:   h.run,
		Log:   quiet,
		ctx:   ctx,
	}
	ct, st := mcp.NewInMemoryTransports()
	if _, err := h.s.MCP().Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	h.client, err = mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		h.client.Close()
		cancel()
		h.s.Wait()
	})
	return h
}

// run is the fake call: it reports progress, waits for release or a hang-up, and succeeds.
func (h *harness) run(ctx context.Context, cfg call.Config, p call.Params) *call.Result {
	h.mu.Lock()
	h.params, h.configs = append(h.params, p), append(h.configs, cfg)
	h.mu.Unlock()
	cfg.Observe(call.Event{Kind: "status", Text: "ringing"})
	cfg.Observe(call.Event{Kind: "status", Text: "answered"})
	cfg.Observe(call.Event{Kind: "callee", Text: "Pronto?"})
	select {
	case <-h.release:
		return &call.Result{CallID: p.CallID, Status: "completed", Answered: true, Talk: 42 * time.Second, EndReason: "end_call",
			Outcome:    &voice.Outcome{Result: "success", Summary: "Arriva alle 20."},
			Transcript: []call.Line{{T: 1, Speaker: "callee", Text: "Pronto?"}, {T: 2, Speaker: "agent", Text: "Ciao!"}}}
	case <-ctx.Done():
		return &call.Result{CallID: p.CallID, Status: "completed", Answered: true, EndReason: "hung_up_on_request"}
	}
}

// tool calls a tool and decodes its structured result; it fails the test on a tool error unless
// wantErr, and then returns the error text.
func (h *harness) tool(name string, args map[string]any, out any, wantErr bool) string {
	h.t.Helper()
	res, err := h.client.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		h.t.Fatalf("%s: %v", name, err)
	}
	if res.IsError != wantErr {
		h.t.Fatalf("%s: error %v, want %v: %v", name, res.IsError, wantErr, text(res))
	}
	if res.IsError {
		return text(res)
	}
	b, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(b, out); err != nil {
		h.t.Fatalf("%s: %v (%s)", name, err, b)
	}
	return ""
}

func text(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, " ")
}

func TestPlaceAndGetCall(t *testing.T) {
	h := newHarness(t)
	var placed placeCallOut
	h.tool("place_call", map[string]any{"to": "Marco (mobile)", "objective": "Chiedi a che ora arriva."}, &placed, false)
	if placed.Status != "queued" || placed.CallID == "" {
		t.Fatalf("placed = %+v", placed)
	}

	// While it runs: in progress, with the transcript so far.
	var rec Record
	h.tool("get_call", map[string]any{"call_id": placed.CallID}, &rec, false)
	if rec.Status != "in-progress" || !rec.Answered || len(rec.Transcript) != 1 || rec.Callee != "Marco" || rec.Kind != "personal" {
		t.Errorf("running call = %+v", rec)
	}
	// A second call is refused while the first runs.
	if msg := h.tool("place_call", map[string]any{"to": "Pizzeria", "objective": "x"}, nil, true); !strings.Contains(msg, "call_in_progress") {
		t.Errorf("second call: %s", msg)
	}

	// get_call waits for the end.
	go func() { time.Sleep(100 * time.Millisecond); close(h.release) }()
	h.tool("get_call", map[string]any{"call_id": placed.CallID, "wait_seconds": 10}, &rec, false)
	if rec.Status != "completed" || rec.DurationS != 42 || rec.Outcome == nil || rec.Outcome.Result != "success" ||
		len(rec.Transcript) != 2 || rec.EndReason != "end_call" || rec.Ended == nil {
		t.Errorf("finished call = %+v", rec)
	}

	// The call got the policy's decision and the allowlist entry's defaults.
	h.mu.Lock()
	p, c := h.params[0], h.configs[0]
	h.mu.Unlock()
	if p.To != "+393330000000" || p.Label != "Marco (mobile)" || p.Brief.Callee != "Marco" || p.Brief.Kind != "personal" ||
		p.Brief.Label != "Marco (mobile)" || p.MaxDuration != 5*time.Minute || p.RingTimeout != 25*time.Second ||
		c.Owner != "Marco" || c.Prompts == nil {
		t.Errorf("params = %+v, config owner %q", p, c.Owner)
	}

	// The history survives a restart.
	idx, err := OpenIndex(h.s.Index.path)
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := idx.Get(placed.CallID); !ok || r.Status != "completed" || len(r.Transcript) != 2 {
		t.Errorf("reopened record = %+v", r)
	}
}

func TestRefusals(t *testing.T) {
	h := newHarness(t)
	for _, c := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"to": "+39 333 999 9999", "objective": "x"}, "not_allowlisted"},
		{map[string]any{"to": "112", "objective": "x"}, "invalid_number"},
		{map[string]any{"to": "Pizzeria", "objective": " "}, "invalid_objective"},
		{map[string]any{"to": "Pizzeria", "objective": "x", "kind": "friend"}, "invalid_kind"},
		{map[string]any{"to": "Pizzeria", "objective": "x", "max_minutes": 9}, "invalid_duration"},
		{map[string]any{"to": "Pizzeria", "objective": "x", "voice": "matthew"}, "voice"}, // not the caller's choice
	} {
		if msg := h.tool("place_call", c.args, nil, true); !strings.Contains(msg, c.want) {
			t.Errorf("%v: %s, want %s", c.args, msg, c.want)
		}
	}
	if msg := h.tool("get_call", map[string]any{"call_id": "nope"}, nil, true); !strings.Contains(msg, "no call") {
		t.Errorf("get_call of an unknown call: %s", msg)
	}
	// Nothing was placed.
	if len(h.params) != 0 {
		t.Errorf("calls placed: %+v", h.params)
	}
}

func TestHangUpAndLimits(t *testing.T) {
	h := newHarness(t)
	var placed placeCallOut
	h.tool("place_call", map[string]any{"to": "Pizzeria", "objective": "x", "callee": "la pizzeria Da Mario", "kind": "business"}, &placed, false)
	var hung hangUpOut
	h.tool("hang_up", map[string]any{"call_id": placed.CallID}, &hung, false)
	if hung.Status != "completed" {
		t.Errorf("hang_up = %+v", hung)
	}
	var rec Record
	h.tool("get_call", map[string]any{"call_id": placed.CallID}, &rec, false)
	if rec.EndReason != "hung_up_on_request" || rec.Callee != "la pizzeria Da Mario" || rec.Kind != "business" {
		t.Errorf("hung-up call = %+v", rec)
	}
	if h.configs[0].Voice != "beatrice" { // the configured one
		t.Errorf("voice = %q", h.configs[0].Voice)
	}

	var list listNumbersOut
	h.tool("list_numbers", nil, &list, false)
	if !list.Enabled || len(list.Numbers) != 2 || list.Numbers[1].Number != "+390212345678" || list.CallsLeftHour == nil ||
		*list.CallsLeftHour != 1 || list.CallsLeftDay == nil || *list.CallsLeftDay != 19 || list.CallInProgress {
		t.Errorf("list_numbers = %+v", list)
	}

	// The hourly limit is 2.
	close(h.release)
	h.tool("place_call", map[string]any{"to": "Pizzeria", "objective": "x"}, &placed, false)
	h.tool("get_call", map[string]any{"call_id": placed.CallID, "wait_seconds": 10}, &rec, false)
	if msg := h.tool("place_call", map[string]any{"to": "Pizzeria", "objective": "x"}, nil, true); !strings.Contains(msg, "hourly_limit") {
		t.Errorf("third call: %s", msg)
	}
}

func TestInterruptedOnRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "calls.json")
	idx, _ := OpenIndex(path)
	idx.Add(Record{CallID: "a", Status: "in-progress", Created: time.Now()})
	idx.Add(Record{CallID: "b", Status: "completed", Created: time.Now()})
	idx, err := OpenIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := idx.Get("a"); a.Status != "interrupted" {
		t.Errorf("a = %+v", a)
	}
	if b, _ := idx.Get("b"); b.Status != "completed" {
		t.Errorf("b = %+v", b)
	}
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func TestHTTP(t *testing.T) {
	h := newHarness(t)
	token := strings.Repeat("t", 32)
	srv := httptest.NewServer(h.s.HTTPHandler(token))
	defer srv.Close()

	for _, auth := range []string{"", "Bearer wrong", "Bearer " + token + "x", token} {
		req, _ := http.NewRequest("POST", srv.URL+"/mcp", strings.NewReader(`{}`))
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") == "" {
			t.Errorf("auth %q: %d", auth, resp.StatusCode)
		}
	}

	ctx := context.Background()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: srv.URL + "/mcp", HTTPClient: &http.Client{Transport: bearer{token}}, DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range tools.Tools {
		names = append(names, tl.Name)
	}
	if strings.Join(names, ",") != "get_call,hang_up,list_numbers,place_call" {
		t.Errorf("tools = %v", names)
	}
	if RequireToken("", http.NotFoundHandler()) == nil {
		t.Error("nil handler")
	}
}

func TestOpenMode(t *testing.T) {
	open := strings.Replace(testConfig, "  enabled: true\n", "  enabled: true\n  mode: open\n  countries: [IT, IE]\n  hours: \"\"\n", 1)
	open = strings.Replace(open, "limits: {per_hour: 2}", "limits: {per_hour: 5, per_number_per_day: 1}", 1)
	h := newHarnessWith(t, open)
	close(h.release)

	var list listNumbersOut
	h.tool("list_numbers", nil, &list, false)
	if list.Mode != "open" || strings.Join(list.Countries, ",") != "IT,IE" || strings.Join(list.Types, ",") != "fixed,mobile" ||
		!strings.HasPrefix(list.Hours, "any time") || list.PerNumberPerDay != 1 {
		t.Errorf("list_numbers = %+v", list)
	}

	// A number not on the allowlist, once a day.
	var placed placeCallOut
	var rec Record
	args := map[string]any{"to": "+353 85 123 4567", "objective": "x", "callee": "Seán", "kind": "personal"}
	h.tool("place_call", args, &placed, false)
	h.tool("get_call", map[string]any{"call_id": placed.CallID, "wait_seconds": 10}, &rec, false)
	if rec.To != "+353851234567" || rec.Label != "" || rec.Callee != "Seán" || rec.Status != "completed" {
		t.Errorf("open call = %+v", rec)
	}
	if msg := h.tool("place_call", args, nil, true); !strings.Contains(msg, "number_daily_limit") {
		t.Errorf("second call to the same number: %s", msg)
	}
	// Countries and types still apply.
	for to, want := range map[string]string{"+44 20 7946 0000": "country_not_allowed", "899 123 456": "number_type_not_allowed"} {
		if msg := h.tool("place_call", map[string]any{"to": to, "objective": "x"}, nil, true); !strings.Contains(msg, want) {
			t.Errorf("%s: %s, want %s", to, msg, want)
		}
	}
	// Listed numbers have no per-number limit.
	for range 2 {
		h.tool("place_call", map[string]any{"to": "Pizzeria", "objective": "x"}, &placed, false)
		h.tool("get_call", map[string]any{"call_id": placed.CallID, "wait_seconds": 10}, &rec, false)
	}
}

func TestPrivateHistory(t *testing.T) {
	h := newHarness(t)
	h.s.Index.Private = true
	close(h.release)
	var placed placeCallOut
	var rec Record
	h.tool("place_call", map[string]any{"to": "Pizzeria", "objective": "x"}, &placed, false)
	h.tool("get_call", map[string]any{"call_id": placed.CallID, "wait_seconds": 10}, &rec, false)
	if len(rec.Transcript) != 2 || rec.Outcome == nil {
		t.Errorf("get_call should still return the transcript and outcome: %+v", rec)
	}
	b, err := os.ReadFile(h.s.Index.path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "Pronto?") || strings.Contains(string(b), "Arriva alle 20") || !strings.Contains(string(b), placed.CallID) {
		t.Errorf("history file = %s", b)
	}
}
