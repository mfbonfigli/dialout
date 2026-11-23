package call

// The turn state and the barge-in gate: a cue must not go out in the middle of the agent's turn, and
// a two-word acknowledgement from the callee must not cut the agent off.

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/mfbonfigli/dialout/internal/audio"
	"github.com/mfbonfigli/dialout/internal/voice"
)

func TestCueWaitsForTheEndOfTheAgentsTurn(t *testing.T) {
	now := time.Now()
	b := &bridge{r: &run{t: Timing{AgentQuiet: 100 * time.Millisecond}}, outstanding: map[string]bool{}}
	b.lastAudio = now.Add(-5 * time.Second) // the audio stopped long ago
	b.turnOpen = true                       // but the engine hasn't ended the turn
	if b.silent(now) || b.cueSafe(now) {
		t.Error("a cue may go out in the middle of the agent's turn")
	}
	b.handle(context.Background(), voice.TurnEnd{}) // the engine ends it
	if b.turnOpen || !b.silent(now) || !b.cueSafe(now) {
		t.Error("no cue after the agent's turn ended")
	}
	// An engine that never marks the end of a turn must not block cues for ever.
	b.turnOpen = true
	b.lastAudio = now.Add(-turnIdle - time.Minute)
	if b.silent(now) {
		t.Error("a turn idle this long is still treated as open")
	}
}

func TestInterruptionEndsTheTurn(t *testing.T) {
	b := &bridge{r: &run{t: Timing{AgentQuiet: 100 * time.Millisecond}}, outstanding: map[string]bool{}}
	b.turnOpen = true
	b.handle(context.Background(), voice.Interrupted{})
	if b.turnOpen {
		t.Error("the agent's turn is still open after a barge-in")
	}
}

func TestBargeInGate(t *testing.T) {
	loud, quiet := fill(160, 3000), fill(160, 20)
	var g bargein

	// The agent is quiet: everything the callee says reaches it.
	if got := g.audio(loud, false); len(got) != 160 {
		t.Errorf("audio while the agent is quiet: %d samples", len(got))
	}
	// A backchannel over the agent: held back, then dropped when the callee stops.
	for i := 0; i < 3; i++ {
		if got := g.audio(loud, true); got != nil {
			t.Fatalf("backchannel frame %d went through", i)
		}
	}
	for i := 0; i < bargeInGap; i++ {
		if got := g.audio(quiet, true); got != nil {
			t.Fatalf("backchannel released after %d quiet frames", i)
		}
	}
	// A real interruption: the burst goes through in one piece, and nothing more is held back.
	burst := 0
	for i := 0; i < bargeInMin; i++ {
		burst += len(g.audio(loud, true))
	}
	if burst != bargeInMin*160 {
		t.Errorf("interruption released %d samples, want %d", burst, bargeInMin*160)
	}
	if got := g.audio(loud, true); len(got) != 160 {
		t.Error("after an interruption a frame was held back")
	}
	if got := g.audio(loud, false); len(got) != 160 {
		t.Error("a frame was held back although the agent had gone quiet")
	}

	// A burst in progress is released, in order, when the agent stops talking.
	var urgent bargein
	urgent.audio(loud, true)
	if got := urgent.audio(quiet, false); len(got) != 320 {
		t.Errorf("a burst in progress wasn't released when the agent stopped: %d samples", len(got))
	}

	// Speech that never gets going is released rather than held for ever.
	var slow bargein
	released := 0
	for i := 0; i < bargeInHold; i++ {
		pcm := quiet
		if i%8 == 0 {
			pcm = loud
		}
		released += len(slow.audio(pcm, true))
	}
	if released == 0 {
		t.Error("a burst held back for too long was never released")
	}
}

func TestNoCueInTheMiddleOfTheAgentsTurn(t *testing.T) {
	// The time limit comes while the agent is still speaking. The cue that asks it to wrap up would
	// cut it off, and the agent would say its line again from the start: the cue is held back, and
	// the call ends at the end of the grace period instead.
	h := newHarness(t)
	ctx := context.Background()
	p := params()
	p.MaxDuration = 2 * time.Second // wrap-up cue at 1 s, final cue at 2 s, 1 s of grace
	resCh := startCall(h, ctx, h.config(), p)
	form := <-h.created
	h.answer(ctx, form.Get("Twiml"), "CA1", true)
	h.sonic.waitInput(t, func(name string, b map[string]any) bool { return name == "textInput" })

	// The agent talks until past the limit, without ever ending its turn.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(200 * time.Millisecond):
			}
			pcm := base64.StdEncoding.EncodeToString(audio.PCMToLE16(nil, fill(3200, 1000)))
			h.sonic.emit("audioOutput", map[string]any{"contentId": "a", "content": pcm})
		}
	}()
	go h.sonic.endsWhenClosed(t)

	res := waitResult(t, resCh)
	if res.EndReason != "time_limit" {
		t.Fatalf("result = %+v", res)
	}
	h.sonic.mu.Lock()
	cues := strings.Join(h.sonic.cues, " | ")
	h.sonic.mu.Unlock()
	if strings.Contains(cues, builtinCues.Final) || strings.Contains(cues, builtinCues.WrapUp) {
		t.Errorf("a cue went out in the middle of the agent's turn: %s", cues)
	}
}
