package sonic

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/mfbonfigli/dialout/internal/voice"
	"github.com/mfbonfigli/dialout/prompts"
)

func encodeJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// fakeStream records what the session sends and lets the test play Sonic's side.
type fakeStream struct {
	mu     sync.Mutex
	sent   []map[string]map[string]any
	out    chan types.InvokeModelWithBidirectionalStreamOutput
	once   sync.Once
	err    error
	failAt int // fail the Nth Send (1-based); 0 = never
}

func newFake() *fakeStream {
	return &fakeStream{out: make(chan types.InvokeModelWithBidirectionalStreamOutput, 64)}
}

func (f *fakeStream) Send(_ context.Context, ev types.InvokeModelWithBidirectionalStreamInput) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAt > 0 && len(f.sent)+1 == f.failAt {
		return errors.New("broken pipe")
	}
	chunk := ev.(*types.InvokeModelWithBidirectionalStreamInputMemberChunk)
	var m map[string]map[string]any
	if err := json.Unmarshal(chunk.Value.Bytes, &m); err != nil {
		panic(err)
	}
	f.sent = append(f.sent, m)
	return nil
}

func (f *fakeStream) Events() <-chan types.InvokeModelWithBidirectionalStreamOutput { return f.out }
func (f *fakeStream) Close() error                                                  { f.end(); return nil }
func (f *fakeStream) Err() error                                                    { return f.err }
func (f *fakeStream) end()                                                          { f.once.Do(func() { close(f.out) }) }

// emit plays one output event from Sonic.
func (f *fakeStream) emit(name string, body map[string]any) {
	b, _ := json.Marshal(map[string]any{"event": map[string]any{name: body}})
	f.out <- &types.InvokeModelWithBidirectionalStreamOutputMemberChunk{Value: types.BidirectionalOutputPayloadPart{Bytes: b}}
}

// events returns the names and bodies of the input events sent so far.
func (f *fakeStream) events() ([]string, []map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var names []string
	var bodies []map[string]any
	for _, m := range f.sent {
		for k, v := range m["event"] {
			names = append(names, k)
			bodies = append(bodies, v.(map[string]any))
		}
	}
	return names, bodies
}

func (f *fakeStream) waitSent(t *testing.T, n int) ([]string, []map[string]any) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		names, bodies := f.events()
		if len(names) >= n || time.Now().After(deadline) {
			return names, bodies
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// testPrompt is the built-in prompt with a short system prompt.
var testPrompt = func() *prompts.Rendered {
	r, err := prompts.Builtin().Render(prompts.ToolsThenGoodbye, prompts.Vars{Owner: "Marco", Brief: prompts.Brief{Objective: "x"}})
	if err != nil {
		panic(err)
	}
	r.System = "Sei un assistente."
	return r
}()

func cfg() Config { return Config{Prompt: testPrompt} }

func TestStartSequenceAndInput(t *testing.T) {
	f := newFake()
	s, err := Start(context.Background(), f, cfg())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SendAudio([]int16{0x0201, 0x0403}); err != nil {
		t.Fatal(err)
	}
	if err := s.Cue("[inizia]", true); err != nil {
		t.Fatal(err)
	}
	if err := s.End(); err != nil {
		t.Fatal(err)
	}
	if err := s.End(); err != nil { // idempotent, sends nothing more
		t.Fatal(err)
	}
	if err := s.SendAudio([]int16{0}); !errors.Is(err, ErrClosed) {
		t.Errorf("SendAudio after End: %v, want ErrClosed", err)
	}

	names, bodies := f.events()
	want := []string{
		"sessionStart", "promptStart",
		"contentStart", "textInput", "contentEnd", // system prompt
		"contentStart", // audio block
		"audioInput",
		"contentStart", "textInput", "contentEnd", // cue
		"contentEnd", "promptEnd", "sessionEnd", // closing sequence
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("events:\n got %v\nwant %v", names, want)
	}

	ss := bodies[0]
	if got := ss["turnDetectionConfiguration"].(map[string]any)["endpointingSensitivity"]; got != "MEDIUM" {
		t.Errorf("endpointing = %v", got)
	}

	ps := bodies[1]
	prompt := ps["promptName"].(string)
	out := ps["audioOutputConfiguration"].(map[string]any)
	if out["voiceId"] != "tiffany" || out["sampleRateHertz"] != 8000.0 || out["mediaType"] != "audio/lpcm" {
		t.Errorf("audio output config = %v", out)
	}
	tools := ps["toolConfiguration"].(map[string]any)["tools"].([]any)
	var toolNames []string
	for _, tl := range tools {
		spec := tl.(map[string]any)["toolSpec"].(map[string]any)
		toolNames = append(toolNames, spec["name"].(string))
		var schema map[string]any
		if err := json.Unmarshal([]byte(spec["inputSchema"].(map[string]any)["json"].(string)), &schema); err != nil {
			t.Errorf("tool %v: schema is not JSON: %v", spec["name"], err)
		}
	}
	if strings.Join(toolNames, ",") != "report_outcome,end_call" {
		t.Errorf("tools = %v", toolNames)
	}

	for i, b := range bodies[2:] {
		if names[i+2] != "sessionEnd" && b["promptName"] != prompt {
			t.Errorf("event %d (%s) has promptName %v, want %s", i+2, names[i+2], b["promptName"], prompt)
		}
	}

	sys, sysText := bodies[2], bodies[3]
	if sys["role"] != "SYSTEM" || sys["type"] != "TEXT" || sys["interactive"] != false {
		t.Errorf("system contentStart = %v", sys)
	}
	if sysText["content"] != "Sei un assistente." || sysText["contentName"] != sys["contentName"] || bodies[4]["contentName"] != sys["contentName"] {
		t.Errorf("system prompt block = %v / %v", sysText, bodies[4])
	}

	au := bodies[5]
	audioName := au["contentName"]
	in := au["audioInputConfiguration"].(map[string]any)
	if au["type"] != "AUDIO" || au["role"] != "USER" || au["interactive"] != true || in["sampleRateHertz"] != 8000.0 {
		t.Errorf("audio contentStart = %v", au)
	}
	if bodies[6]["contentName"] != audioName || bodies[6]["content"] != base64.StdEncoding.EncodeToString([]byte{1, 2, 3, 4}) {
		t.Errorf("audioInput = %v", bodies[6])
	}

	cue := bodies[7]
	if cue["role"] != "USER" || cue["interactive"] != true || bodies[8]["content"] != "[inizia]" || cue["contentName"] == audioName {
		t.Errorf("cue = %v / %v", cue, bodies[8])
	}

	if bodies[10]["contentName"] != audioName {
		t.Errorf("closing contentEnd is for %v, want the audio block %v", bodies[10]["contentName"], audioName)
	}

	f.end()
	if err := s.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func textStart(id, role, stage string) map[string]any {
	return map[string]any{
		"contentId": id, "type": "TEXT", "role": role,
		"additionalModelFields": `{"generationStage":"` + stage + `"}`,
	}
}

func collect(t *testing.T, s *Session) []voice.Event {
	t.Helper()
	var got []voice.Event
	timeout := time.After(2 * time.Second)
	for {
		select {
		case e, ok := <-s.Events():
			if !ok {
				return got
			}
			got = append(got, e)
		case <-timeout:
			t.Fatal("Events not closed")
		}
	}
}

func TestOutputEvents(t *testing.T) {
	f := newFake()
	s, err := Start(context.Background(), f, cfg())
	if err != nil {
		t.Fatal(err)
	}
	sent, _ := f.events()
	opening := len(sent)

	f.emit("completionStart", map[string]any{"completionId": "x"})
	// The callee's words.
	f.emit("contentStart", textStart("c1", "USER", "FINAL"))
	f.emit("textOutput", map[string]any{"contentId": "c1", "content": " Pronto?\n"})
	f.emit("contentEnd", map[string]any{"contentId": "c1", "type": "TEXT", "stopReason": "END_TURN"})
	// The plan (speculative) is not a transcript.
	f.emit("contentStart", textStart("c2", "ASSISTANT", "SPECULATIVE"))
	f.emit("textOutput", map[string]any{"contentId": "c2", "content": "Buongiorno, chiamo per"})
	f.emit("contentEnd", map[string]any{"contentId": "c2", "type": "TEXT", "stopReason": "PARTIAL_TURN"})
	// Audio, then a barge-in signalled twice: one Interrupted.
	f.emit("contentStart", map[string]any{"contentId": "c3", "type": "AUDIO", "role": "ASSISTANT"})
	f.emit("audioOutput", map[string]any{"contentId": "c3", "content": base64.StdEncoding.EncodeToString([]byte{9, 8})})
	f.emit("contentStart", textStart("c4", "ASSISTANT", "FINAL"))
	f.emit("textOutput", map[string]any{"contentId": "c4", "content": `{ "interrupted" : true }`})
	f.emit("contentEnd", map[string]any{"contentId": "c3", "type": "AUDIO", "stopReason": "INTERRUPTED"})
	// What was actually said.
	f.emit("textOutput", map[string]any{"contentId": "c4", "content": "Buongiorno,"})
	f.emit("textOutput", map[string]any{"contentId": "c4", "content": " \n "}) // blank: dropped
	f.emit("contentEnd", map[string]any{"contentId": "c4", "type": "TEXT", "stopReason": "INTERRUPTED"})
	// Tools: a bad call, then good ones.
	for i, tu := range []map[string]any{
		{"toolName": "report_outcome", "toolUseId": "t1", "content": `{"result":"maybe","summary":"boh"}`},
		{"toolName": "report_outcome", "toolUseId": "t2", "content": `{"result":"success","summary":" Tavolo prenotato. ","details":"Venerdì 20:00, 4 persone"}`},
		{"toolName": "transfer", "toolUseId": "t3", "content": `{}`},
		{"toolName": "end_call", "toolUseId": "t4", "content": map[string]any{"reason": "saluti fatti"}}, // object, not string
	} {
		id := "tool" + string(rune('a'+i))
		f.emit("contentStart", map[string]any{"contentId": id, "type": "TOOL", "role": "TOOL"})
		tu["contentId"] = id
		f.emit("toolUse", tu)
		f.emit("contentEnd", map[string]any{"contentId": id, "type": "TOOL", "stopReason": "TOOL_USE"})
	}
	// The goodbye: a partial block, then the end of the turn.
	f.emit("contentStart", map[string]any{"contentId": "c5", "type": "AUDIO", "role": "ASSISTANT"})
	f.emit("audioOutput", map[string]any{"contentId": "c5", "content": base64.StdEncoding.EncodeToString([]byte{7, 0})})
	f.emit("contentEnd", map[string]any{"contentId": "c5", "type": "AUDIO", "stopReason": "PARTIAL_TURN"})
	f.emit("contentStart", map[string]any{"contentId": "c6", "type": "AUDIO", "role": "ASSISTANT"})
	f.emit("audioOutput", map[string]any{"contentId": "c6", "content": base64.StdEncoding.EncodeToString([]byte{6, 0})})
	f.emit("contentEnd", map[string]any{"contentId": "c6", "type": "AUDIO", "stopReason": "END_TURN"})
	f.emit("usageEvent", map[string]any{"details": map[string]any{"total": map[string]any{
		"input":  map[string]any{"speechTokens": 100, "textTokens": 50},
		"output": map[string]any{"speechTokens": 80, "textTokens": 20},
	}}})
	f.emit("completionEnd", map[string]any{"stopReason": "END_TURN"})
	f.end()

	got := collect(t, s)
	want := []voice.Event{
		voice.Transcript{Speaker: voice.SpeakerCallee, Text: "Pronto?"},
		voice.AudioOut{PCM: []int16{0x0809}}, // the bytes 9, 8 as little-endian 16-bit
		voice.Interrupted{},
		voice.Transcript{Speaker: voice.SpeakerAgent, Text: "Buongiorno,"},
		voice.Outcome{Result: voice.ResultSuccess, Summary: "Tavolo prenotato.", Details: "Venerdì 20:00, 4 persone"},
		voice.EndCall{Reason: "saluti fatti"},
		voice.AudioOut{PCM: []int16{7}},
		voice.AudioOut{PCM: []int16{6}},
		voice.TurnEnd{},
		voice.Usage{"input_speech_tokens": 100, "input_text_tokens": 50, "output_speech_tokens": 80, "output_text_tokens": 20},
	}
	if len(got) != len(want) {
		t.Fatalf("events:\n got %#v\nwant %#v", got, want)
	}
	for i := range want {
		if g, w := encodeJSON(got[i]), encodeJSON(want[i]); string(g) != string(w) || typeOf(got[i]) != typeOf(want[i]) {
			t.Errorf("event %d = %T %s, want %T %s", i, got[i], g, want[i], w)
		}
	}

	// Every tool call got a result, matched by toolUseId; the invalid ones got an error.
	names, bodies := f.waitSent(t, opening+4*3)
	if err := s.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	results := map[string]string{}
	for i := opening; i+2 < len(names); i++ {
		if names[i] != "contentStart" || bodies[i]["type"] != "TOOL" {
			continue
		}
		cfg := bodies[i]["toolResultInputConfiguration"].(map[string]any)
		if bodies[i]["role"] != "TOOL" || names[i+1] != "toolResult" || names[i+2] != "contentEnd" ||
			bodies[i+1]["contentName"] != bodies[i]["contentName"] || bodies[i+2]["contentName"] != bodies[i]["contentName"] {
			t.Errorf("malformed tool result block at %d: %v", i, names[i:i+3])
		}
		results[cfg["toolUseId"].(string)] = bodies[i+1]["content"].(string)
	}
	for id, ok := range map[string]bool{"t1": false, "t2": true, "t3": false, "t4": true} {
		var r struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal([]byte(results[id]), &r); err != nil {
			t.Errorf("result for %s = %q: %v", id, results[id], err)
			continue
		}
		if r.OK != ok || (!ok && r.Error == "") {
			t.Errorf("result for %s = %q, want ok=%v", id, results[id], ok)
		}
	}
}

func typeOf(e voice.Event) string {
	switch e.(type) {
	case voice.AudioOut:
		return "AudioOut"
	case voice.TurnEnd:
		return "TurnEnd"
	case voice.Interrupted:
		return "Interrupted"
	case voice.Transcript:
		return "Transcript"
	case voice.Outcome:
		return "Outcome"
	case voice.EndCall:
		return "EndCall"
	case voice.Usage:
		return "Usage"
	}
	return "?"
}

func TestInterruptedOnlyAfterAudio(t *testing.T) {
	f := newFake()
	s, err := Start(context.Background(), f, cfg())
	if err != nil {
		t.Fatal(err)
	}
	// An interruption with nothing played yet is nothing to clear.
	f.emit("contentEnd", map[string]any{"contentId": "a", "type": "TEXT", "stopReason": "INTERRUPTED"})
	f.emit("audioOutput", map[string]any{"contentId": "b", "content": "AAA="})
	f.emit("contentEnd", map[string]any{"contentId": "b", "type": "AUDIO", "stopReason": "INTERRUPTED"})
	f.emit("audioOutput", map[string]any{"contentId": "c", "content": "AAA="})
	f.emit("textOutput", map[string]any{"contentId": "d", "content": `{"interrupted":true}`})
	f.end()
	var kinds []string
	for _, e := range collect(t, s) {
		kinds = append(kinds, typeOf(e))
	}
	if got := strings.Join(kinds, ","); got != "AudioOut,Interrupted,AudioOut,Interrupted" {
		t.Errorf("events = %s", got)
	}
	_ = s.Close()
}

func TestStreamError(t *testing.T) {
	f := newFake()
	f.err = errors.New("ModelStreamErrorException: boom")
	s, err := Start(context.Background(), f, cfg())
	if err != nil {
		t.Fatal(err)
	}
	f.end()
	collect(t, s)
	if err := s.Err(); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("Err = %v", err)
	}
	if err := s.Close(); err == nil {
		t.Error("Close returned no error after a stream failure")
	}
}

func TestMalformedOutput(t *testing.T) {
	f := newFake()
	s, err := Start(context.Background(), f, cfg())
	if err != nil {
		t.Fatal(err)
	}
	f.out <- &types.InvokeModelWithBidirectionalStreamOutputMemberChunk{Value: types.BidirectionalOutputPayloadPart{Bytes: []byte("not json")}}
	collect(t, s)
	if s.Err() == nil {
		t.Error("no error for a malformed event")
	}
	_ = s.Close()
}

func TestCloseWithoutDraining(t *testing.T) {
	f := newFake()
	s, err := Start(context.Background(), f, cfg())
	if err != nil {
		t.Fatal(err)
	}
	// More audio than the Events buffer holds, and nobody reading: the reader blocks in emit.
	for i := 0; i < 100; i++ {
		f.emit("audioOutput", map[string]any{"contentId": "a", "content": "AAA="})
	}
	time.Sleep(50 * time.Millisecond)
	done := make(chan error)
	go func() { done <- s.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked")
	}
	if err := s.Cue("x", true); !errors.Is(err, ErrClosed) {
		t.Errorf("Cue after Close: %v", err)
	}
}

func TestStartFailures(t *testing.T) {
	for _, c := range []Config{
		{},
		{Prompt: &prompts.Rendered{}},
		{Prompt: testPrompt, Voice: "joanna"}, // a Polly voice, not Sonic's
		{Prompt: testPrompt, Endpointing: "FAST"},
	} {
		if _, err := Start(context.Background(), newFake(), c); err == nil {
			t.Errorf("Start(%+v) accepted", c)
		}
	}

	f := newFake()
	f.failAt = 3
	if _, err := Start(context.Background(), f, cfg()); err == nil || !strings.Contains(err.Error(), "broken pipe") {
		t.Errorf("Start with a failing stream: %v", err)
	}

	c := Config{Prompt: testPrompt, Voice: "lorenzo", Endpointing: "LOW"}
	f = newFake()
	s, err := Start(context.Background(), f, c)
	if err != nil {
		t.Fatal(err)
	}
	_, bodies := f.events()
	if bodies[1]["audioOutputConfiguration"].(map[string]any)["voiceId"] != "lorenzo" ||
		bodies[0]["turnDetectionConfiguration"].(map[string]any)["endpointingSensitivity"] != "LOW" {
		t.Errorf("config not applied: %v / %v", bodies[0], bodies[1])
	}
	if d := time.Until(s.Deadline()); d < MaxStream-time.Minute || d > MaxStream {
		t.Errorf("deadline in %v", d)
	}
	_ = s.Close()
}
