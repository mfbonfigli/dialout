// Package voice is the seam between a call and the model that holds the conversation. The call
// engine talks to a Session; an Engine opens one when the callee answers: Nova 2 Sonic (package
// sonic), ElevenLabs Agents or OpenAI Realtime. Audio is 16-bit PCM at 8 kHz, mono, in both
// directions, as on the phone line.
package voice

import (
	"context"
	"errors"
	"time"

	"github.com/mfbonfigli/dialout/prompts"
)

// Config is one call's conversation, as an engine needs it.
type Config struct {
	Owner   string        // who the agent calls for
	Voice   string        // the engine's voice; the engine's default when empty
	Brief   prompts.Brief // who answers and why the agent calls
	Prompts *prompts.Set  // the prompt file; the built-in one when nil
	Now     time.Time     // the call's start, in the configured time zone; now when zero

	// Prompt is the rendered prompt and Closing how the engine ends a call: Prepare fills them,
	// and the call engine takes its cues and closing rules from them.
	Prompt  *prompts.Rendered
	Closing prompts.Closing
}

// Render renders the prompt for an engine that closes calls in the given way and whose voice has
// the given grammatical gender, and stores it in c.Prompt.
func (c Config) Render(closing prompts.Closing, female bool) (Config, error) {
	set := c.Prompts
	if set == nil {
		set = prompts.Builtin()
	}
	r, err := set.Render(closing, prompts.Vars{Owner: c.Owner, Female: female, Now: c.Now, Brief: c.Brief})
	if err != nil {
		return c, err
	}
	c.Prompt, c.Closing = r, closing
	return c, nil
}

// Engine opens conversations.
type Engine interface {
	// Name identifies the engine in meta.json and in logs.
	Name() string
	// Prepare checks a call's configuration before the call is placed, so a mistake fails before
	// anyone is rung, and fills in the engine's defaults (such as the voice).
	Prepare(Config) (Config, error)
	// Dial opens the conversation once the callee has answered. ctx bounds the whole session.
	Dial(ctx context.Context, c Config) (Session, error)
}

// Session is one conversation. Read Events until it is closed, then call Close. The methods that
// send are safe to call from any goroutine.
type Session interface {
	// SendAudio streams the callee's audio, any length.
	SendAudio(pcm []int16) error
	// Cue gives the agent a line from the system. With respond, the agent should act
	// on it now (the opening and final cues); without, it is context for the agent's next turn
	// (the wrap-up cue). Engines that can't tell the two apart take every cue as a turn.
	Cue(text string, respond bool) error
	// Events delivers what the agent does. It is closed when the conversation ends; see Err.
	Events() <-chan Event
	// End closes the conversation cleanly. Keep reading Events until it is closed. Idempotent.
	End() error
	// Close releases the session at once and returns the error that ended it, if any. Idempotent.
	Close() error
	// Err returns the error that ended the conversation, once Events is closed.
	Err() error
}

// ErrClosed is returned by sends after End or Close.
var ErrClosed = errors.New("voice: session closed")

// Event is something the agent did: AudioOut, TurnEnd, Interrupted, Transcript, Outcome, EndCall
// or Usage.
type Event interface{ isEvent() }

// AudioOut is agent speech to play.
type AudioOut struct{ PCM []int16 }

// TurnEnd means all the audio of the agent's current turn has been delivered (it may still be
// playing). After EndCall, it marks the end of the goodbye.
type TurnEnd struct{}

// Interrupted means the callee talked over the agent: drop the agent audio not yet played.
type Interrupted struct{}

// Transcript is a final transcript line. For the agent it is what was actually said, also when the
// callee interrupted it.
type Transcript struct {
	Speaker string // SpeakerCallee or SpeakerAgent
	Text    string
}

const (
	SpeakerCallee = "callee"
	SpeakerAgent  = "agent"
)

// Outcome is the agent's report_outcome call. Result is one of the Result* constants.
type Outcome struct {
	Result  string `json:"result"`
	Summary string `json:"summary"`
	Details string `json:"details"`
}

// EndCall is the agent's end_call: hang up once its goodbye has played.
type EndCall struct {
	Reason string `json:"reason"`
}

// Usage is the engine's running usage count by name, e.g. Sonic's tokens by kind. It goes into
// meta.json as is.
type Usage map[string]int

func (AudioOut) isEvent()    {}
func (TurnEnd) isEvent()     {}
func (Interrupted) isEvent() {}
func (Transcript) isEvent()  {}
func (Outcome) isEvent()     {}
func (EndCall) isEvent()     {}
func (Usage) isEvent()       {}
