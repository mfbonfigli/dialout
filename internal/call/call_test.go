package call

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/coder/websocket"

	"github.com/mfbonfigli/dialout/internal/audio"
	"github.com/mfbonfigli/dialout/internal/recorder"
	"github.com/mfbonfigli/dialout/internal/sonic"
	"github.com/mfbonfigli/dialout/internal/twilio"
	"github.com/mfbonfigli/dialout/internal/voice"
	"github.com/mfbonfigli/dialout/internal/watchdog"
	"github.com/mfbonfigli/dialout/prompts"
)

// These tests run whole calls with no network: the harness plays Twilio (its REST API and, as a
// "phone", the Media Stream with a valid signature) and Nova Sonic (a scripted Bedrock stream
// behind the real sonic.Engine).

const (
	testHost  = "calls.example.test"
	authToken = "test-auth-token"
)

type harness struct {
	t         *testing.T
	twilioSrv *httptest.Server
	streamSrv *httptest.Server
	streams   *Streams
	sonic     *fakeSonic
	out       string

	mu       sync.Mutex
	status   string
	created  chan url.Values
	hungUp   chan string
	notices  []string
	events   []Event
	dialed   bool
	statusFn func(n int) string // the status of the n-th poll, before the call is answered
	polls    int
}

func newHarness(t *testing.T) *harness {
	h := &harness{
		t:       t,
		sonic:   newFakeSonic(),
		out:     t.TempDir(),
		created: make(chan url.Values, 1),
		hungUp:  make(chan string, 4),
	}
	h.twilioSrv = httptest.NewServer(http.HandlerFunc(h.serveREST))
	t.Cleanup(h.twilioSrv.Close)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	h.streams = &Streams{AuthToken: authToken, Log: quiet}
	h.streamSrv = httptest.NewServer(h.streams)
	t.Cleanup(h.streamSrv.Close)
	return h
}

func (h *harness) config() Config {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	return Config{
		Twilio:  &twilio.Client{AccountSID: "ACtest", KeySID: "SKtest", KeySecret: "secret", BaseURL: h.twilioSrv.URL},
		From:    "+4915700000000",
		Tunnel:  NoTunnel{Host: testHost},
		Streams: h.streams,
		Engine: &sonic.Engine{Connect: func(context.Context) (sonic.Stream, error) {
			h.mu.Lock()
			h.dialed = true
			h.mu.Unlock()
			return h.sonic, nil
		}},
		Recorder: recorder.Config{
			SpoolDir: filepath.Join(h.t.TempDir(), "spool"),
			Audio:    true,
			Uploader: &recorder.Dir{Root: h.out},
			Log:      quiet,
		},
		Owner: "Marco",
		Notify: func(text string) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.notices = append(h.notices, text)
		},
		Observe: func(e Event) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.events = append(h.events, e)
		},
		Timing: Timing{
			Poll: 10 * time.Millisecond, StreamWait: 2 * time.Second, OpeningCue: 150 * time.Millisecond,
			WrapUp: time.Second, AgentQuiet: 100 * time.Millisecond, EndNoAudio: 300 * time.Millisecond, Overtime: time.Second,
			EndMax: 3 * time.Second, OutcomeIdle: 300 * time.Millisecond, Drain: 500 * time.Millisecond,
			TimeLimitMargin: 30 * time.Second, Tick: 20 * time.Millisecond,
		},
		Log: quiet,
	}
}

func params() Params {
	return Params{
		CallID:      "20261022-120000-abcdef",
		To:          "+393330000000",
		Label:       "Marco (mobile)",
		Brief:       prompts.Brief{Opening: "Ciao, sono l'assistente di Marco.", Objective: "Chiedere a che ora arriva."},
		MaxDuration: 10 * time.Second,
		RingTimeout: 5 * time.Second,
		Policy:      json.RawMessage(`{"Allowed":true}`),
	}
}

func (h *harness) setStatus(s string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.status = s
}

// serveREST is Twilio's Calls resource for one call, CA1.
func (h *harness) serveREST(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	base := "/2010-04-01/Accounts/ACtest/Calls"
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && r.URL.Path == base+".json":
		h.status = "queued"
		h.created <- r.PostForm
	case r.Method == http.MethodGet && r.URL.Path == base+"/CA1.json":
		h.polls++
		if h.statusFn != nil && h.status != "in-progress" && h.status != "completed" && h.status != "canceled" {
			h.status = h.statusFn(h.polls)
		}
	case r.Method == http.MethodPost && r.URL.Path == base+"/CA1.json":
		h.status = r.PostForm.Get("Status")
		h.hungUp <- h.status
	default:
		w.WriteHeader(404)
		return
	}
	json.NewEncoder(w).Encode(twilio.Call{SID: "CA1", Status: h.status})
}

// phone is the Twilio side of the media stream.
type phone struct {
	t      *testing.T
	ws     *websocket.Conn
	echo   bool // play the agent's audio at once: echo every mark
	mu     sync.Mutex
	media  int // µ-law bytes received
	marks  []string
	clears int
	done   chan struct{}
}

var streamURLRE = regexp.MustCompile(`<Stream url="wss://([^"/]+)(/twilio/stream/[^"]+)">`)

// answer picks up: the call goes in-progress and the stream connects as Twilio would.
func (h *harness) answer(ctx context.Context, twiml, callSID string, echo bool) *phone {
	m := streamURLRE.FindStringSubmatch(twiml)
	if m == nil || m[1] != testHost {
		h.t.Fatalf("no stream URL for %s in %s", testHost, twiml)
	}
	path := m[2]
	h.setStatus("in-progress")
	sig := twilio.Signature(authToken, "wss://"+testHost+path, nil)
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(h.streamSrv.URL, "http")+path,
		&websocket.DialOptions{HTTPHeader: http.Header{"X-Twilio-Signature": {sig}}})
	if err != nil {
		h.t.Fatalf("stream refused: %v", err)
	}
	p := &phone{t: h.t, ws: ws, echo: echo, done: make(chan struct{})}
	p.send(ctx, map[string]any{"event": "connected", "protocol": "Call", "version": "1.0.0"})
	p.send(ctx, map[string]any{"event": "start", "streamSid": "MZ1", "start": map[string]any{
		"accountSid": "ACtest", "streamSid": "MZ1", "callSid": callSID, "tracks": []string{"inbound"},
		"customParameters": map[string]string{"call_id": params().CallID},
		"mediaFormat":      map[string]any{"encoding": "audio/x-mulaw", "sampleRate": 8000, "channels": 1},
	}})
	go p.talk(ctx)
	go p.listen(ctx)
	return p
}

func (p *phone) send(ctx context.Context, v any) error {
	b, _ := json.Marshal(v)
	return p.ws.Write(ctx, websocket.MessageText, b)
}

// talk streams silence from the callee, 20 ms at a time.
func (p *phone) talk(ctx context.Context) {
	silence := base64.StdEncoding.EncodeToString(audio.PCMToMulaw(nil, make([]int16, 160)))
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for ts := 0; ; ts += 20 {
		select {
		case <-p.done:
			return
		case <-tick.C:
		}
		err := p.send(ctx, map[string]any{"event": "media", "streamSid": "MZ1", "media": map[string]any{
			"track": "inbound", "chunk": "1", "timestamp": itoa(ts), "payload": silence,
		}})
		if err != nil {
			return
		}
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// listen takes what the bridge sends: audio, marks (echoed if the phone plays at once), clears.
func (p *phone) listen(ctx context.Context) {
	defer close(p.done)
	for {
		_, data, err := p.ws.Read(ctx)
		if err != nil {
			return
		}
		var m struct {
			Event string `json:"event"`
			Media struct {
				Payload string `json:"payload"`
			} `json:"media"`
			Mark struct {
				Name string `json:"name"`
			} `json:"mark"`
		}
		json.Unmarshal(data, &m)
		p.mu.Lock()
		switch m.Event {
		case "media":
			b, _ := base64.StdEncoding.DecodeString(m.Media.Payload)
			p.media += len(b)
		case "mark":
			p.marks = append(p.marks, m.Mark.Name)
		case "clear":
			p.clears++
		}
		echo := p.echo && m.Event == "mark"
		p.mu.Unlock()
		if echo {
			p.send(ctx, map[string]any{"event": "mark", "streamSid": "MZ1", "mark": map[string]string{"name": m.Mark.Name}})
		}
	}
}

// hangUp is the callee hanging up: Twilio sends stop.
func (p *phone) hangUp(ctx context.Context) {
	p.send(ctx, map[string]any{"event": "stop", "streamSid": "MZ1", "stop": map[string]string{"accountSid": "ACtest", "callSid": "CA1"}})
}

// fakeSonic is a scripted Bedrock stream for sonic.Engine.
type fakeSonic struct {
	mu     sync.Mutex
	inputs []string // event names, in order
	cues   []string
	audio  int // audioInput events
	in     chan map[string]map[string]any
	out    chan types.InvokeModelWithBidirectionalStreamOutput
	closed bool
}

func newFakeSonic() *fakeSonic {
	return &fakeSonic{
		in:  make(chan map[string]map[string]any, 10000),
		out: make(chan types.InvokeModelWithBidirectionalStreamOutput, 1024),
	}
}

func (f *fakeSonic) Send(_ context.Context, ev types.InvokeModelWithBidirectionalStreamInput) error {
	var m map[string]map[string]map[string]any
	json.Unmarshal(ev.(*types.InvokeModelWithBidirectionalStreamInputMemberChunk).Value.Bytes, &m)
	f.mu.Lock()
	defer f.mu.Unlock()
	for name, body := range m["event"] {
		f.inputs = append(f.inputs, name)
		switch name {
		case "audioInput":
			f.audio++
		case "textInput":
			if c, _ := body["content"].(string); strings.HasPrefix(c, "[") {
				f.cues = append(f.cues, c)
			}
		}
		select {
		case f.in <- map[string]map[string]any{name: body}:
		default:
		}
	}
	return nil
}

func (f *fakeSonic) Events() <-chan types.InvokeModelWithBidirectionalStreamOutput { return f.out }
func (f *fakeSonic) Err() error                                                    { return nil }
func (f *fakeSonic) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.out)
	}
	return nil
}

func (f *fakeSonic) emit(name string, body map[string]any) {
	b, _ := json.Marshal(map[string]any{"event": map[string]any{name: body}})
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.out <- &types.InvokeModelWithBidirectionalStreamOutputMemberChunk{Value: types.BidirectionalOutputPayloadPart{Bytes: b}}
	}
}

// waitInput waits for an input event that matches.
func (f *fakeSonic) waitInput(t *testing.T, match func(name string, body map[string]any) bool) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev := <-f.in:
			for name, body := range ev {
				if match(name, body) {
					return
				}
			}
		case <-timeout:
			t.Fatal("expected input never reached Sonic")
		}
	}
}

func (f *fakeSonic) say(id string, samples int, text string) {
	pcm := audio.PCMToLE16(nil, fill(samples, 1000))
	f.emit("contentStart", map[string]any{"contentId": id, "type": "AUDIO", "role": "ASSISTANT"})
	f.emit("audioOutput", map[string]any{"contentId": id, "content": base64.StdEncoding.EncodeToString(pcm)})
	f.emit("contentEnd", map[string]any{"contentId": id, "type": "AUDIO", "stopReason": "END_TURN"})
	f.emit("contentStart", map[string]any{"contentId": id + "t", "type": "TEXT", "role": "ASSISTANT", "additionalModelFields": `{"generationStage":"FINAL"}`})
	f.emit("textOutput", map[string]any{"contentId": id + "t", "content": text})
	f.emit("contentEnd", map[string]any{"contentId": id + "t", "type": "TEXT", "stopReason": "END_TURN"})
}

func (f *fakeSonic) tool(id, name, input string) {
	f.emit("contentStart", map[string]any{"contentId": id, "type": "TOOL", "role": "TOOL"})
	f.emit("toolUse", map[string]any{"contentId": id, "toolName": name, "toolUseId": id, "content": input})
	f.emit("contentEnd", map[string]any{"contentId": id, "type": "TOOL", "stopReason": "TOOL_USE"})
}

// endsWhenClosed makes the fake end its stream after sessionEnd, as Bedrock does.
func (f *fakeSonic) endsWhenClosed(t *testing.T) {
	f.waitInput(t, func(name string, _ map[string]any) bool { return name == "sessionEnd" })
	f.Close()
}

func fill(n int, v int16) []int16 {
	s := make([]int16, n)
	for i := range s {
		s[i] = v
	}
	return s
}

func startCall(h *harness, ctx context.Context, cfg Config, p Params) chan *Result {
	ch := make(chan *Result, 1)
	go func() { ch <- Run(ctx, cfg, p) }()
	return ch
}

func waitResult(t *testing.T, ch chan *Result) *Result {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(15 * time.Second):
		t.Fatal("the call never ended")
		return nil
	}
}

func TestCallEndToEnd(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	resCh := startCall(h, ctx, h.config(), params())

	form := <-h.created
	if form.Get("To") != "+393330000000" || form.Get("From") != "+4915700000000" ||
		form.Get("Timeout") != "5" || form.Get("TimeLimit") != "40" {
		t.Errorf("create call form = %v", form)
	}
	twiml := form.Get("Twiml")
	if !strings.Contains(twiml, `<Parameter name="call_id" value="20261022-120000-abcdef"/>`) {
		t.Errorf("TwiML = %s", twiml)
	}
	ph := h.answer(ctx, twiml, "CA1", true)

	// The callee says nothing: the opening cue starts the agent.
	h.sonic.waitInput(t, func(name string, b map[string]any) bool {
		return name == "textInput" && b["content"] == builtinCues.Opening
	})
	h.sonic.say("a1", 3200, "Ciao, sono l'assistente di Marco.")
	h.sonic.tool("t1", "report_outcome", `{"result":"success","summary":"Arriva alle 20.","details":"verso le 20"}`)
	h.sonic.tool("t2", "end_call", `{"reason":"fatto"}`)
	h.sonic.say("a2", 1600, "Arrivederci.")
	go h.sonic.endsWhenClosed(t)

	if st := <-h.hungUp; st != "completed" {
		t.Errorf("hung up with Status=%s", st)
	}
	res := waitResult(t, resCh)
	<-ph.done

	if res.Err != nil || res.Status != "completed" || !res.Answered || res.EndReason != "end_call" || res.TwilioSID != "CA1" {
		t.Fatalf("result = %+v", res)
	}
	if o := res.Outcome; o == nil || o.Result != "success" || o.Details != "verso le 20" {
		t.Errorf("outcome = %+v", res.Outcome)
	}
	var agent []string
	for _, l := range res.Transcript {
		agent = append(agent, l.Speaker+": "+l.Text)
	}
	if strings.Join(agent, " | ") != "agent: Ciao, sono l'assistente di Marco. | agent: Arrivederci." {
		t.Errorf("transcript = %v", agent)
	}

	// Twilio got all the agent's audio as µ-law, a mark every 200 ms, and no clear.
	ph.mu.Lock()
	if ph.media != 4800 || len(ph.marks) != 3 || ph.clears != 0 {
		t.Errorf("phone got %d µ-law bytes, marks %v, %d clears", ph.media, ph.marks, ph.clears)
	}
	ph.mu.Unlock()

	// Sonic heard the callee and got exactly one cue; the session closed in order.
	h.sonic.mu.Lock()
	if h.sonic.audio == 0 || len(h.sonic.cues) != 1 {
		t.Errorf("sonic got %d audio events, cues %v", h.sonic.audio, h.sonic.cues)
	}
	in := strings.Join(h.sonic.inputs, ",")
	h.sonic.mu.Unlock()
	if !strings.HasSuffix(in, "contentEnd,promptEnd,sessionEnd") {
		t.Errorf("sonic inputs end with %s", in[max(0, len(in)-60):])
	}

	// The recording, the transcript and meta.json are where they belong.
	prefix := filepath.Join(h.out, "calls", time.Now().UTC().Format("2006/01/02"), params().CallID)
	wav, err := os.ReadFile(filepath.Join(prefix, "recording.wav"))
	if err != nil || len(wav) <= 44 {
		t.Fatalf("recording: %v (%d bytes)", err, len(wav))
	}
	if res.Recording.RecordingKey == "" || res.RecordingErr != nil {
		t.Errorf("recording result = %+v, %v", res.Recording, res.RecordingErr)
	}
	tr, _ := os.ReadFile(filepath.Join(prefix, "transcript.jsonl"))
	if !strings.Contains(string(tr), `"speaker":"agent","text":"Arrivederci."`) {
		t.Errorf("transcript.jsonl = %s", tr)
	}
	var m meta
	mb, _ := os.ReadFile(filepath.Join(prefix, "meta.json"))
	if err := json.Unmarshal(mb, &m); err != nil {
		t.Fatal(err)
	}
	if m.Status != "completed" || m.TwilioSID != "CA1" || m.Outcome == nil || m.StreamStarted == nil || m.Ended == nil ||
		m.EndReason != "end_call" || string(m.Policy) != `{"Allowed":true}` || m.Objective != "Chiedere a che ora arriva." ||
		m.Engine != "sonic" || m.Voice != "tiffany" || !m.Audio {
		t.Errorf("meta.json = %s", mb)
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.notices) != 2 || !strings.HasPrefix(h.notices[0], "📞 Calling Marco (mobile): Chiedere") ||
		!strings.HasPrefix(h.notices[1], "✅ Call with Marco (mobile) ended") || !strings.Contains(h.notices[1], "success. Arriva alle 20.") {
		t.Errorf("notices = %q", h.notices)
	}
}

func TestCallNoAnswer(t *testing.T) {
	h := newHarness(t)
	h.statusFn = func(n int) string {
		if n < 3 {
			return "ringing"
		}
		return "no-answer"
	}
	cfg := h.config()
	res := waitResult(t, startCall(h, context.Background(), cfg, params()))
	<-h.created
	if res.Status != "no-answer" || res.Answered || res.Err != nil || h.dialed {
		t.Fatalf("result = %+v, sonic dialed: %v", res, h.dialed)
	}
	if len(h.notices) != 2 || !strings.HasPrefix(h.notices[1], "📵 Marco (mobile) didn't answer (no-answer)") {
		t.Errorf("notices = %q", h.notices)
	}
	mb, _ := os.ReadFile(filepath.Join(h.out, "calls", time.Now().UTC().Format("2006/01/02"), params().CallID, "meta.json"))
	if !strings.Contains(string(mb), `"status":"no-answer"`) {
		t.Errorf("meta.json = %s", mb)
	}
}

func TestCallStreamForAnotherCall(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	resCh := startCall(h, ctx, h.config(), params())
	form := <-h.created
	h.answer(ctx, form.Get("Twiml"), "CAother", true)
	res := waitResult(t, resCh)
	if res.Err == nil || res.EndReason != "invalid_stream" || h.dialed {
		t.Fatalf("result = %+v, sonic dialed: %v", res, h.dialed)
	}
	select {
	case st := <-h.hungUp:
		if st != "completed" {
			t.Errorf("hung up with %s", st)
		}
	default:
		t.Error("the call was not hung up")
	}
}

func TestBargeInAndCalleeHangsUp(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	resCh := startCall(h, ctx, h.config(), params())
	form := <-h.created
	ph := h.answer(ctx, form.Get("Twiml"), "CA1", false) // audio queues up: no mark comes back
	h.sonic.waitInput(t, func(name string, b map[string]any) bool { return name == "textInput" })
	pcm := audio.PCMToLE16(nil, fill(16000, 1000)) // 2 s
	h.sonic.emit("audioOutput", map[string]any{"contentId": "a", "content": base64.StdEncoding.EncodeToString(pcm)})
	h.sonic.emit("contentEnd", map[string]any{"contentId": "a", "type": "AUDIO", "stopReason": "INTERRUPTED"})
	deadline := time.Now().Add(3 * time.Second)
	for {
		ph.mu.Lock()
		c := ph.clears
		ph.mu.Unlock()
		if c > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no clear after the interruption")
		}
		time.Sleep(10 * time.Millisecond)
	}
	go h.sonic.endsWhenClosed(t)
	ph.hangUp(ctx)
	res := waitResult(t, resCh)
	if res.EndReason != "callee_hung_up" || res.Err != nil {
		t.Fatalf("result = %+v", res)
	}
	found := false
	for _, e := range h.events {
		found = found || e.Kind == "interrupted"
	}
	if !found {
		t.Error("no interrupted event")
	}
}

func TestStreamsRefuse(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	token, _ := twilio.NewStreamToken()
	other, _ := twilio.NewStreamToken()
	exp := h.streams.Expect(token, testHost, time.Now().Add(time.Minute))
	defer exp.Cancel()
	expired := h.streams.Expect(other, testHost, time.Now().Add(-time.Second))
	defer expired.Cancel()

	dial := func(path, sig string) error {
		hdr := http.Header{}
		if sig != "" {
			hdr.Set("X-Twilio-Signature", sig)
		}
		ws, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(h.streamSrv.URL, "http")+path, &websocket.DialOptions{HTTPHeader: hdr})
		if err == nil {
			go func() {
				st := <-exp.C()
				st.Close(websocket.StatusNormalClosure, "")
			}()
			ws.CloseNow()
			return nil
		}
		if resp != nil && resp.StatusCode != 404 {
			t.Errorf("%s: status %d, want 404", path, resp.StatusCode)
		}
		return err
	}
	sign := func(path string) string { return twilio.Signature(authToken, "wss://"+testHost+path, nil) }
	good := "/twilio/stream/" + token

	if dial(good, "") == nil {
		t.Error("accepted without a signature")
	}
	if dial(good, twilio.Signature("wrong", "wss://"+testHost+good, nil)) == nil {
		t.Error("accepted with a wrong signature")
	}
	if dial("/twilio/stream/"+strings.Repeat("A", 43), sign("/twilio/stream/"+strings.Repeat("A", 43))) == nil {
		t.Error("accepted an unknown token")
	}
	if dial("/twilio/stream/"+other, sign("/twilio/stream/"+other)) == nil {
		t.Error("accepted an expired token")
	}
	if dial("/", sign("/")) == nil {
		t.Error("accepted another path")
	}
	if err := dial(good, sign(good)); err != nil {
		t.Fatalf("refused a valid stream: %v", err)
	}
	if dial(good, sign(good)) == nil {
		t.Error("accepted a token twice")
	}
}

func TestMarkInterrupted(t *testing.T) {
	for in, want := range map[string]string{
		`{"status":"in-progress","to":"x"}`: `{"status":"interrupted","to":"x"}`,
		`{"status":"completed"}`:            `{"status":"completed"}`,
		`not json`:                          `{"status":"interrupted"}`,
	} {
		if got := string(MarkInterrupted([]byte(in))); got != want {
			t.Errorf("MarkInterrupted(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestVAD(t *testing.T) {
	var v vad
	quiet, loud := fill(160, 50), fill(160, 3000)
	got := []bool{v.speech(quiet), v.speech(loud), v.speech(loud), v.speech(quiet), v.speech(loud), v.speech(loud), v.speech(loud), v.speech(loud)}
	want := []bool{false, false, false, false, false, false, true, false}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("speech = %v, want %v", got, want)
		}
	}
}

func TestFindCloudflared(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "cloudflared.exe")
	os.WriteFile(bin, nil, 0o700)
	if got, err := FindCloudflared(bin); err != nil || got != bin {
		t.Errorf("FindCloudflared(%s) = %s, %v", bin, got, err)
	}
	if _, err := FindCloudflared(bin + ".missing"); err == nil {
		t.Error("a configured path that doesn't exist was accepted")
	}
}

func TestTimeLimit(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p := params()
	p.MaxDuration = 2 * time.Second // wrap-up cue at 1 s, final cue at 2 s, 1 s of grace
	resCh := startCall(h, ctx, h.config(), p)
	form := <-h.created
	h.answer(ctx, form.Get("Twiml"), "CA1", true)

	// The agent says nothing until the final cue, then only goodbye (no tools).
	h.sonic.waitInput(t, func(name string, b map[string]any) bool {
		return name == "textInput" && b["content"] == builtinCues.Final
	})
	h.sonic.say("bye", 800, "Mi scusi, devo chiudere. Arrivederci.")
	go h.sonic.endsWhenClosed(t)
	res := waitResult(t, resCh)

	if res.EndReason != "time_limit" || res.Err != nil {
		t.Fatalf("result = %+v", res)
	}
	if res.Talk < 2*time.Second || res.Talk > 2900*time.Millisecond {
		t.Errorf("hung up after %v: want after the goodbye, within the grace period", res.Talk)
	}
	h.sonic.mu.Lock()
	cues := strings.Join(h.sonic.cues, " | ")
	h.sonic.mu.Unlock()
	if cues != builtinCues.Opening+" | "+builtinCues.WrapUp+" | "+builtinCues.Final {
		t.Errorf("cues = %s", cues)
	}
	if n := len(res.Transcript); n == 0 || res.Transcript[n-1].Text != "Mi scusi, devo chiudere. Arrivederci." {
		t.Errorf("transcript = %+v", res.Transcript)
	}
}

func TestCueWaitsForTheAgentsAnswer(t *testing.T) {
	now := time.Now()
	b := &bridge{r: &run{t: Timing{AgentQuiet: 100 * time.Millisecond}}, outstanding: map[string]bool{}}
	b.lastAudio = now.Add(-5 * time.Second)
	b.lastTurnEnd = now.Add(-3 * time.Second)
	b.lastCallee = now.Add(-time.Second) // the callee spoke last: Sonic is preparing its answer
	if b.cueSafe(now) {
		t.Error("cue allowed while Sonic owes an answer")
	}
	b.lastTurnEnd = now.Add(-500 * time.Millisecond) // it answered
	if !b.cueSafe(now) {
		t.Error("cue refused after the answer")
	}
	b.lastTurnEnd = now.Add(-20 * time.Second)
	b.lastCallee = now.Add(-replyWait - time.Second) // no answer for too long: go ahead
	if !b.cueSafe(now) {
		t.Error("cue refused after waiting too long for an answer")
	}
	b.outstanding["m1"] = true // agent audio still playing
	if b.cueSafe(now) {
		t.Error("cue allowed over the agent's audio")
	}
}

func TestBadVoiceFailsBeforeCalling(t *testing.T) {
	h := newHarness(t)
	cfg := h.config()
	cfg.Voice = "joanna" // a Polly voice, not Sonic's
	res := waitResult(t, startCall(h, context.Background(), cfg, params()))
	if res.Err == nil || res.Status != "failed" || res.TwilioSID != "" {
		t.Fatalf("result = %+v", res)
	}
	select {
	case <-h.created:
		t.Error("a call was placed with an invalid voice")
	default:
	}
	if len(h.notices) != 0 {
		t.Errorf("notices = %q", h.notices)
	}
}

func TestAgentClosesTheConversation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	resCh := startCall(h, ctx, h.config(), params())
	form := <-h.created
	ph := h.answer(ctx, form.Get("Twiml"), "CA1", true)

	// The agent says goodbye and closes the conversation itself, with no outcome and no end_call,
	// as ElevenLabs does with its own end_call tool.
	h.sonic.waitInput(t, func(name string, b map[string]any) bool { return name == "textInput" })
	h.sonic.say("bye", 1600, "Grazie, arrivederci.")
	h.sonic.Close()

	res := waitResult(t, resCh)
	<-ph.done
	if res.Err != nil || res.EndReason != "agent_closed" || res.Status != "completed" || res.Outcome != nil {
		t.Fatalf("result = %+v", res)
	}
	ph.mu.Lock()
	if ph.media != 1600 || len(ph.marks) != 1 {
		t.Errorf("the goodbye didn't play in full: %d µ-law bytes, marks %v", ph.media, ph.marks)
	}
	ph.mu.Unlock()
	if n := len(h.notices); n != 2 || !strings.HasPrefix(h.notices[1], "✅ Call with Marco (mobile) ended") ||
		!strings.Contains(h.notices[1], "no outcome reported") {
		t.Errorf("notices = %q", h.notices)
	}
}

func TestGoodbyeThenEndCall(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	cfg := h.config()
	cfg.Engine = closingEngine{cfg.Engine, prompts.WithGoodbye}
	resCh := startCall(h, ctx, cfg, params())
	form := <-h.created
	h.answer(ctx, form.Get("Twiml"), "CA1", true)

	// As ElevenLabs and OpenAI do: the goodbye first, then end_call in the same turn. The bridge
	// hangs up once the goodbye has played, without waiting for audio that won't come.
	h.sonic.waitInput(t, func(name string, b map[string]any) bool { return name == "textInput" })
	h.sonic.say("bye", 1600, "Perfetto, grazie. Buonasera!")
	h.sonic.tool("t1", "end_call", `{"reason":"fatto"}`)
	go h.sonic.endsWhenClosed(t)
	res := waitResult(t, resCh)
	if res.Err != nil || res.EndReason != "end_call" {
		t.Fatalf("result = %+v", res)
	}
}

// builtinCues are the cues of the built-in prompt file, which the test calls use.
var builtinCues = func() prompts.Cues {
	r, err := prompts.Builtin().Render(prompts.ToolsThenGoodbye, prompts.Vars{Owner: "Marco", Brief: prompts.Brief{Objective: "x"}})
	if err != nil {
		panic(err)
	}
	return r.Cues
}()

func TestCallWithoutStorage(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	cfg := h.config()
	cfg.Recorder.Disabled = true
	resCh := startCall(h, ctx, cfg, params())
	form := <-h.created
	h.answer(ctx, form.Get("Twiml"), "CA1", true)
	h.sonic.waitInput(t, func(name string, b map[string]any) bool { return name == "textInput" })
	h.sonic.say("bye", 1600, "Perfetto, grazie. Buonasera!")
	h.sonic.tool("t1", "end_call", `{"reason":"fatto"}`)
	go h.sonic.endsWhenClosed(t)
	res := waitResult(t, resCh)
	if res.Err != nil || res.RecordingErr != nil || res.Recording.Prefix != "" || len(res.Transcript) == 0 {
		t.Fatalf("result = %+v", res)
	}
	// Nothing on disk: no stored call, no spool.
	for _, dir := range []string{h.out, cfg.Recorder.SpoolDir} {
		if entries, _ := os.ReadDir(dir); len(entries) > 0 {
			t.Errorf("%s holds %d entries", dir, len(entries))
		}
	}
}

// fakeJudge answers the watchdog's questions with a fixed function of the state.
type fakeJudge struct {
	mu     sync.Mutex
	calls  int
	answer func(st watchdog.State) map[string]float64
}

func (j *fakeJudge) Judge(_ context.Context, state any, qs []prompts.Question) (watchdog.Answers, error) {
	j.mu.Lock()
	j.calls++
	j.mu.Unlock()
	if len(qs) != len(prompts.WatchdogQuestions) {
		return watchdog.Answers{}, fmt.Errorf("%d questions", len(qs))
	}
	return watchdog.Answers{Probabilities: j.answer(state.(watchdog.State)), Cost: 0.001}, nil
}

func (j *fakeJudge) count() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.calls
}

func watchdogCall(t *testing.T, judge *fakeJudge) (*harness, chan *Result) {
	h := newHarness(t)
	cfg := h.config()
	cfg.Watchdog = &watchdog.Config{Judge: judge, Thresholds: watchdog.DefaultThresholds, Every: 4, Timeout: time.Second}
	resCh := startCall(h, context.Background(), cfg, params())
	form := <-h.created
	h.answer(context.Background(), form.Get("Twiml"), "CA1", true)
	h.sonic.waitInput(t, func(name string, b map[string]any) bool { return name == "textInput" })
	return h, resCh
}

// hear makes the fake Sonic report a final line from the callee.
func (f *fakeSonic) hear(id, text string) {
	f.emit("contentStart", map[string]any{"contentId": id, "type": "TEXT", "role": "USER", "additionalModelFields": `{"generationStage":"FINAL"}`})
	f.emit("textOutput", map[string]any{"contentId": id, "content": text})
	f.emit("contentEnd", map[string]any{"contentId": id, "type": "TEXT", "stopReason": "END_TURN"})
}

func TestWatchdogHangsUpOnVoicemail(t *testing.T) {
	judge := &fakeJudge{answer: func(watchdog.State) map[string]float64 {
		return map[string]float64{prompts.QVoicemail: 0.97}
	}}
	h, resCh := watchdogCall(t, judge)
	h.sonic.hear("c1", "Risponde la segreteria telefonica di")
	h.sonic.say("a1", 1600, "Pronto, buongiorno!") // the first check comes after two lines
	go h.sonic.endsWhenClosed(t)
	res := waitResult(t, resCh)
	if res.EndReason != "watchdog_hang_up" || res.Watchdog == nil || len(res.Watchdog.Actions) != 1 ||
		res.Watchdog.Actions[0].Action != "hang_up" || res.Watchdog.Actions[0].Probabilities[prompts.QVoicemail] != 0.97 ||
		res.Watchdog.Checks < 1 || res.Watchdog.Cost == 0 {
		t.Fatalf("result = %+v, watchdog %+v", res, res.Watchdog)
	}
}

func TestWatchdogChecksEveryFewLines(t *testing.T) {
	// Nothing to hang up on: one check after the first two lines, then one every four.
	judge := &fakeJudge{answer: func(watchdog.State) map[string]float64 {
		return map[string]float64{prompts.QVoicemail: 0.1, prompts.QHostile: 0.2}
	}}
	h, resCh := watchdogCall(t, judge)
	lines := []string{"Pronto?", "Buongiorno, vorrei ordinare due pizze.", "Certo, per che ora?", "Per le 20.",
		"Va bene, a che nome?", "Rossi.", "Perfetto."}
	for i, l := range lines {
		if i%2 == 0 {
			h.sonic.hear(fmt.Sprintf("c%d", i), l)
		} else {
			h.sonic.say(fmt.Sprintf("a%d", i), 160, l)
		}
		time.Sleep(30 * time.Millisecond)
	}
	h.sonic.tool("t1", "end_call", `{"reason":"fatto"}`)
	h.sonic.say("bye", 1600, "Grazie, buonasera!")
	go h.sonic.endsWhenClosed(t)
	res := waitResult(t, resCh)
	if n := judge.count(); n != 2 { // after lines 2 and 6; none after end_call
		t.Errorf("judge called %d times, want 2", n)
	}
	if res.EndReason != "end_call" || len(res.Watchdog.Actions) != 0 {
		t.Errorf("result = %+v, watchdog %+v", res, res.Watchdog)
	}
}

// closingEngine makes an engine report another way of closing, e.g. Sonic's fake standing in for
// an engine that says goodbye and calls end_call in the same turn.
type closingEngine struct {
	voice.Engine
	closing prompts.Closing
}

func (e closingEngine) Prepare(c voice.Config) (voice.Config, error) {
	c, err := e.Engine.Prepare(c)
	c.Closing = e.closing
	return c, err
}

func TestSonicEndCallWaitsForTheGoodbye(t *testing.T) {
	// Sonic calls end_call before its goodbye. An end_call right after an ordinary turn must not
	// count as said goodbye: dialout waits for it (and gives up only if none starts).
	h := newHarness(t)
	ctx := context.Background()
	resCh := startCall(h, ctx, h.config(), params())
	form := <-h.created
	h.answer(ctx, form.Get("Twiml"), "CA1", true)
	h.sonic.waitInput(t, func(name string, b map[string]any) bool { return name == "textInput" })
	h.sonic.say("a1", 1600, "Ho capito, verifico e la richiamo.")
	h.sonic.tool("t1", "end_call", `{"reason":"verifica"}`)
	go h.sonic.endsWhenClosed(t)
	res := waitResult(t, resCh)
	if res.EndReason != "end_call_without_goodbye" {
		t.Fatalf("result = %+v", res)
	}
}
