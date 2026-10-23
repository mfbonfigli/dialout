package elevenlabs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/mfbonfigli/dialout/internal/voice"
)

// Session is one conversation with the agent. A reader goroutine takes messages off the WebSocket;
// a loop goroutine owns the state, turns messages into voice events and ends the agent's turn on
// a timer when ElevenLabs doesn't mark it.
type Session struct {
	ctx            context.Context
	ws             *websocket.Conn
	conversationID string
	in, out        format
	quiet          time.Duration

	events    chan voice.Event
	abort     chan struct{}
	done      chan struct{}
	err       error // set by the loop before done is closed
	readErr   error // set by the reader before it closes its channel
	ended     atomic.Bool
	endOnce   sync.Once
	closeOnce sync.Once

	// Loop state.
	turnOpen       bool      // agent audio arrived and its turn hasn't ended
	lastAudio      time.Time // when the last agent audio arrived
	audioSinceStop bool      // agent audio arrived since the last Interrupted
	pending        string    // the agent's text for the current turn, reported when it ends
	waitCorrection bool      // interrupted: wait for the corrected text before reporting it
}

func newSession(ctx context.Context, ws *websocket.Conn, id string, in, out format, quiet time.Duration) *Session {
	s := &Session{
		ctx: ctx, ws: ws, conversationID: id, in: in, out: out, quiet: quiet,
		events: make(chan voice.Event, 64),
		abort:  make(chan struct{}),
		done:   make(chan struct{}),
	}
	msgs := make(chan []byte, 64)
	go s.read(msgs)
	go s.loop(msgs)
	return s
}

// ConversationID is ElevenLabs' id for the conversation, to find it in their dashboard.
func (s *Session) ConversationID() string { return s.conversationID }

func (s *Session) Events() <-chan voice.Event { return s.events }

// SendAudio streams the callee's audio in the agent's input format.
func (s *Session) SendAudio(pcm []int16) error {
	return s.send(map[string]any{"user_audio_chunk": base64.StdEncoding.EncodeToString(s.in.encode(pcm))})
}

// Cue sends a user message when the agent should answer now, and a contextual update otherwise:
// ElevenLabs adds that to the agent's context without interrupting it.
func (s *Session) Cue(text string, respond bool) error {
	if respond {
		return s.send(map[string]any{"type": "user_message", "text": text})
	}
	return s.send(map[string]any{"type": "contextual_update", "text": text})
}

// End closes the WebSocket normally, which ends the conversation; Events closes once ElevenLabs
// has answered the close. Idempotent.
func (s *Session) End() error {
	s.endOnce.Do(func() {
		s.ended.Store(true)
		go s.ws.Close(websocket.StatusNormalClosure, "call over")
	})
	return nil
}

// Close drops the connection at once and waits for the loop to stop. Idempotent.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		s.ended.Store(true)
		close(s.abort) // before closing, so the end of the connection isn't taken for a failure
		s.ws.CloseNow()
	})
	<-s.done
	return s.err
}

func (s *Session) Err() error {
	select {
	case <-s.done:
		return s.err
	default:
		return nil
	}
}

func (s *Session) send(v any) error {
	if s.ended.Load() {
		return voice.ErrClosed
	}
	if err := writeJSON(s.ctx, s.ws, v); err != nil {
		if s.ended.Load() {
			return voice.ErrClosed
		}
		return err
	}
	return nil
}

func (s *Session) read(msgs chan<- []byte) {
	defer close(msgs)
	for {
		_, data, err := s.ws.Read(s.ctx)
		if err != nil {
			s.readErr = err
			return
		}
		select {
		case msgs <- data:
		case <-s.abort:
			return
		}
	}
}

func (s *Session) loop(msgs <-chan []byte) {
	// done closes before events (defers run last-first), so whoever sees Events close can
	// read Err right away.
	defer close(s.events)
	defer close(s.done)
	tick := time.NewTicker(max(s.quiet/4, 10*time.Millisecond))
	defer tick.Stop()
	for {
		select {
		case data, ok := <-msgs:
			if !ok {
				_ = s.flushAgent()
				s.err = s.endErr()
				return
			}
			if err := s.handle(data); err != nil {
				if !errors.Is(err, voice.ErrClosed) {
					s.err = err
				}
				return
			}
		case now := <-tick.C:
			if s.turnOpen && now.Sub(s.lastAudio) >= s.quiet {
				if s.endTurn() != nil {
					return
				}
			}
		case <-s.abort:
			return
		}
	}
}

// endErr says how the connection ended: a normal close, or one we asked for, is no failure.
func (s *Session) endErr() error {
	select {
	case <-s.abort:
		return nil
	default:
	}
	if s.ended.Load() || websocket.CloseStatus(s.readErr) == websocket.StatusNormalClosure {
		return nil
	}
	return fmt.Errorf("elevenlabs: connection closed: %w", s.readErr)
}

func (s *Session) emit(e voice.Event) error {
	select {
	case s.events <- e:
		return nil
	case <-s.abort:
		return voice.ErrClosed
	}
}

type message struct {
	Type  string `json:"type"`
	Audio *struct {
		AudioBase64 string `json:"audio_base_64"`
		IsFinal     bool   `json:"is_final"`
	} `json:"audio_event"`
	UserTranscript *struct {
		Text string `json:"user_transcript"`
	} `json:"user_transcription_event"`
	AgentResponse *struct {
		Text string `json:"agent_response"`
	} `json:"agent_response_event"`
	Correction *struct {
		Corrected string `json:"corrected_agent_response"`
	} `json:"agent_response_correction_event"`
	Ping *struct {
		EventID int64 `json:"event_id"`
	} `json:"ping_event"`
	ToolCall *struct {
		ToolName   string          `json:"tool_name"`
		ToolCallID string          `json:"tool_call_id"`
		Parameters json.RawMessage `json:"parameters"`
	} `json:"client_tool_call"`
}

func (s *Session) handle(data []byte) error {
	var m message
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("elevenlabs: decoding a message: %w", err)
	}
	switch m.Type {
	case "audio":
		if m.Audio == nil {
			return nil
		}
		b, err := base64.StdEncoding.DecodeString(m.Audio.AudioBase64)
		if err != nil {
			return fmt.Errorf("elevenlabs: decoding audio: %w", err)
		}
		s.lastAudio = time.Now()
		s.turnOpen = true
		s.audioSinceStop = true
		if err := s.emit(voice.AudioOut{PCM: s.out.decode(b)}); err != nil {
			return err
		}
		if m.Audio.IsFinal {
			return s.endTurn()
		}

	case "agent_response":
		// The text of a whole turn, sent as it starts: reported when the turn ends, corrected if the
		// callee interrupts it.
		if err := s.flushAgent(); err != nil {
			return err
		}
		if m.AgentResponse != nil {
			s.pending = strings.TrimSpace(m.AgentResponse.Text)
		}

	case "agent_response_correction":
		if m.Correction != nil {
			if c := strings.TrimSpace(m.Correction.Corrected); c != "" {
				s.pending = c
			}
		}
		return s.flushAgent()

	case "user_transcript":
		if err := s.flushAgent(); err != nil {
			return err
		}
		if m.UserTranscript != nil {
			if t := strings.TrimSpace(m.UserTranscript.Text); t != "" {
				return s.emit(voice.Transcript{Speaker: voice.SpeakerCallee, Text: t})
			}
		}

	case "interruption":
		s.turnOpen = false
		s.waitCorrection = s.pending != ""
		if s.audioSinceStop {
			s.audioSinceStop = false
			return s.emit(voice.Interrupted{})
		}

	case "ping":
		if m.Ping != nil {
			return s.send(map[string]any{"type": "pong", "event_id": m.Ping.EventID})
		}

	case "client_tool_call":
		if m.ToolCall != nil {
			return s.answerTool(m.ToolCall.ToolName, m.ToolCall.ToolCallID, m.ToolCall.Parameters)
		}
	}
	return nil
}

// answerTool sends the result back to the agent, then reports a valid call. An invalid one gets
// an error result so the agent can correct itself.
func (s *Session) answerTool(name, id string, params json.RawMessage) error {
	ev, perr := voice.ParseTool(name, params)
	err := s.send(map[string]any{
		"type":         "client_tool_result",
		"tool_call_id": id,
		"result":       voice.PlainToolResult(perr),
		"is_error":     perr != nil,
	})
	if err != nil && !errors.Is(err, voice.ErrClosed) {
		return err
	}
	if perr != nil {
		return nil
	}
	return s.emit(ev)
}

// endTurn reports the end of the agent's turn, and its text unless a correction is due.
func (s *Session) endTurn() error {
	s.turnOpen = false
	if !s.waitCorrection {
		if err := s.flushAgent(); err != nil {
			return err
		}
	}
	return s.emit(voice.TurnEnd{})
}

func (s *Session) flushAgent() error {
	text := s.pending
	s.pending, s.waitCorrection = "", false
	if text == "" {
		return nil
	}
	return s.emit(voice.Transcript{Speaker: voice.SpeakerAgent, Text: text})
}
