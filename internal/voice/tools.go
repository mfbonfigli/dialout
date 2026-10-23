package voice

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mfbonfigli/dialout/prompts"
)

// The only tools the agent gets, whatever the engine. Their schema is fixed here, since the call
// engine parses them; their descriptions come from the prompt file. Each engine declares them in
// its own format.
const (
	ToolReportOutcome = "report_outcome"
	ToolEndCall       = "end_call"
)

// Outcome results, as the agent reports them and get_call returns them.
const (
	ResultSuccess     = "success"
	ResultPartial     = "partial"
	ResultFailed      = "failed"
	ResultVoicemail   = "voicemail"
	ResultWrongNumber = "wrong_number"
)

var results = []string{ResultSuccess, ResultPartial, ResultFailed, ResultVoicemail, ResultWrongNumber}

// ToolSpec is a tool as the agent sees it: Schema is a JSON Schema for its input.
type ToolSpec struct {
	Name        string
	Description string
	Schema      string
}

// ToolSpecs returns the agent's tools with the descriptions of a rendered prompt.
func ToolSpecs(t prompts.ToolTexts) []ToolSpec {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	result := str(t.Result)
	result["enum"] = results
	return []ToolSpec{
		{Name: ToolReportOutcome, Description: t.ReportOutcome, Schema: schema(map[string]any{
			"result": result, "summary": str(t.Summary), "details": str(t.Details),
		}, "result", "summary")},
		{Name: ToolEndCall, Description: t.EndCall, Schema: schema(map[string]any{
			"reason": str(t.Reason),
		}, "reason")},
	}
}

func schema(props map[string]any, required ...string) string {
	b, _ := json.MarshalIndent(map[string]any{"type": "object", "properties": props, "required": required}, "", "  ")
	return string(b)
}

// ParseTool turns a tool call into the Event it stands for, or returns an error that goes back to
// the agent as the tool result so it can correct itself.
func ParseTool(name string, input json.RawMessage) (Event, error) {
	switch name {
	case ToolReportOutcome:
		var o Outcome
		if err := json.Unmarshal(input, &o); err != nil {
			return nil, fmt.Errorf("invalid input: %v", err)
		}
		o.Summary = strings.TrimSpace(o.Summary)
		o.Details = strings.TrimSpace(o.Details)
		valid := false
		for _, r := range results {
			valid = valid || o.Result == r
		}
		if !valid {
			return nil, fmt.Errorf("result must be one of: %s", strings.Join(results, ", "))
		}
		if o.Summary == "" {
			return nil, fmt.Errorf("summary is required")
		}
		return o, nil
	case ToolEndCall:
		var e EndCall
		if err := json.Unmarshal(input, &e); err != nil {
			return nil, fmt.Errorf("invalid input: %v", err)
		}
		e.Reason = strings.TrimSpace(e.Reason)
		return e, nil
	}
	return nil, fmt.Errorf("unknown tool: %s", name)
}

// PlainToolResult is what the agent gets back for a tool call on engines that say goodbye and
// call the tools in the same turn: just whether it worked.
func PlainToolResult(err error) string {
	v := map[string]any{"ok": true}
	if err != nil {
		v = map[string]any{"ok": false, "error": err.Error()}
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// ToolResult is what Sonic gets back for a tool call, with the next closing step from the prompt
// file. In live Sonic sessions the agent ended its turn as soon as it spoke, so a goodbye between
// the two tools meant end_call never came: the tools come first and the goodbye last.
func ToolResult(name string, err error, h prompts.Hints) string {
	var v map[string]any
	switch {
	case err != nil:
		v = map[string]any{"ok": false, "error": err.Error()}
	case name == ToolReportOutcome:
		v = map[string]any{"ok": true, "next": h.EndCall}
	case name == ToolEndCall:
		v = map[string]any{"ok": true, "next": h.Goodbye}
	default:
		v = map[string]any{"ok": true}
	}
	b, _ := json.Marshal(v)
	return string(b)
}
