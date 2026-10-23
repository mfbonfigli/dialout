package voice

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mfbonfigli/dialout/prompts"
)

func TestParseTool(t *testing.T) {
	ev, err := ParseTool(ToolReportOutcome, json.RawMessage(`{"result":"success","summary":" Fatto. ","details":" alle 22 "}`))
	if err != nil || ev != (Outcome{Result: ResultSuccess, Summary: "Fatto.", Details: "alle 22"}) {
		t.Errorf("report_outcome = %#v, %v", ev, err)
	}
	ev, err = ParseTool(ToolEndCall, json.RawMessage(`{"reason":" fatto "}`))
	if err != nil || ev != (EndCall{Reason: "fatto"}) {
		t.Errorf("end_call = %#v, %v", ev, err)
	}
	for _, bad := range []struct{ name, input string }{
		{ToolReportOutcome, `{"result":"maybe","summary":"x"}`},
		{ToolReportOutcome, `{"result":"success","summary":"  "}`},
		{ToolReportOutcome, `not json`},
		{ToolEndCall, `[]`},
		{"transfer", `{}`},
	} {
		if _, err := ParseTool(bad.name, json.RawMessage(bad.input)); err == nil {
			t.Errorf("ParseTool(%s, %s) accepted", bad.name, bad.input)
		}
	}
}

func TestToolSpecs(t *testing.T) {
	texts := prompts.ToolTexts{ReportOutcome: "Report.", Result: "How it went.", Summary: "Sum \"up\".",
		Details: "Details.", EndCall: "End.", Reason: "Why."}
	specs := ToolSpecs(texts)
	if len(specs) != 2 || specs[0].Name != ToolReportOutcome || specs[0].Description != "Report." ||
		specs[1].Name != ToolEndCall || specs[1].Description != "End." {
		t.Fatalf("specs = %+v", specs)
	}
	var s struct {
		Properties map[string]struct {
			Description string   `json:"description"`
			Enum        []string `json:"enum"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal([]byte(specs[0].Schema), &s); err != nil {
		t.Fatal(err)
	}
	if s.Properties["summary"].Description != `Sum "up".` || len(s.Properties["result"].Enum) != 5 ||
		strings.Join(s.Required, ",") != "result,summary" {
		t.Errorf("report_outcome schema = %s", specs[0].Schema)
	}

	var r struct {
		OK   bool   `json:"ok"`
		Next string `json:"next"`
	}
	h := prompts.Hints{EndCall: "Now end_call.", Goodbye: "Now goodbye."}
	if json.Unmarshal([]byte(ToolResult(ToolReportOutcome, nil, h)), &r) != nil || !r.OK || r.Next != "Now end_call." {
		t.Errorf("report_outcome result = %s", ToolResult(ToolReportOutcome, nil, h))
	}
}
