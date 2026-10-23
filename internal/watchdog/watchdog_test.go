package watchdog

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mfbonfigli/dialout/prompts"
)

func questions(t *testing.T) []prompts.Question {
	t.Helper()
	r, err := prompts.Builtin().Render(prompts.ToolsThenGoodbye, prompts.Vars{Owner: "Marco", Brief: prompts.Brief{Objective: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	return r.Watchdog
}

func TestDecisions(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/alpha/decisions" || r.Header.Get("Authorization") != "Bearer key" {
			http.Error(w, "bad request", 400)
			return
		}
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got)
		answers := map[string]any{}
		for name := range got["questions"].(map[string]any) {
			answers[name] = map[string]any{"type": "noul", "noul": 0.25}
		}
		answers["hostile"] = map[string]any{"type": "noul", "noul": 0.91}
		json.NewEncoder(w).Encode(map[string]any{"model": "m", "answers": answers, "usage": map[string]any{"cost": 0.0002}})
	}))
	defer srv.Close()

	qs := questions(t)
	d := &Decisions{BaseURL: srv.URL + "/api/", APIKey: "key", Model: "openai/gpt-6-luna-decisions"}
	state := State{Note: StateNote, Owner: "Marco", Objective: "Chiedi come sta.", Transcript: []Line{{"callee", "Pronto?"}}}
	a, err := d.Judge(context.Background(), state, qs)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Probabilities) != 2 || a.Probabilities["hostile"] != 0.91 || a.Probabilities["voicemail_or_ivr"] != 0.25 || a.Cost != 0.0002 {
		t.Errorf("answers = %+v", a)
	}
	// The request: the model, the state as given, every question as a yes/no with its criteria.
	q := got["questions"].(map[string]any)["voicemail_or_ivr"].(map[string]any)
	if got["model"] != "openai/gpt-6-luna-decisions" || q["type"] != "noul" || !strings.Contains(q["instructions"].(string), "voicemail") ||
		q["criteria"].(map[string]any)["true"] == "" || got["state"].(map[string]any)["objective"] != "Chiedi come sta." {
		t.Errorf("request = %v", got)
	}

	// Errors: HTTP status, a missing answer.
	d.APIKey = "wrong"
	if _, err := d.Judge(context.Background(), state, qs); err == nil || !strings.Contains(err.Error(), "HTTP 400") {
		t.Errorf("bad key: %v", err)
	}
	missing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"answers":{"voicemail_or_ivr":{"type":"noul","noul":0.1}}}`))
	}))
	defer missing.Close()
	d = &Decisions{BaseURL: missing.URL, APIKey: "key", Model: "m"}
	if _, err := d.Judge(context.Background(), state, qs); err == nil || !strings.Contains(err.Error(), "no answer") {
		t.Errorf("missing answer: %v", err)
	}
}

func TestRules(t *testing.T) {
	r := &Rules{T: DefaultThresholds}
	for _, c := range []struct {
		name string
		p    map[string]float64
		want Action
	}{
		{"nothing", map[string]float64{"voicemail_or_ivr": 0.3, "hostile": 0.2}, None},
		{"voicemail", map[string]float64{"voicemail_or_ivr": 0.95}, HangUp},
		{"hostile", map[string]float64{"hostile": 0.92}, HangUp},
		{"almost hostile", map[string]float64{"hostile": 0.85}, None},
		{"missing answers", map[string]float64{}, None},
	} {
		if got, why := r.Decide(c.p); got != c.want || (got == HangUp && why == "") {
			t.Errorf("%s: %q (%s), want %q", c.name, got, why, c.want)
		}
	}
}
