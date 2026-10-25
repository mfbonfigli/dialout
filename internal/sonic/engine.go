package sonic

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"

	"github.com/mfbonfigli/dialout/internal/voice"
	"github.com/mfbonfigli/dialout/prompts"
)

var (
	_ voice.Engine  = (*Engine)(nil)
	_ voice.Session = (*Session)(nil)
)

// Engine is Nova 2 Sonic behind voice.Engine.
type Engine struct {
	// Connect opens a bidirectional stream. NewEngine connects to Bedrock; tests plug in a fake.
	Connect func(ctx context.Context) (Stream, error)
}

// NewEngine returns an Engine on Bedrock. The client must be for a region with Nova 2 Sonic
// (eu-north-1 is the EU one).
func NewEngine(client *bedrockruntime.Client) *Engine {
	return &Engine{Connect: func(ctx context.Context) (Stream, error) {
		out, err := client.InvokeModelWithBidirectionalStream(ctx, &bedrockruntime.InvokeModelWithBidirectionalStreamInput{
			ModelId: aws.String(ModelID),
		})
		if err != nil {
			return nil, fmt.Errorf("sonic: %w", err)
		}
		return out.GetStream(), nil
	}}
}

func (*Engine) Name() string { return "sonic" }

// Prepare checks the voice and renders the prompt, so a bad brief fails before the call is placed.
func (e *Engine) Prepare(c voice.Config) (voice.Config, error) {
	return e.prompt(c)
}

func (e *Engine) Dial(ctx context.Context, c voice.Config) (voice.Session, error) {
	if c.Prompt == nil {
		var err error
		if c, err = e.prompt(c); err != nil {
			return nil, err
		}
	}
	stream, err := e.Connect(ctx)
	if err != nil {
		return nil, err
	}
	s, err := Start(ctx, stream, Config{Prompt: c.Prompt, Voice: c.Voice})
	if err != nil {
		_ = stream.Close()
		return nil, err
	}
	return s, nil
}

func (e *Engine) prompt(c voice.Config) (voice.Config, error) {
	if c.Voice == "" {
		c.Voice = DefaultVoice
	}
	female, ok := voices[c.Voice]
	if !ok {
		return c, fmt.Errorf("sonic: unsupported voice %q (%s)", c.Voice, voiceNames)
	}
	return c.Render(prompts.ToolsThenGoodbye, female)
}
