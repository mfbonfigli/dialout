package sonic

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/mfbonfigli/dialout/internal/voice"
	"github.com/mfbonfigli/dialout/prompts"
)

// TestLive holds a short conversation with the real model: the opening cue over silence, the agent
// speaks, the callee answers (as text input), then the agent should call report_outcome and
// end_call and say goodbye. It checks the protocol (audio, transcripts, a tool round trip, the
// closing sequence); the model's closing manners are logged, with a compact trace of the raw output
// events. It needs AWS credentials allowed to invoke Nova 2 Sonic in eu-north-1 and costs a
// fraction of a cent, so it runs only on request:
//
//	SONIC_LIVE=1 go test -run TestLive -v ./internal/sonic/
func TestLive(t *testing.T) {
	if os.Getenv("SONIC_LIVE") == "" {
		t.Skip("set SONIC_LIVE=1 to talk to Nova 2 Sonic")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	awsCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion("eu-north-1"))
	if err != nil {
		t.Fatal(err)
	}
	owner := os.Getenv("DIALOUT_OWNER")
	if owner == "" {
		owner = "Marco"
	}
	p, err := prompts.Builtin().Render(prompts.ToolsThenGoodbye, prompts.Vars{Owner: owner, Female: true, Brief: prompts.Brief{
		Opening:   "Ciao, sono l'assistente di Marco: questa è una chiamata di prova.",
		Objective: "È una prova. Chiedi all'interlocutore se ti sente bene. Quando risponde, chiudi la chiamata.",
	}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := bedrockruntime.NewFromConfig(awsCfg).InvokeModelWithBidirectionalStream(ctx,
		&bedrockruntime.InvokeModelWithBidirectionalStreamInput{ModelId: aws.String(ModelID)})
	if err != nil {
		t.Fatal(err)
	}
	tr := newTrace(out.GetStream())
	defer func() { t.Logf("raw output events:\n%s", tr.String()) }()
	s, err := Start(ctx, tr, Config{Prompt: p})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// The callee is silent: stream 32 ms of silence at a time, as a phone line would.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		silence := make([]int16, 256)
		tick := time.NewTicker(32 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				if s.SendAudio(silence) != nil {
					return
				}
			}
		}
	}()
	if err := s.Cue(p.Cues.Opening, true); err != nil {
		t.Fatal(err)
	}

	var (
		audioBytes, afterEnd int
		agentLines           int
		outcome              *voice.Outcome
		endCall              bool
		usage                voice.Usage
		answered             bool
		settle               <-chan time.Time // after the outcome: wait this long for end_call and the goodbye
	)
	timeout := time.After(45 * time.Second)
loop:
	for {
		select {
		case e, ok := <-s.Events():
			if !ok {
				t.Fatalf("stream ended early: %v", s.Err())
			}
			switch e := e.(type) {
			case voice.AudioOut:
				audioBytes += 2 * len(e.PCM)
				if endCall {
					afterEnd += 2 * len(e.PCM)
				}
			case voice.Transcript:
				t.Logf("%s: %s", e.Speaker, e.Text)
				if e.Speaker == voice.SpeakerAgent {
					agentLines++
					if !answered {
						answered = true
						if err := s.Cue("Sì, ti sento benissimo. Grazie!", true); err != nil {
							t.Fatal(err)
						}
					}
				}
			case voice.Outcome:
				t.Logf("outcome: %+v", e)
				outcome = &e
				settle = time.After(8 * time.Second)
			case voice.EndCall:
				t.Logf("end_call: %q", e.Reason)
				endCall = true
			case voice.Interrupted:
				t.Log("interrupted")
			case voice.Usage:
				usage = e
			}
		case <-settle:
			break loop
		case <-timeout:
			t.Fatal("no outcome within 45 s")
		}
	}

	if err := s.End(); err != nil {
		t.Fatal(err)
	}
	drain := time.After(15 * time.Second)
	for open := true; open; {
		select {
		case _, open = <-s.Events():
		case <-drain:
			t.Fatal("Events not closed within 15 s of End")
		}
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}

	t.Logf("agent audio: %.1f s, %.1f s of it after end_call; end_call: %v; usage %+v",
		float64(audioBytes)/2/8000, float64(afterEnd)/2/8000, endCall, usage)
	if audioBytes == 0 || agentLines == 0 {
		t.Errorf("no agent speech: %d audio bytes, %d transcript lines", audioBytes, agentLines)
	}
	if outcome == nil || outcome.Result != voice.ResultSuccess {
		t.Errorf("outcome = %+v", outcome)
	}
}

// trace passes a stream through and keeps a one-line summary of each output event, with runs of
// audio and usage events collapsed.
type trace struct {
	Stream
	out   chan types.InvokeModelWithBidirectionalStreamOutput
	mu    sync.Mutex
	lines []string
	runs  int
}

func newTrace(s Stream) *trace {
	tr := &trace{Stream: s, out: make(chan types.InvokeModelWithBidirectionalStreamOutput, 64)}
	go func() {
		defer close(tr.out)
		for ev := range s.Events() {
			if c, ok := ev.(*types.InvokeModelWithBidirectionalStreamOutputMemberChunk); ok {
				tr.add(c.Value.Bytes)
			}
			tr.out <- ev
		}
	}()
	return tr
}

func (tr *trace) Events() <-chan types.InvokeModelWithBidirectionalStreamOutput { return tr.out }

func (tr *trace) add(b []byte) {
	var m map[string]map[string]map[string]any
	if json.Unmarshal(b, &m) != nil {
		return
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	for name, body := range m["event"] {
		if name == "audioOutput" || name == "usageEvent" {
			if n := len(tr.lines); n > 0 && strings.HasPrefix(tr.lines[n-1], "  "+name+" ×") {
				tr.runs++
				tr.lines[n-1] = fmt.Sprintf("  %s ×%d", name, tr.runs)
				return
			}
			tr.runs = 1
			tr.lines = append(tr.lines, fmt.Sprintf("  %s ×1", name))
			return
		}
		parts := []string{"  " + name}
		for _, k := range []string{"type", "role", "additionalModelFields", "stopReason", "toolName", "content"} {
			if v, ok := body[k]; ok {
				s := strings.ReplaceAll(fmt.Sprint(v), "\n", " ")
				if len(s) > 70 {
					s = s[:70] + "…"
				}
				parts = append(parts, k+"="+s)
			}
		}
		tr.lines = append(tr.lines, strings.Join(parts, " "))
	}
}

func (tr *trace) String() string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return strings.Join(tr.lines, "\n")
}
