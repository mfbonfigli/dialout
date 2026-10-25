// Package sonic runs one Amazon Nova 2 Sonic conversation over Bedrock's bidirectional stream:
// the opening event sequence, the callee's audio in, the agent's audio and
// transcripts out, text cues, the report_outcome and end_call tools, barge-in and the closing
// sequence. Engine puts it behind voice.Engine.
package sonic

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/mfbonfigli/dialout/internal/audio"
	"github.com/mfbonfigli/dialout/internal/voice"
	"github.com/mfbonfigli/dialout/prompts"
)

const (
	// ModelID is Nova 2 Sonic, available in the EU only in eu-north-1.
	ModelID = "amazon.nova-2-sonic-v1:0"
	// MaxStream is how long Bedrock keeps a bidirectional stream open. Calls must end well before.
	MaxStream = 8 * time.Minute

	DefaultVoice       = "tiffany"
	DefaultEndpointing = "MEDIUM"
)

// Inference settings: the values AWS documents as a starting point.
const (
	maxTokens   = 1024
	topP        = 0.9
	temperature = 0.7
)

var (
	// voices maps each of Nova 2 Sonic's voices to its grammatical gender (true: female), which a
	// prompt may state. tiffany and matthew (en-US) speak every supported language.
	voices = map[string]bool{
		"tiffany": true, "matthew": false, // en-US
		"amy": true, "olivia": true, // en-GB, en-AU
		"kiara": true, "arjun": false, // en-IN, hi-IN
		"ambre": true, "florian": false, // fr-FR
		"beatrice": true, "lorenzo": false, // it-IT
		"tina": true, "lennart": false, // de-DE
		"lupe": true, "carlos": false, // es-US
		"carolina": true, "leo": false, // pt-BR
	}
	voiceNames  = "tiffany, matthew, amy, olivia, kiara, arjun, ambre, florian, beatrice, lorenzo, tina, lennart, lupe, carlos, carolina or leo"
	endpointing = map[string]bool{"HIGH": true, "MEDIUM": true, "LOW": true}

	// ErrClosed is returned by sends after End or Close.
	ErrClosed = voice.ErrClosed
)

// Config is what varies between calls.
type Config struct {
	Prompt      *prompts.Rendered // the system prompt, the tools' descriptions and the tool hints
	Voice       string            // one of voices; DefaultVoice when empty
	Endpointing string            // turn detection sensitivity: HIGH, MEDIUM (DefaultEndpointing) or LOW
}

func (c *Config) normalize() error {
	if c.Prompt == nil || c.Prompt.System == "" {
		return errors.New("sonic: empty system prompt")
	}
	if c.Voice == "" {
		c.Voice = DefaultVoice
	}
	if _, ok := voices[c.Voice]; !ok {
		return fmt.Errorf("sonic: unsupported voice %q (%s)", c.Voice, voiceNames)
	}
	if c.Endpointing == "" {
		c.Endpointing = DefaultEndpointing
	}
	if !endpointing[c.Endpointing] {
		return fmt.Errorf("sonic: unsupported endpointing sensitivity %q", c.Endpointing)
	}
	return nil
}

// Stream is the part of the Bedrock event stream the session uses;
// *bedrockruntime.InvokeModelWithBidirectionalStreamEventStream implements it.
type Stream interface {
	Send(context.Context, types.InvokeModelWithBidirectionalStreamInput) error
	Events() <-chan types.InvokeModelWithBidirectionalStreamOutput
	Close() error
	Err() error
}

// Session is one conversation. Read Events until it is closed; the methods that send are safe to
// call from any goroutine.
type Session struct {
	ctx     context.Context
	stream  Stream
	prompt  string // promptName, shared by every input event
	audio   string // contentName of the audio block, open for the whole session
	started time.Time
	hints   prompts.Hints // the next step, in the tool results

	sendMu sync.Mutex
	ended  bool // the closing sequence was sent; guarded by sendMu

	events    chan voice.Event
	abort     chan struct{}
	closeOnce sync.Once
	done      chan struct{}
	err       error // set by the reader before done is closed

	// Reader state.
	contents       map[string]outContentStart
	pendingTool    *outToolUse
	audioSinceStop bool // agent audio was emitted since the last Interrupted
}

// Start sends the opening sequence on an open stream: sessionStart, promptStart (voice, 8 kHz
// output, tools), the system prompt, and the start of the audio block that SendAudio fills.
func Start(ctx context.Context, stream Stream, cfg Config) (*Session, error) {
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	s := &Session{
		ctx:      ctx,
		stream:   stream,
		prompt:   newID(),
		audio:    newID(),
		started:  time.Now(),
		hints:    cfg.Prompt.Hints,
		events:   make(chan voice.Event, 64),
		abort:    make(chan struct{}),
		done:     make(chan struct{}),
		contents: map[string]outContentStart{},
	}
	go s.read()

	pcm := audioConfig{
		MediaType:       "audio/lpcm",
		SampleRateHertz: audio.SampleRate,
		SampleSizeBits:  16,
		ChannelCount:    1,
		Encoding:        "base64",
		AudioType:       "SPEECH",
	}
	out := pcm
	out.VoiceID = cfg.Voice
	system := newID()
	err := s.sendAll(
		encodeEvent("sessionStart", sessionStart{
			InferenceConfiguration:     inferenceConfig{MaxTokens: maxTokens, TopP: topP, Temperature: temperature},
			TurnDetectionConfiguration: turnDetection{EndpointingSensitivity: cfg.Endpointing},
		}),
		encodeEvent("promptStart", promptStart{
			PromptName:                 s.prompt,
			TextOutputConfiguration:    mediaType{"text/plain"},
			AudioOutputConfiguration:   out,
			ToolUseOutputConfiguration: mediaType{"application/json"},
			ToolConfiguration:          toolConfig{Tools: callTools(voice.ToolSpecs(cfg.Prompt.Tools))},
		}),
		encodeEvent("contentStart", contentStart{
			PromptName: s.prompt, ContentName: system, Type: "TEXT", Role: "SYSTEM",
			TextInputConfiguration: &mediaType{"text/plain"},
		}),
		encodeEvent("textInput", content{PromptName: s.prompt, ContentName: system, Content: cfg.Prompt.System}),
		encodeEvent("contentEnd", contentEnd{PromptName: s.prompt, ContentName: system}),
		encodeEvent("contentStart", contentStart{
			PromptName: s.prompt, ContentName: s.audio, Type: "AUDIO", Interactive: true, Role: "USER",
			AudioInputConfiguration: &pcm,
		}),
	)
	if err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// Deadline is when Bedrock will cut the stream.
func (s *Session) Deadline() time.Time { return s.started.Add(MaxStream) }

// Events delivers what the model produces. It is closed when the stream ends, after End or on an
// error (see Err).
func (s *Session) Events() <-chan voice.Event { return s.events }

// SendAudio streams the callee's audio (8 kHz, any length) as Sonic's little-endian LPCM.
func (s *Session) SendAudio(pcm []int16) error {
	return s.sendAll(encodeEvent("audioInput", content{
		PromptName: s.prompt, ContentName: s.audio, Content: base64.StdEncoding.EncodeToString(audio.PCMToLE16(nil, pcm)),
	}))
}

// Cue injects a line of text into the conversation while the audio keeps streaming, such as the
// opening and wrap-up cues. Sonic takes it as the user's turn: sent while the agent
// is still audible, it interrupts it.
func (s *Session) Cue(text string, _ bool) error {
	name := newID()
	return s.sendAll(
		encodeEvent("contentStart", contentStart{
			PromptName: s.prompt, ContentName: name, Type: "TEXT", Interactive: true, Role: "USER",
			TextInputConfiguration: &mediaType{"text/plain"},
		}),
		encodeEvent("textInput", content{PromptName: s.prompt, ContentName: name, Content: text}),
		encodeEvent("contentEnd", contentEnd{PromptName: s.prompt, ContentName: name}),
	)
}

// End sends the closing sequence in the documented order: contentEnd for the audio block,
// promptEnd, sessionEnd. Keep reading Events until it is closed, then call Close. End is
// idempotent.
func (s *Session) End() error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if s.ended || s.aborted() {
		return nil
	}
	s.ended = true
	return s.sendLocked(
		encodeEvent("contentEnd", contentEnd{PromptName: s.prompt, ContentName: s.audio}),
		encodeEvent("promptEnd", promptEnd{PromptName: s.prompt}),
		encodeEvent("sessionEnd", struct{}{}),
	)
}

// Close releases the stream at once, whether or not End was called, and waits for the reader to
// stop. It returns the error that ended the stream, if any. Close is idempotent.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		close(s.abort) // before closing the stream, so the reader doesn't take its end for a failure
		_ = s.stream.Close()
	})
	<-s.done
	return s.err
}

// Err returns the error that ended the stream, once Events is closed.
func (s *Session) Err() error {
	select {
	case <-s.done:
		return s.err
	default:
		return nil
	}
}

func (s *Session) aborted() bool {
	select {
	case <-s.abort:
		return true
	default:
		return false
	}
}

func (s *Session) sendAll(events ...[]byte) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if s.ended || s.aborted() {
		return ErrClosed
	}
	return s.sendLocked(events...)
}

func (s *Session) sendLocked(events ...[]byte) error {
	for _, e := range events {
		err := s.stream.Send(s.ctx, &types.InvokeModelWithBidirectionalStreamInputMemberChunk{
			Value: types.BidirectionalInputPayloadPart{Bytes: e},
		})
		if err != nil {
			return fmt.Errorf("sonic: send: %w", err)
		}
	}
	return nil
}

func (s *Session) read() {
	// done closes before events (defers run last-first), so whoever sees Events close can
	// read Err right away.
	defer close(s.events)
	defer close(s.done)
	err := s.readLoop()
	if !s.aborted() { // after Close, whatever the stream reports is not a failure
		s.err = err
	}
}

func (s *Session) readLoop() error {
	for ev := range s.stream.Events() {
		chunk, ok := ev.(*types.InvokeModelWithBidirectionalStreamOutputMemberChunk)
		if !ok {
			continue
		}
		if err := s.handle(chunk.Value.Bytes); err != nil {
			return err
		}
	}
	if err := s.stream.Err(); err != nil {
		return fmt.Errorf("sonic: stream: %w", err)
	}
	return nil
}

// emit delivers an event, giving up when the session is closed under a reader that stopped
// draining Events.
func (s *Session) emit(e voice.Event) error {
	select {
	case s.events <- e:
		return nil
	case <-s.abort:
		return ErrClosed
	}
}

func (s *Session) handle(data []byte) error {
	var o output
	if err := json.Unmarshal(data, &o); err != nil {
		return fmt.Errorf("sonic: decoding output event: %w", err)
	}
	ev := &o.Event
	switch {
	case ev.ContentStart != nil:
		s.contents[ev.ContentStart.ContentID] = *ev.ContentStart

	case ev.TextOutput != nil:
		text := strings.TrimSpace(ev.TextOutput.Content)
		if isInterruptedMarker(text) {
			return s.interrupted()
		}
		// SPECULATIVE agent text is what the agent plans to say; the FINAL text that follows the
		// audio is what it actually said.
		c := s.contents[ev.TextOutput.ContentID]
		stage := c.generationStage()
		switch {
		case text == "" || stage == "SPECULATIVE":
		case c.Role == "USER":
			return s.emit(voice.Transcript{Speaker: voice.SpeakerCallee, Text: text})
		case c.Role == "ASSISTANT" && stage == "FINAL":
			return s.emit(voice.Transcript{Speaker: voice.SpeakerAgent, Text: text})
		}

	case ev.AudioOutput != nil:
		le, err := base64.StdEncoding.DecodeString(ev.AudioOutput.Content)
		if err != nil {
			return fmt.Errorf("sonic: decoding audio: %w", err)
		}
		s.audioSinceStop = true
		return s.emit(voice.AudioOut{PCM: audio.PCMFromLE16(nil, le)})

	case ev.ToolUse != nil:
		s.pendingTool = ev.ToolUse

	case ev.ContentEnd != nil:
		delete(s.contents, ev.ContentEnd.ContentID)
		switch {
		case ev.ContentEnd.StopReason == "INTERRUPTED":
			return s.interrupted()
		case ev.ContentEnd.Type == "AUDIO" && ev.ContentEnd.StopReason == "END_TURN":
			return s.emit(voice.TurnEnd{})
		}
		if ev.ContentEnd.Type == "TOOL" && s.pendingTool != nil {
			t := s.pendingTool
			s.pendingTool = nil
			return s.answerTool(t)
		}

	case ev.UsageEvent != nil:
		t := ev.UsageEvent.Details.Total
		return s.emit(voice.Usage{
			"input_speech_tokens": t.Input.SpeechTokens, "input_text_tokens": t.Input.TextTokens,
			"output_speech_tokens": t.Output.SpeechTokens, "output_text_tokens": t.Output.TextTokens,
		})
	}
	return nil
}

// interrupted emits Interrupted once per barge-in: Sonic can signal one interruption on several
// content blocks.
func (s *Session) interrupted() error {
	if !s.audioSinceStop {
		return nil
	}
	s.audioSinceStop = false
	return s.emit(voice.Interrupted{})
}

// answerTool reports the tool call to the reader of Events and sends the result back to Sonic. An
// invalid call is answered with an error and not reported. The result goes out from its own
// goroutine so the reader never waits on a write; if it fails, the stream is broken and the reader
// sees that.
func (s *Session) answerTool(t *outToolUse) error {
	ev, perr := voice.ParseTool(t.ToolName, t.input())
	name := newID()
	go s.sendAll(
		encodeEvent("contentStart", contentStart{
			PromptName: s.prompt, ContentName: name, Type: "TOOL", Role: "TOOL",
			ToolResultInputConfiguration: &toolResultConfig{
				ToolUseID: t.ToolUseID, Type: "TEXT", TextInputConfiguration: mediaType{"text/plain"},
			},
		}),
		encodeEvent("toolResult", content{PromptName: s.prompt, ContentName: name, Content: voice.ToolResult(t.ToolName, perr, s.hints)}),
		encodeEvent("contentEnd", contentEnd{PromptName: s.prompt, ContentName: name}),
	)
	if perr != nil {
		return nil
	}
	return s.emit(ev)
}
