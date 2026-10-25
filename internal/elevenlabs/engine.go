// Package elevenlabs holds the conversation on ElevenLabs Agents, over their Agents Platform
// WebSocket, behind voice.Engine: an alternative to Nova Sonic. The agent is set up
// once in ElevenLabs (the language, audio formats, client tools, which overrides are allowed); each
// call overrides only what changes between calls: the system prompt, and optionally the voice and
// the LLM. Every override must be allowed on the agent, or ElevenLabs closes the conversation
// right after it starts.
package elevenlabs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/mfbonfigli/dialout/internal/voice"
	"github.com/mfbonfigli/dialout/prompts"
)

const (
	// DefaultBaseURL is ElevenLabs' API. EU data residency (Enterprise plans) uses
	// https://api.eu.residency.elevenlabs.io instead.
	DefaultBaseURL = "https://api.elevenlabs.io"
	// DefaultTurnQuiet ends the agent's turn when its audio stops for this long and ElevenLabs
	// didn't mark the last chunk with is_final.
	DefaultTurnQuiet = 800 * time.Millisecond
)

var (
	_ voice.Engine  = (*Engine)(nil)
	_ voice.Session = (*Session)(nil)
)

// Engine opens conversations with one ElevenLabs agent.
type Engine struct {
	APIKey  string // xi-api-key: only used to get a signed URL for each conversation
	AgentID string
	BaseURL string // DefaultBaseURL when empty
	Voice   string // voice_id when a call names none; the agent's own voice when empty
	Female  bool   // the grammatical gender of the voice, which a prompt may state
	LLM     string // the agent's own LLM when empty; otherwise an override

	HTTP      *http.Client
	TurnQuiet time.Duration // DefaultTurnQuiet when 0
}

func (*Engine) Name() string { return "elevenlabs" }

// Prepare checks the settings and renders the prompt, so a bad brief fails before the call is
// placed. Voice stays empty when the agent's own voice is used.
func (e *Engine) Prepare(c voice.Config) (voice.Config, error) {
	return e.prompt(c)
}

func (e *Engine) prompt(c voice.Config) (voice.Config, error) {
	if e.APIKey == "" || e.AgentID == "" {
		return c, errors.New("elevenlabs: an API key and an agent ID are required")
	}
	if c.Voice == "" {
		c.Voice = e.Voice
	}
	return c.Render(prompts.WithGoodbye, e.Female)
}

// Dial gets a signed URL, opens the conversation with this call's overrides and waits for the
// agent's audio formats. ctx bounds the whole session.
func (e *Engine) Dial(ctx context.Context, c voice.Config) (voice.Session, error) {
	if c.Prompt == nil {
		var err error
		if c, err = e.prompt(c); err != nil {
			return nil, err
		}
	}
	signed, err := e.signedURL(ctx)
	if err != nil {
		return nil, err
	}
	ws, _, err := websocket.Dial(ctx, signed, nil)
	if err != nil {
		return nil, fmt.Errorf("elevenlabs: %w", err)
	}
	ws.SetReadLimit(4 << 20) // an audio chunk at a high PCM rate is far above the 32 KiB default
	s, err := e.start(ctx, ws, c.Prompt.System, c)
	if err != nil {
		ws.CloseNow()
		return nil, err
	}
	return s, nil
}

func (e *Engine) start(ctx context.Context, ws *websocket.Conn, system string, c voice.Config) (*Session, error) {
	// The first message stays the agent's own: it must be empty, so the agent waits for the callee's
	// "Hello?" as with Sonic, and the opening comes from the prompt.
	// The language is the agent's own: it never changes, so it isn't overridden.
	agent := map[string]any{"prompt": map[string]any{"prompt": system}}
	if e.LLM != "" {
		agent["prompt"].(map[string]any)["llm"] = e.LLM
	}
	override := map[string]any{"agent": agent}
	if c.Voice != "" {
		override["tts"] = map[string]any{"voice_id": c.Voice}
	}
	if err := writeJSON(ctx, ws, map[string]any{
		"type":                         "conversation_initiation_client_data",
		"conversation_config_override": override,
	}); err != nil {
		return nil, err
	}

	// The agent answers with its audio formats; nothing else is expected before.
	mctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for {
		_, data, err := ws.Read(mctx)
		if err != nil {
			return nil, fmt.Errorf("elevenlabs: waiting for the conversation to start: %w", err)
		}
		var m struct {
			Type     string `json:"type"`
			Metadata struct {
				ConversationID string `json:"conversation_id"`
				Input          string `json:"user_input_audio_format"`
				Output         string `json:"agent_output_audio_format"`
			} `json:"conversation_initiation_metadata_event"`
		}
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, fmt.Errorf("elevenlabs: decoding a message: %w", err)
		}
		if m.Type != "conversation_initiation_metadata" {
			continue
		}
		in, err := parseFormat(m.Metadata.Input)
		if err != nil {
			return nil, err
		}
		out, err := parseFormat(m.Metadata.Output)
		if err != nil {
			return nil, err
		}
		quiet := e.TurnQuiet
		if quiet == 0 {
			quiet = DefaultTurnQuiet
		}
		return newSession(ctx, ws, m.Metadata.ConversationID, in, out, quiet), nil
	}
}

// signedURL asks for a single conversation's WebSocket URL (valid 15 minutes), so the API key
// itself never travels on the conversation.
func (e *Engine) signedURL(ctx context.Context) (string, error) {
	base := strings.TrimRight(e.BaseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		base+"/v1/convai/conversation/get-signed-url?agent_id="+url.QueryEscape(e.AgentID), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("xi-api-key", e.APIKey)
	client := e.HTTP
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("elevenlabs: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("elevenlabs: signed URL: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body[:min(len(body), 300)])))
	}
	var r struct {
		SignedURL string `json:"signed_url"`
	}
	if err := json.Unmarshal(body, &r); err != nil || r.SignedURL == "" {
		return "", errors.New("elevenlabs: signed URL: unexpected response")
	}
	return r.SignedURL, nil
}

func writeJSON(ctx context.Context, ws *websocket.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := ws.Write(wctx, websocket.MessageText, b); err != nil {
		return fmt.Errorf("elevenlabs: send: %w", err)
	}
	return nil
}
