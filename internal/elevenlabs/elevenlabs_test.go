package elevenlabs

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

// fakeAgent is ElevenLabs: the signed-URL endpoint and the conversation WebSocket.
type fakeAgent struct {
	t             *testing.T
	srv           *httptest.Server
	input, output string // the agent's audio formats

	mu   sync.Mutex
	init map[string]any
	ws   *websocket.Conn
	got  chan map[string]any // what the client sent after the initiation
	up   chan struct{}       // the conversation has started
}

func newFakeAgent(t *testing.T, input, output string) *fakeAgent {
	f := &fakeAgent{t: t, input: input, output: output, got: make(chan map[string]any, 1024), up: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/convai/conversation/get-signed-url", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("xi-api-key") != "key" || r.URL.Query().Get("agent_id") != "agent" {
			w.WriteHeader(401)
			w.Write([]byte(`{"detail":"invalid api key"}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]string{
			"signed_url": "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/v1/convai/conversation?agent_id=agent&conversation_signature=sig",
		})
	})
	mux.HandleFunc("/v1/convai/conversation", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("conversation_signature") != "sig" {
			w.WriteHeader(403)
			return
		}
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ws.SetReadLimit(4 << 20)
		ctx := context.Background()
		_, data, err := ws.Read(ctx)
		if err != nil {
			return
		}
		var init map[string]any
		json.Unmarshal(data, &init)
		f.mu.Lock()
		f.init, f.ws = init, ws
		f.mu.Unlock()
		f.send(map[string]any{"type": "conversation_initiation_metadata", "conversation_initiation_metadata_event": map[string]any{
			"conversation_id": "conv_1", "user_input_audio_format": f.input, "agent_output_audio_format": f.output,
		}})
		close(f.up)
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
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAgent) send(v any) {
	b, _ := json.Marshal(v)
	f.mu.Lock()
	ws := f.ws
	f.mu.Unlock()
	if err := ws.Write(context.Background(), websocket.MessageText, b); err != nil {
		f.t.Errorf("fake agent: %v", err)
	}
}

// next returns the next message the client sent, skipping audio unless asked for it.
func (f *fakeAgent) next(t *testing.T, wantAudio bool) map[string]any {
	t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case m, ok := <-f.got:
			if !ok {
				t.Fatal("the client closed the conversation")
			}
			if _, isAudio := m["user_audio_chunk"]; isAudio != wantAudio {
				continue
			}
			return m
		case <-timeout:
			t.Fatal("no message from the client")
		}
	}
}

func (f *fakeAgent) audio(pcm []int16, final bool) {
	var b []byte
	if f.output == "ulaw_8000" {
		b = audio.PCMToMulaw(nil, pcm)
	} else {
		b = audio.PCMToLE16(nil, pcm)
	}
	ev := map[string]any{"audio_base_64": base64.StdEncoding.EncodeToString(b), "event_id": 1}
	if final {
		ev["is_final"] = true
	}
	f.send(map[string]any{"type": "audio", "audio_event": ev})
}

func (f *fakeAgent) engine() *Engine {
	return &Engine{APIKey: "key", AgentID: "agent", BaseURL: f.srv.URL, Female: true, TurnQuiet: 150 * time.Millisecond}
}

func fill(n int, v int16) []int16 {
	s := make([]int16, n)
	for i := range s {
		s[i] = v
	}
	return s
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

func dial(t *testing.T, f *fakeAgent, e *Engine, c voice.Config) *Session {
	t.Helper()
	vs, err := e.Dial(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	s := vs.(*Session)
	t.Cleanup(func() { s.Close() })
	return s
}

func TestConversation(t *testing.T) {
	f := newFakeAgent(t, "ulaw_8000", "ulaw_8000")
	e := f.engine()
	e.Voice = "voice_it"
	s := dial(t, f, e, voice.Config{Owner: "Marco", Brief: prompts.Brief{Objective: "Una margherita da asporto."}})

	// The overrides: the rendered prompt and the voice, nothing else. Language and first message are
	// the agent's own (Italian, empty), and every override needs a toggle on the agent.
	agent := f.init["conversation_config_override"].(map[string]any)["agent"].(map[string]any)
	prompt := agent["prompt"].(map[string]any)["prompt"].(string)
	if f.init["type"] != "conversation_initiation_client_data" ||
		!strings.Contains(prompt, "Sei un'assistente che telefona per conto di Marco") || !strings.Contains(prompt, "Insieme al saluto, nella stessa battuta") ||
		!strings.Contains(prompt, "Una margherita da asporto.") {
		t.Errorf("initiation = %v", f.init)
	}
	for _, field := range []string{"first_message", "language"} {
		if _, ok := agent[field]; ok {
			t.Errorf("%s overridden", field)
		}
	}
	if _, ok := agent["prompt"].(map[string]any)["llm"]; ok {
		t.Error("llm overridden although none is set")
	}
	if tts := f.init["conversation_config_override"].(map[string]any)["tts"].(map[string]any); tts["voice_id"] != "voice_it" {
		t.Errorf("tts = %v", tts)
	}
	if s.ConversationID() != "conv_1" {
		t.Errorf("conversation id = %s", s.ConversationID())
	}

	// The callee's audio goes out as µ-law, unchanged in rate.
	pcm := []int16{0, 1000, -1000, 8000}
	if err := s.SendAudio(pcm); err != nil {
		t.Fatal(err)
	}
	chunk, _ := base64.StdEncoding.DecodeString(f.next(t, true)["user_audio_chunk"].(string))
	if string(chunk) != string(audio.PCMToMulaw(nil, pcm)) {
		t.Errorf("audio chunk = %v", chunk)
	}

	// Cues: context that doesn't interrupt, or a user turn when the agent must act now.
	s.Cue("[Mancano 30 secondi]", false)
	if m := f.next(t, false); m["type"] != "contextual_update" || m["text"] != "[Mancano 30 secondi]" {
		t.Errorf("wrap-up cue sent as %v", m)
	}
	s.Cue("[Inizia]", true)
	if m := f.next(t, false); m["type"] != "user_message" || m["text"] != "[Inizia]" {
		t.Errorf("opening cue sent as %v", m)
	}

	// Pings get pongs.
	f.send(map[string]any{"type": "ping", "ping_event": map[string]any{"event_id": 7, "ping_ms": 40}})
	if m := f.next(t, false); m["type"] != "pong" || m["event_id"] != 7.0 {
		t.Errorf("pong = %v", m)
	}

	// A turn: its text first, then the audio, the last chunk marked final.
	f.send(map[string]any{"type": "agent_response", "agent_response_event": map[string]any{"agent_response": " Buonasera, vorrei una pizza. "}})
	f.audio(fill(160, 1000), false)
	f.audio(fill(160, 2000), true)
	want := audio.MulawToPCM(nil, audio.PCMToMulaw(nil, fill(160, 1000)))
	if e := next(t, s); e.(voice.AudioOut).PCM[0] != want[0] || len(e.(voice.AudioOut).PCM) != 160 {
		t.Errorf("audio = %#v", e)
	}
	next(t, s) // the second chunk
	if e := next(t, s); e != (voice.Transcript{Speaker: voice.SpeakerAgent, Text: "Buonasera, vorrei una pizza."}) {
		t.Errorf("agent transcript = %#v", e)
	}
	if e := next(t, s); e != (voice.TurnEnd{}) {
		t.Errorf("after the final chunk: %#v", e)
	}

	f.send(map[string]any{"type": "user_transcript", "user_transcription_event": map[string]any{"user_transcript": "Per che ora?"}})
	if e := next(t, s); e != (voice.Transcript{Speaker: voice.SpeakerCallee, Text: "Per che ora?"}) {
		t.Errorf("callee transcript = %#v", e)
	}

	// Barge-in: one Interrupted, and the transcript is what was actually said.
	f.send(map[string]any{"type": "agent_response", "agent_response_event": map[string]any{"agent_response": "Per le dieci, e se possibile anche"}})
	f.audio(fill(160, 500), false)
	f.send(map[string]any{"type": "interruption", "interruption_event": map[string]any{"reason": "user"}})
	f.send(map[string]any{"type": "agent_response_correction", "agent_response_correction_event": map[string]any{
		"original_agent_response": "Per le dieci, e se possibile anche", "corrected_agent_response": "Per le dieci",
	}})
	next(t, s) // audio
	if e := next(t, s); e != (voice.Interrupted{}) {
		t.Errorf("after the interruption: %#v", e)
	}
	if e := next(t, s); e != (voice.Transcript{Speaker: voice.SpeakerAgent, Text: "Per le dieci"}) {
		t.Errorf("corrected transcript = %#v", e)
	}

	// Client tools: answered with the next closing step, reported when valid.
	f.send(map[string]any{"type": "client_tool_call", "client_tool_call": map[string]any{
		"tool_name": "transfer", "tool_call_id": "t0", "parameters": map[string]any{}, "expects_response": true,
	}})
	if m := f.next(t, false); m["type"] != "client_tool_result" || m["tool_call_id"] != "t0" || m["is_error"] != true {
		t.Errorf("result for an unknown tool = %v", m)
	}
	f.send(map[string]any{"type": "client_tool_call", "client_tool_call": map[string]any{
		"tool_name": "report_outcome", "tool_call_id": "t1", "expects_response": true,
		"parameters": map[string]any{"result": "success", "summary": "Pizza per le dieci."},
	}})
	if m := f.next(t, false); m["tool_call_id"] != "t1" || m["is_error"] != false || m["result"] != `{"ok":true}` {
		t.Errorf("report_outcome result = %v", m)
	}
	if e := next(t, s); e != (voice.Outcome{Result: voice.ResultSuccess, Summary: "Pizza per le dieci."}) {
		t.Errorf("outcome = %#v", e)
	}
	f.send(map[string]any{"type": "client_tool_call", "client_tool_call": map[string]any{
		"tool_name": "end_call", "tool_call_id": "t2", "parameters": map[string]any{"reason": "fatto"},
	}})
	f.next(t, false)
	if e := next(t, s); e != (voice.EndCall{Reason: "fatto"}) {
		t.Errorf("end_call = %#v", e)
	}

	// A goodbye without is_final: the turn ends when the audio stops.
	f.send(map[string]any{"type": "agent_response", "agent_response_event": map[string]any{"agent_response": "Grazie, buonasera!"}})
	f.audio(fill(160, 300), false)
	next(t, s)
	start := time.Now()
	if e := next(t, s); e != (voice.Transcript{Speaker: voice.SpeakerAgent, Text: "Grazie, buonasera!"}) {
		t.Errorf("goodbye transcript = %#v", e)
	}
	if e := next(t, s); e != (voice.TurnEnd{}) || time.Since(start) > time.Second {
		t.Errorf("no TurnEnd after the audio stopped: %#v after %v", e, time.Since(start))
	}

	// End closes the conversation normally.
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

func TestPCMFormats(t *testing.T) {
	f := newFakeAgent(t, "pcm_16000", "pcm_24000")
	s := dial(t, f, f.engine(), voice.Config{Owner: "Marco", Brief: prompts.Brief{Objective: "x"}})
	if err := s.SendAudio(fill(160, 1000)); err != nil { // 20 ms
		t.Fatal(err)
	}
	b, _ := base64.StdEncoding.DecodeString(f.next(t, true)["user_audio_chunk"].(string))
	up := audio.PCMFromLE16(nil, b)
	if len(up) != 320 || up[100] != 1000 {
		t.Errorf("16 kHz chunk: %d samples, sample %d", len(up), up[100])
	}
	f.audio(fill(480, 2000), true) // 20 ms at 24 kHz
	if pcm := next(t, s).(voice.AudioOut).PCM; len(pcm) != 160 || pcm[80] != 2000 {
		t.Errorf("8 kHz audio: %d samples, sample %d", len(pcm), pcm[80])
	}
}

func TestServerCloses(t *testing.T) {
	for _, c := range []struct {
		code    websocket.StatusCode
		failure bool
	}{{websocket.StatusNormalClosure, false}, {websocket.StatusInternalError, true}} {
		f := newFakeAgent(t, "ulaw_8000", "ulaw_8000")
		s := dial(t, f, f.engine(), voice.Config{Owner: "Marco", Brief: prompts.Brief{Objective: "x"}})
		<-f.up
		f.mu.Lock()
		f.ws.Close(c.code, "bye")
		f.mu.Unlock()
		for range s.Events() {
		}
		if err := s.Err(); (err != nil) != c.failure {
			t.Errorf("close %d: Err = %v", c.code, err)
		}
	}
}

func TestDialFailures(t *testing.T) {
	f := newFakeAgent(t, "ulaw_8000", "mp3_44100")
	c := voice.Config{Owner: "Marco", Brief: prompts.Brief{Objective: "x"}}

	e := f.engine()
	e.APIKey = "wrong"
	if _, err := e.Dial(context.Background(), c); err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Errorf("wrong key: %v", err)
	}
	if _, err := f.engine().Dial(context.Background(), c); err == nil || !strings.Contains(err.Error(), "mp3_44100") {
		t.Errorf("unsupported format: %v", err)
	}
	for _, bad := range []*Engine{{AgentID: "agent"}, {APIKey: "key"}} {
		if _, err := bad.Prepare(c); err == nil {
			t.Errorf("Prepare(%+v) accepted", bad)
		}
	}
	if _, err := f.engine().Prepare(voice.Config{Owner: "", Brief: c.Brief}); err == nil {
		t.Error("Prepare accepted an empty owner")
	}
	if got, err := f.engine().Prepare(c); err != nil || got.Voice != "" {
		t.Errorf("Prepare without a voice = %+v, %v", got, err)
	}
}

func TestResample(t *testing.T) {
	for _, c := range []struct{ from, to, in, out int }{
		{8000, 16000, 160, 320}, {16000, 8000, 320, 160}, {24000, 8000, 480, 160},
		{48000, 8000, 960, 160}, {44100, 8000, 882, 160}, {22050, 8000, 441, 160}, {8000, 8000, 160, 160},
	} {
		got := resample(fill(c.in, 1234), c.from, c.to)
		if len(got) != c.out {
			t.Errorf("%d→%d: %d samples, want %d", c.from, c.to, len(got), c.out)
			continue
		}
		if got[len(got)/2] != 1234 {
			t.Errorf("%d→%d: a constant became %d", c.from, c.to, got[len(got)/2])
		}
	}
	for _, s := range []string{"ulaw_8000", "pcm_8000", "pcm_16000", "pcm_22050", "pcm_24000", "pcm_44100", "pcm_48000"} {
		if _, err := parseFormat(s); err != nil {
			t.Errorf("parseFormat(%s): %v", s, err)
		}
	}
	for _, s := range []string{"mp3_44100", "pcm_11025", "ulaw_16000", ""} {
		if _, err := parseFormat(s); err == nil {
			t.Errorf("parseFormat(%s) accepted", s)
		}
	}
}
