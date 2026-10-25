// Package openai holds the conversation on OpenAI's Realtime API, over a server-side WebSocket,
// behind voice.Engine: another alternative to Nova Sonic. Audio is G.711 µ-law at
// 8 kHz both ways (audio/pcmu), as on the phone line, so nothing is resampled. Unlike ElevenLabs
// nothing is set up in advance: instructions, voice, tools, turn detection and transcription are
// sent with every session.
package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/mfbonfigli/dialout/internal/voice"
	"github.com/mfbonfigli/dialout/prompts"
)

const (
	DefaultBaseURL = "wss://api.openai.com" // a project with EU data residency uses its own host
	DefaultModel   = "gpt-realtime-2.1"
	DefaultVoice   = "marin"
	// transcriptionModel transcribes the callee for the transcript; the model itself hears the
	// audio directly.
	transcriptionModel = "gpt-4o-transcribe"
)

// voices maps the built-in voices to their grammatical gender (true: female), which a prompt may
// state (examples/prompt-it.tmpl does). A newer voice needs Engine.Female set explicitly.
var voices = map[string]bool{
	"marin": true, "cedar": false, "alloy": true, "ash": false, "ballad": false,
	"coral": true, "echo": false, "sage": true, "shimmer": true, "verse": false,
}

var (
	_ voice.Engine  = (*Engine)(nil)
	_ voice.Session = (*Session)(nil)
)

// Engine opens Realtime sessions.
type Engine struct {
	APIKey  string
	Model   string // DefaultModel when empty
	BaseURL string // DefaultBaseURL when empty
	Voice   string // DefaultVoice when a call names none
	// Female is the grammatical gender of a voice missing from the built-in list. Nil: the
	// voice must be a known one.
	Female *bool
	// Language is the callee's language for the transcription (ISO 639-1); "it" when empty.
	Language string
}

func (*Engine) Name() string { return "openai" }

func (e *Engine) language() string {
	if e.Language == "" {
		return "it"
	}
	return e.Language
}

// Prepare checks the key and the voice and renders the prompt, so a bad brief fails before the
// call is placed.
func (e *Engine) Prepare(c voice.Config) (voice.Config, error) {
	return e.prompt(c)
}

func (e *Engine) prompt(c voice.Config) (voice.Config, error) {
	if e.APIKey == "" {
		return c, errors.New("openai: an API key is required")
	}
	if c.Voice == "" {
		c.Voice = e.Voice
	}
	if c.Voice == "" {
		c.Voice = DefaultVoice
	}
	female, known := voices[c.Voice]
	if e.Female != nil {
		female, known = *e.Female, true
	}
	if !known {
		return c, fmt.Errorf("openai: voice %q isn't a known one: set its gender too", c.Voice)
	}
	return c.Render(prompts.WithGoodbye, female)
}

// Dial opens the WebSocket, configures the session and waits for the server to accept it. ctx
// bounds the whole session.
func (e *Engine) Dial(ctx context.Context, c voice.Config) (voice.Session, error) {
	if c.Prompt == nil {
		var err error
		if c, err = e.prompt(c); err != nil {
			return nil, err
		}
	}
	base := strings.TrimRight(e.BaseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}
	model := e.Model
	if model == "" {
		model = DefaultModel
	}
	ws, _, err := websocket.Dial(ctx, base+"/v1/realtime?model="+url.QueryEscape(model), &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + e.APIKey}},
	})
	if err != nil {
		return nil, fmt.Errorf("openai: %w", err)
	}
	ws.SetReadLimit(4 << 20)
	if err := configure(ctx, ws, c.Prompt, c.Voice, e.language()); err != nil {
		ws.CloseNow()
		return nil, err
	}
	return newSession(ctx, ws), nil
}

// configure sends the session settings and waits for session.updated; an error event means the
// server refused them.
func configure(ctx context.Context, ws *websocket.Conn, p *prompts.Rendered, voiceName, language string) error {
	var tools []map[string]any
	for _, t := range voice.ToolSpecs(p.Tools) {
		tools = append(tools, map[string]any{
			"type": "function", "name": t.Name, "description": t.Description, "parameters": json.RawMessage(t.Schema),
		})
	}
	pcmu := map[string]any{"type": "audio/pcmu"}
	err := writeJSON(ctx, ws, map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"type":              "realtime",
			"instructions":      p.System,
			"output_modalities": []string{"audio"},
			"audio": map[string]any{
				"input": map[string]any{
					"format":         pcmu,
					"turn_detection": map[string]any{"type": "semantic_vad"},
					"transcription":  map[string]any{"model": transcriptionModel, "language": language},
				},
				"output": map[string]any{"format": pcmu, "voice": voiceName},
			},
			"tools":       tools,
			"tool_choice": "auto",
		},
	})
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for {
		_, data, err := ws.Read(cctx)
		if err != nil {
			return fmt.Errorf("openai: waiting for the session: %w", err)
		}
		var m struct {
			Type  string     `json:"type"`
			Error *errorBody `json:"error"`
		}
		if err := json.Unmarshal(data, &m); err != nil {
			return fmt.Errorf("openai: decoding an event: %w", err)
		}
		switch m.Type {
		case "session.updated":
			return nil
		case "error":
			return fmt.Errorf("openai: session refused: %s", m.Error)
		}
	}
}

type errorBody struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *errorBody) String() string {
	if e == nil {
		return "unknown error"
	}
	return strings.Trim(e.Code+": "+e.Message, ": ")
}

func writeJSON(ctx context.Context, ws *websocket.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := ws.Write(wctx, websocket.MessageText, b); err != nil {
		return fmt.Errorf("openai: send: %w", err)
	}
	return nil
}
