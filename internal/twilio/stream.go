package twilio

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// NewStreamToken returns a single-use token for the stream URL: 32 random bytes, base64url without
// padding, so always 43 characters of [A-Za-z0-9_-] (the tunnel's path rule).
func NewStreamToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

var tokenRE = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// ValidTokenFormat reports whether s looks like a token from NewStreamToken.
func ValidTokenFormat(s string) bool { return tokenRE.MatchString(s) }

// StreamTwiML returns the inline TwiML that connects the answered call to the bridge over a
// bidirectional Media Stream. Values are XML-escaped.
func StreamTwiML(streamURL string, params map[string]string) (string, error) {
	if !strings.HasPrefix(streamURL, "wss://") {
		return "", errors.New("stream URL must use wss://")
	}
	if strings.Contains(streamURL, "?") {
		return "", errors.New("stream URL must not have a query string (use parameters instead)")
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><Response><Connect><Stream url="`)
	if err := xml.EscapeText(&b, []byte(streamURL)); err != nil {
		return "", err
	}
	b.WriteString(`">`)
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sortStrings(keys)
	for _, k := range keys {
		b.WriteString(`<Parameter name="`)
		_ = xml.EscapeText(&b, []byte(k))
		b.WriteString(`" value="`)
		_ = xml.EscapeText(&b, []byte(params[k]))
		b.WriteString(`"/>`)
	}
	b.WriteString(`</Stream></Connect></Response>`)
	return b.String(), nil
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// --- Media Streams messages (https://www.twilio.com/docs/voice/media-streams/websocket-messages)

// Inbound is a message Twilio sends on the stream. Exactly one of the event bodies is set,
// according to Event. Twilio encodes numbers such as chunk and timestamp as strings.
type Inbound struct {
	Event          string     `json:"event"` // connected, start, media, mark, dtmf, stop
	SequenceNumber string     `json:"sequenceNumber"`
	StreamSID      string     `json:"streamSid"`
	Start          *StartBody `json:"start,omitempty"`
	Media          *MediaBody `json:"media,omitempty"`
	Mark           *MarkBody  `json:"mark,omitempty"`
	DTMF           *DTMFBody  `json:"dtmf,omitempty"`
	Stop           *StopBody  `json:"stop,omitempty"`
	Protocol       string     `json:"protocol,omitempty"`
	Version        string     `json:"version,omitempty"`
}

type StartBody struct {
	AccountSID       string            `json:"accountSid"`
	StreamSID        string            `json:"streamSid"`
	CallSID          string            `json:"callSid"`
	Tracks           []string          `json:"tracks"`
	CustomParameters map[string]string `json:"customParameters"`
	MediaFormat      struct {
		Encoding   string `json:"encoding"`
		SampleRate int    `json:"sampleRate"`
		Channels   int    `json:"channels"`
	} `json:"mediaFormat"`
}

type MediaBody struct {
	Track     string `json:"track"`
	Chunk     string `json:"chunk"`
	Timestamp string `json:"timestamp"` // milliseconds since the stream started
	Payload   string `json:"payload"`   // base64 µ-law, 8 kHz mono
}

type MarkBody struct {
	Name string `json:"name"`
}

type DTMFBody struct {
	Track string `json:"track"`
	Digit string `json:"digit"`
}

type StopBody struct {
	AccountSID string `json:"accountSid"`
	CallSID    string `json:"callSid"`
}

// ParseInbound decodes one stream message and checks the fields dialout relies on.
func ParseInbound(data []byte) (*Inbound, error) {
	var m Inbound
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("twilio stream: %w", err)
	}
	switch m.Event {
	case "connected":
	case "start":
		if m.Start == nil || m.Start.CallSID == "" || m.Start.StreamSID == "" {
			return nil, errors.New("twilio stream: start without callSid/streamSid")
		}
		f := m.Start.MediaFormat
		if f.Encoding != "audio/x-mulaw" || f.SampleRate != 8000 || f.Channels != 1 {
			return nil, fmt.Errorf("twilio stream: unexpected media format %s/%d/%d", f.Encoding, f.SampleRate, f.Channels)
		}
	case "media":
		if m.Media == nil {
			return nil, errors.New("twilio stream: media without body")
		}
	case "mark":
		if m.Mark == nil {
			return nil, errors.New("twilio stream: mark without body")
		}
	case "dtmf":
		if m.DTMF == nil {
			return nil, errors.New("twilio stream: dtmf without body")
		}
	case "stop":
	default:
		return nil, fmt.Errorf("twilio stream: unknown event %q", m.Event)
	}
	return &m, nil
}

// TimestampMillis returns the media timestamp as an integer.
func (m *MediaBody) TimestampMillis() (int64, error) { return strconv.ParseInt(m.Timestamp, 10, 64) }

// Audio decodes the base64 µ-law payload.
func (m *MediaBody) Audio() ([]byte, error) { return base64.StdEncoding.DecodeString(m.Payload) }

// OutMedia builds a message that plays µ-law audio into the call. The payload must be raw µ-law
// bytes without any file header.
func OutMedia(streamSID string, mulaw []byte) []byte {
	return mustJSON(map[string]any{
		"event":     "media",
		"streamSid": streamSID,
		"media":     map[string]string{"payload": base64.StdEncoding.EncodeToString(mulaw)},
	})
}

// OutMark asks Twilio to echo name once the audio sent before it has played (or was cleared).
func OutMark(streamSID, name string) []byte {
	return mustJSON(map[string]any{"event": "mark", "streamSid": streamSID, "mark": map[string]string{"name": name}})
}

// OutClear empties Twilio's playback buffer (barge-in).
func OutClear(streamSID string) []byte {
	return mustJSON(map[string]any{"event": "clear", "streamSid": streamSID})
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
