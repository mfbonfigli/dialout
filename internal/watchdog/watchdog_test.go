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
		answers["off_topic"] = map[string]any{"type": "noul", "noul": 0.91}
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
	if len(a.Probabilities) != 6 || a.Probabilities["off_topic"] != 0.91 || a.Probabilities["hostile"] != 0.25 || a.Cost != 0.0002 {
		t.Errorf("answers = %+v", a)
	}
	// The request: the model, the state as given, every question as a yes/no with its criteria.
	q := got["questions"].(map[string]any)["agent_said_goodbye"].(map[string]any)
	if got["model"] != "openai/gpt-6-luna-decisions" || q["type"] != "noul" || !strings.Contains(q["instructions"].(string), "goodbye") ||
		q["criteria"].(map[string]any)["true"] == "" || got["state"].(map[string]any)["objective"] != "Chiedi come sta." {
		t.Errorf("request = %v", got)
	}

	// Errors: HTTP status, a missing answer.
	d.APIKey = "wrong"
	if _, err := d.Judge(context.Background(), state, qs); err == nil || !strings.Contains(err.Error(), "HTTP 400") {
		t.Errorf("bad key: %v", err)
	}
	missing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"answers":{"objective_reached":{"type":"noul","noul":0.1}}}`))
	}))
	defer missing.Close()
	d = &Decisions{BaseURL: missing.URL, APIKey: "key", Model: "m"}
	if _, err := d.Judge(context.Background(), state, qs); err == nil || !strings.Contains(err.Error(), "no answer") {
		t.Errorf("missing answer: %v", err)
	}
}

func TestRules(t *testing.T) {
	p := func(kv ...any) map[string]float64 {
		m := map[string]float64{}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1].(float64)
		}
		return m
	}
	for _, c := range []struct {
		name string
		p    map[string]float64
		want Action
	}{
		{"nothing", p("objective_reached", 0.3, "off_topic", 0.2), None},
		{"voicemail", p("voicemail_or_ivr", 0.95, "agent_said_goodbye", 0.9), HangUp},
		{"hostile", p("hostile", 0.92), HangUp},
		{"almost hostile", p("hostile", 0.85), None},
		{"goodbye beats wrap up", p("agent_said_goodbye", 0.81, "objective_reached", 0.99), Close},
		{"objective", p("objective_reached", 0.8), WrapUp},
		{"callee wants to end", p("callee_wants_to_end", 0.9), WrapUp},
	} {
		r := &Rules{T: DefaultThresholds}
		if got, why := r.Decide(c.p); got != c.want {
			t.Errorf("%s: %q (%s), want %q", c.name, got, why, c.want)
		}
	}

	// Off topic acts on the second check in a row.
	r := &Rules{T: DefaultThresholds}
	if a, _ := r.Decide(p("off_topic", 0.75)); a != None {
		t.Errorf("first off-topic check: %q", a)
	}
	if a, why := r.Decide(p("off_topic", 0.8)); a != WrapUp || !strings.Contains(why, "twice") {
		t.Errorf("second off-topic check: %q %s", a, why)
	}
	r = &Rules{T: DefaultThresholds}
	r.Decide(p("off_topic", 0.8))
	r.Decide(p("off_topic", 0.1))
	if a, _ := r.Decide(p("off_topic", 0.8)); a != None {
		t.Errorf("streak not reset: %q", a)
	}
}
