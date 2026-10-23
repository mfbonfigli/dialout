package sonic

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
)

// Wire format. Every chunk on the bidirectional stream carries one JSON object
// {"event": {"<name>": {...}}}, in both directions.
// Input:  https://docs.aws.amazon.com/nova/latest/nova2-userguide/sonic-input-events.html
// Output: https://docs.aws.amazon.com/nova/latest/nova2-userguide/sonic-output-events.html

type mediaType struct {
	MediaType string `json:"mediaType"`
}

type audioConfig struct {
	MediaType       string `json:"mediaType"`
	SampleRateHertz int    `json:"sampleRateHertz"`
	SampleSizeBits  int    `json:"sampleSizeBits"`
	ChannelCount    int    `json:"channelCount"`
	VoiceID         string `json:"voiceId,omitempty"`
	Encoding        string `json:"encoding"`
	AudioType       string `json:"audioType"`
}

type inferenceConfig struct {
	MaxTokens   int     `json:"maxTokens"`
	TopP        float64 `json:"topP"`
	Temperature float64 `json:"temperature"`
}

type turnDetection struct {
	EndpointingSensitivity string `json:"endpointingSensitivity"`
}

type sessionStart struct {
	InferenceConfiguration     inferenceConfig `json:"inferenceConfiguration"`
	TurnDetectionConfiguration turnDetection   `json:"turnDetectionConfiguration"`
}

type inputSchema struct {
	JSON string `json:"json"` // the JSON Schema, itself encoded as a string
}

type toolSpec struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema inputSchema `json:"inputSchema"`
}

type tool struct {
	ToolSpec toolSpec `json:"toolSpec"`
}

type toolConfig struct {
	Tools []tool `json:"tools"`
}

type promptStart struct {
	PromptName                 string      `json:"promptName"`
	TextOutputConfiguration    mediaType   `json:"textOutputConfiguration"`
	AudioOutputConfiguration   audioConfig `json:"audioOutputConfiguration"`
	ToolUseOutputConfiguration mediaType   `json:"toolUseOutputConfiguration"`
	ToolConfiguration          toolConfig  `json:"toolConfiguration"`
}

type toolResultConfig struct {
	ToolUseID              string    `json:"toolUseId"`
	Type                   string    `json:"type"`
	TextInputConfiguration mediaType `json:"textInputConfiguration"`
}

type contentStart struct {
	PromptName                   string            `json:"promptName"`
	ContentName                  string            `json:"contentName"`
	Type                         string            `json:"type"` // TEXT, AUDIO, TOOL
	Interactive                  bool              `json:"interactive"`
	Role                         string            `json:"role"` // SYSTEM, USER, TOOL
	TextInputConfiguration       *mediaType        `json:"textInputConfiguration,omitempty"`
	AudioInputConfiguration      *audioConfig      `json:"audioInputConfiguration,omitempty"`
	ToolResultInputConfiguration *toolResultConfig `json:"toolResultInputConfiguration,omitempty"`
}

// content is the body of textInput, audioInput and toolResult.
type content struct {
	PromptName  string `json:"promptName"`
	ContentName string `json:"contentName"`
	Content     string `json:"content"`
}

type contentEnd struct {
	PromptName  string `json:"promptName"`
	ContentName string `json:"contentName"`
}

type promptEnd struct {
	PromptName string `json:"promptName"`
}

func encodeEvent(name string, body any) []byte {
	b, err := json.Marshal(map[string]map[string]any{"event": {name: body}})
	if err != nil {
		panic(err) // only fixed, marshalable types are passed
	}
	return b
}

// output is one decoded output event. Exactly one field of Event is set; events dialout
// doesn't use (completionStart, completionEnd) decode to nothing.
type output struct {
	Event struct {
		ContentStart *outContentStart `json:"contentStart"`
		TextOutput   *outContent      `json:"textOutput"`
		AudioOutput  *outContent      `json:"audioOutput"`
		ToolUse      *outToolUse      `json:"toolUse"`
		ContentEnd   *outContentEnd   `json:"contentEnd"`
		UsageEvent   *outUsage        `json:"usageEvent"`
	} `json:"event"`
}

type outContentStart struct {
	ContentID             string `json:"contentId"`
	Type                  string `json:"type"` // TEXT, AUDIO, TOOL
	Role                  string `json:"role"` // USER, ASSISTANT, TOOL
	AdditionalModelFields string `json:"additionalModelFields"`
}

// generationStage reads {"generationStage": "FINAL" | "SPECULATIVE"} out of additionalModelFields,
// which is a JSON object encoded as a string.
func (c *outContentStart) generationStage() string {
	var f struct {
		GenerationStage string `json:"generationStage"`
	}
	if c.AdditionalModelFields == "" || json.Unmarshal([]byte(c.AdditionalModelFields), &f) != nil {
		return ""
	}
	return f.GenerationStage
}

type outContent struct {
	ContentID string `json:"contentId"`
	Content   string `json:"content"`
}

type outToolUse struct {
	ContentID string          `json:"contentId"`
	ToolName  string          `json:"toolName"`
	ToolUseID string          `json:"toolUseId"`
	Content   json.RawMessage `json:"content"` // the tool input: a JSON object, sent as a string
}

// input returns the tool input as raw JSON, whether it was sent as a string or as an object.
func (t *outToolUse) input() json.RawMessage {
	var s string
	if json.Unmarshal(t.Content, &s) == nil {
		return json.RawMessage(s)
	}
	return t.Content
}

type outContentEnd struct {
	ContentID  string `json:"contentId"`
	Type       string `json:"type"`
	StopReason string `json:"stopReason"` // PARTIAL_TURN, END_TURN, INTERRUPTED, TOOL_USE
}

type tokens struct {
	SpeechTokens int `json:"speechTokens"`
	TextTokens   int `json:"textTokens"`
}

type outUsage struct {
	Details struct {
		Total struct {
			Input  tokens `json:"input"`
			Output tokens `json:"output"`
		} `json:"total"`
	} `json:"details"`
}

// isInterruptedMarker reports whether a textOutput is the barge-in marker {"interrupted": true}
// rather than transcript text.
func isInterruptedMarker(s string) bool {
	if len(s) == 0 || s[0] != '{' {
		return false
	}
	var m struct {
		Interrupted bool `json:"interrupted"`
	}
	return json.Unmarshal([]byte(s), &m) == nil && m.Interrupted
}

// newID returns a random UUID (version 4), used for prompt and content names.
func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
