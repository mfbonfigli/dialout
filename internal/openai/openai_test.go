package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/mfbonfigli/dialout/internal/audio"
	"github.com/mfbonfigli/dialout/internal/voice"
	"github.com/mfbonfigli/dialout/prompts"
)

// fakeRealtime is the Realtime API: it records the session settings and plays the server side.
type fakeRealtime struct {
	t      *testing.T
	srv    *httptest.Server
	refuse bool // answer session.update with an error

	mu      sync.Mutex
	model   string
	session map[string]any
	ws      *websocket.Conn
	got     chan map[string]any
}

func newFake(t *testing.T) *fakeRealtime {
	f := &fakeRealtime{t: t, got: make(chan map[string]any, 1024)}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/realtime" || r.Header.Get("Authorization") != "Bearer key" {
			w.WriteHeader(401)
			return
		}
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ws.SetReadLimit(4 << 20)
		ctx := context.Background()
		f.send0(ws, map[string]any{"type": "session.created", "session": map[string]any{}})
		_, data, err := ws.Read(ctx)
		if err != nil {
			return
		}
		var upd map[string]any
		json.Unmarshal(data, &upd)
		f.mu.Lock()
		f.model, f.ws = r.URL.Query().Get("model"), ws
		if s, ok := upd["session"].(map[string]any); ok && upd["type"] == "session.update" {
			f.session = s
		}
		f.mu.Unlock()
		if f.refuse {
			f.send0(ws, map[string]any{"type": "error", "error": map[string]any{"code": "invalid_value", "message": "voice not supported"}})
		} else {
			f.send0(ws, map[string]any{"type": "session.updated", "session": upd["session"]})
		}
		for {
			_, data, err := ws.Read(ctx)
			if err != nil {
				close(f.got)
				return
			}
			var m map[string]any
			json.Unmarshal(data, &m)
			f.got <- m
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRealtime) send0(ws *websocket.Conn, v any) {
	b, _ := json.Marshal(v)
	ws.Write(context.Background(), websocket.MessageText, b)
}

func (f *fakeRealtime) send(v any) {
	f.mu.Lock()
	ws := f.ws
	f.mu.Unlock()
	f.send0(ws, v)
}

// next returns the next event the client sent, skipping audio unless asked for it.
func (f *fakeRealtime) next(t *testing.T, wantAudio bool) map[string]any {
	t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case m, ok := <-f.got:
			if !ok {
				t.Fatal("the client closed the session")
			}
			if (m["type"] == "input_audio_buffer.append") != wantAudio {
				continue
			}
			return m
		case <-timeout:
			t.Fatal("no event from the client")
		}
	}
}

// quiet asserts the client sends nothing (but audio) for a moment.
func (f *fakeRealtime) quiet(t *testing.T) {
	t.Helper()
	timeout := time.After(150 * time.Millisecond)
	for {
		select {
		case m := <-f.got:
			if m["type"] != "input_audio_buffer.append" {
				t.Errorf("unexpected event %v", m)
			}
		case <-timeout:
			return
		}
	}
}

func (f *fakeRealtime) engine() *Engine {
	return &Engine{APIKey: "key", BaseURL: "ws" + strings.TrimPrefix(f.srv.URL, "http")}
}

func next(t *testing.T, s *Session) voice.Event {
	t.Helper()
	select {
	case e, ok := <-s.Events():
		if !ok {
			t.Fatalf("events closed: %v", s.Err())
		}
		return e
	case <-time.After(3 * time.Second):
		t.Fatal("no event")
		return nil
	}
}

func noEvent(t *testing.T, s *Session) {
	t.Helper()
	select {
	case e := <-s.Events():
		t.Errorf("unexpected event %#v", e)
	case <-time.After(100 * time.Millisecond):
	}
}

func fill(n int, v int16) []int16 {
	s := make([]int16, n)
	for i := range s {
		s[i] = v
	}
	return s
}

var brief = voice.Config{Owner: "Marco", Brief: prompts.Brief{Objective: "Una margherita da asporto."}}

func dial(t *testing.T, e *Engine) *Session {
	t.Helper()
	vs, err := e.Dial(context.Background(), brief)
	if err != nil {
		t.Fatal(err)
	}
	s := vs.(*Session)
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSession(t *testing.T) {
	f := newFake(t)
	s := dial(t, f.engine())

	// The session settings: instructions, µ-law both ways, the voice, VAD, transcription, tools.
	sess := f.session
	in := sess["audio"].(map[string]any)["input"].(map[string]any)
	out := sess["audio"].(map[string]any)["output"].(map[string]any)
	if f.model != DefaultModel || sess["type"] != "realtime" ||
		!strings.Contains(sess["instructions"].(string), "You are a personal assistant phoning on behalf of Marco") ||
		!strings.Contains(sess["instructions"].(string), "Together with your goodbye, in the same turn") ||
		!strings.Contains(sess["instructions"].(string), "Una margherita da asporto.") {
		t.Errorf("session = %v (model %s)", sess, f.model)
	}
	if in["format"].(map[string]any)["type"] != "audio/pcmu" || out["format"].(map[string]any)["type"] != "audio/pcmu" ||
		out["voice"] != "marin" || in["turn_detection"].(map[string]any)["type"] != "semantic_vad" ||
		in["transcription"].(map[string]any)["language"] != "it" {
		t.Errorf("audio settings = %v / %v", in, out)
	}
	tools := sess["tools"].([]any)
	if len(tools) != 2 || sess["tool_choice"] != "auto" {
		t.Fatalf("tools = %v", tools)
	}
	for i, name := range []string{"report_outcome", "end_call"} {
		tl := tools[i].(map[string]any)
		if tl["type"] != "function" || tl["name"] != name || tl["parameters"].(map[string]any)["type"] != "object" {
			t.Errorf("tool %d = %v", i, tl)
		}
	}

	// The callee's audio goes out as µ-law.
	pcm := []int16{0, 1000, -1000, 8000}
	s.SendAudio(pcm)
	if m := f.next(t, true); m["audio"] != base64.StdEncoding.EncodeToString(audio.PCMToMulaw(nil, pcm)) {
		t.Errorf("audio append = %v", m)
	}

	// Cues: a system message for context, a user message and a response when the agent must act.
	s.Cue("[Mancano 30 secondi]", false)
	m := f.next(t, false)
	item := m["item"].(map[string]any)
	if m["type"] != "conversation.item.create" || item["role"] != "system" ||
		item["content"].([]any)[0].(map[string]any)["text"] != "[Mancano 30 secondi]" {
		t.Errorf("wrap-up cue sent as %v", m)
	}
	f.quiet(t) // no response asked for
	s.Cue("[Inizia]", true)
	if m := f.next(t, false); m["item"].(map[string]any)["role"] != "user" {
		t.Errorf("opening cue sent as %v", m)
	}
	if m := f.next(t, false); m["type"] != "response.create" {
		t.Errorf("after the opening cue: %v", m)
	}

	// The callee speaks with nothing playing: not an interruption.
	f.send(map[string]any{"type": "input_audio_buffer.speech_started", "audio_start_ms": 100, "item_id": "i1"})
	f.send(map[string]any{"type": "conversation.item.input_audio_transcription.completed", "item_id": "i1", "transcript": " Pronto? "})
	if e := next(t, s); e != (voice.Transcript{Speaker: voice.SpeakerCallee, Text: "Pronto?"}) {
		t.Errorf("callee transcript = %#v", e)
	}

	// A response: audio, its transcript, the end of its audio.
	mu := audio.PCMToMulaw(nil, fill(160, 1000))
	f.send(map[string]any{"type": "response.output_audio.delta", "item_id": "a1", "delta": base64.StdEncoding.EncodeToString(mu)})
	f.send(map[string]any{"type": "response.output_audio_transcript.done", "item_id": "a1", "transcript": "Buonasera, vorrei una pizza."})
	f.send(map[string]any{"type": "response.output_audio.done", "item_id": "a1"})
	if e := next(t, s).(voice.AudioOut); len(e.PCM) != 160 || e.PCM[0] != audio.MulawToPCM(nil, mu)[0] {
		t.Errorf("audio = %d samples", len(e.PCM))
	}
	if e := next(t, s); e != (voice.Transcript{Speaker: voice.SpeakerAgent, Text: "Buonasera, vorrei una pizza."}) {
		t.Errorf("agent transcript = %#v", e)
	}
	if e := next(t, s); e != (voice.TurnEnd{}) {
		t.Errorf("after the audio: %#v", e)
	}

	// The callee talks over it: one Interrupted.
	f.send(map[string]any{"type": "input_audio_buffer.speech_started", "item_id": "i2"})
	f.send(map[string]any{"type": "input_audio_buffer.speech_started", "item_id": "i3"})
	if e := next(t, s); e != (voice.Interrupted{}) {
		t.Errorf("barge-in = %#v", e)
	}
	noEvent(t, s)

	// Tools: reported at once, answered once the response is done, then a new response.
	f.send(map[string]any{"type": "response.function_call_arguments.done", "call_id": "c1", "name": "report_outcome",
		"arguments": `{"result":"success","summary":"Pizza per le dieci."}`})
	f.send(map[string]any{"type": "response.function_call_arguments.done", "call_id": "c2", "name": "transfer", "arguments": `{}`})
	if e := next(t, s); e != (voice.Outcome{Result: voice.ResultSuccess, Summary: "Pizza per le dieci."}) {
		t.Errorf("outcome = %#v", e)
	}
	f.quiet(t) // nothing sent while the response is active
	f.send(map[string]any{"type": "response.done", "response": map[string]any{"status": "completed", "usage": map[string]any{
		"input_token_details":  map[string]any{"audio_tokens": 100, "text_tokens": 50, "cached_tokens": 10},
		"output_token_details": map[string]any{"audio_tokens": 80, "text_tokens": 20},
	}}})
	if e := next(t, s); e.(voice.Usage)["input_audio_tokens"] != 100 || e.(voice.Usage)["output_text_tokens"] != 20 {
		t.Errorf("usage = %#v", e)
	}
	for _, want := range []struct {
		call string
		ok   bool
	}{{"c1", true}, {"c2", false}} {
		m := f.next(t, false)
		it := m["item"].(map[string]any)
		var r struct {
			OK   bool   `json:"ok"`
			Next string `json:"next"`
		}
		json.Unmarshal([]byte(it["output"].(string)), &r)
		if it["type"] != "function_call_output" || it["call_id"] != want.call || r.OK != want.ok {
			t.Errorf("tool output = %v", m)
		}
	}
	if m := f.next(t, false); m["type"] != "response.create" {
		t.Errorf("after the tool outputs: %v", m)
	}

	f.send(map[string]any{"type": "response.function_call_arguments.done", "call_id": "c3", "name": "end_call", "arguments": `{"reason":"fatto"}`})
	if e := next(t, s); e != (voice.EndCall{Reason: "fatto"}) {
		t.Errorf("end_call = %#v", e)
	}
	// After end_call its result goes in, but no new response: the goodbye is already said.
	f.send(map[string]any{"type": "response.done", "response": map[string]any{"status": "completed"}})
	if m := f.next(t, false); m["item"].(map[string]any)["call_id"] != "c3" {
		t.Errorf("end_call output = %v", m)
	}
	f.quiet(t)

	// End closes the session normally.
	s.End()
	for range s.Events() {
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := s.SendAudio(pcm); !errors.Is(err, voice.ErrClosed) {
		t.Errorf("SendAudio after End: %v", err)
	}
}

func TestServerCloses(t *testing.T) {
	f := newFake(t)
	s := dial(t, f.engine())
	f.send(map[string]any{"type": "error", "error": map[string]any{"code": "server_error", "message": "boom"}})
	f.mu.Lock()
	f.ws.Close(websocket.StatusInternalError, "bye")
	f.mu.Unlock()
	for range s.Events() {
	}
	if err := s.Err(); err == nil || !strings.Contains(err.Error(), "server_error: boom") {
		t.Errorf("Err = %v", err)
	}

	f = newFake(t)
	s = dial(t, f.engine())
	f.mu.Lock()
	f.ws.Close(websocket.StatusNormalClosure, "bye")
	f.mu.Unlock()
	for range s.Events() {
	}
	if err := s.Err(); err != nil {
		t.Errorf("Err after a normal close = %v", err)
	}
}

func TestDialFailures(t *testing.T) {
	f := newFake(t)
	e := f.engine()
	e.APIKey = "wrong"
	if _, err := e.Dial(context.Background(), brief); err == nil {
		t.Error("wrong key accepted")
	}
	f.refuse = true
	if _, err := f.engine().Dial(context.Background(), brief); err == nil || !strings.Contains(err.Error(), "voice not supported") {
		t.Errorf("refused session: %v", err)
	}
}

func TestPrepare(t *testing.T) {
	if _, err := (&Engine{}).Prepare(brief); err == nil {
		t.Error("no API key accepted")
	}
	got, err := (&Engine{APIKey: "k"}).Prepare(brief)
	if err != nil || got.Voice != DefaultVoice {
		t.Errorf("default voice = %+v, %v", got, err)
	}
	c := brief
	c.Voice = "cedar"
	if got, err := (&Engine{APIKey: "k"}).Prepare(c); err != nil || got.Voice != "cedar" {
		t.Errorf("cedar = %+v, %v", got, err)
	}
	c.Voice = "nova2"
	if _, err := (&Engine{APIKey: "k"}).Prepare(c); err == nil {
		t.Error("unknown voice accepted without a gender")
	}
	male := false
	if _, err := (&Engine{APIKey: "k", Female: &male}).Prepare(c); err != nil {
		t.Errorf("unknown voice with a gender: %v", err)
	}
}
