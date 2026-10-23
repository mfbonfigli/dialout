// Package prompts renders what the voice agent is told: the system prompt, the cues sent while
// the call runs, the closing sentence for each kind of engine, the hints in Sonic's tool results
// and the tools' descriptions.
//
// They come from a prompt file: a text/template file of named blocks. The built-in one
// (builtin.tmpl, Italian, compiled into the binary) is the default; the owner's file overrides any
// of its blocks. Whoever places a call only fills in the brief, which is inserted as data.
//
// The built-in prompt states a few conversational principles rather than a list of prohibitions:
// the first test calls showed that a long list made the agent recite it or give up at the first
// unforeseen request. The hard safety lives outside it (allowlist, policy, no payments).
package prompts

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"text/template"
	"text/template/parse"
	"time"
	"unicode/utf8"
)

//go:embed builtin.tmpl
var builtinText string

// Block names. A prompt file may define any of them, and no others.
const (
	BlockSystem           = "system"
	BlockToolsThenGoodbye = "closing.tools_then_goodbye"
	BlockWithGoodbye      = "closing.with_goodbye"
	BlockCueOpening       = "cue.opening"
	BlockCueWrapUp        = "cue.wrap_up"
	BlockCueFinal         = "cue.final"
	BlockNextEndCall      = "tool.next.end_call"
	BlockNextGoodbye      = "tool.next.goodbye"
	BlockReportOutcome    = "tool.report_outcome"
	BlockResult           = "tool.report_outcome.result"
	BlockSummary          = "tool.report_outcome.summary"
	BlockDetails          = "tool.report_outcome.details"
	BlockEndCall          = "tool.end_call"
	BlockReason           = "tool.end_call.reason"
	BlockCueWatchdog      = "cue.watchdog_wrap_up"
)

// The watchdog's questions. Each has three blocks: "watchdog.<name>" (the question) and
// "watchdog.<name>.true" / ".false" (what yes and no mean).
const (
	QObjectiveReached = "objective_reached"
	QAgentGoodbye     = "agent_said_goodbye"
	QCalleeWantsToEnd = "callee_wants_to_end"
	QOffTopic         = "off_topic"
	QVoicemail        = "voicemail_or_ivr"
	QHostile          = "hostile"
)

// WatchdogQuestions are the questions the watchdog asks after every turn, in order.
var WatchdogQuestions = []string{QObjectiveReached, QAgentGoodbye, QCalleeWantsToEnd, QOffTopic, QVoicemail, QHostile}

var blocks = func() []string {
	b := []string{
		BlockSystem, BlockToolsThenGoodbye, BlockWithGoodbye, BlockCueOpening, BlockCueWrapUp, BlockCueFinal,
		BlockNextEndCall, BlockNextGoodbye, BlockReportOutcome, BlockResult, BlockSummary, BlockDetails,
		BlockEndCall, BlockReason, BlockCueWatchdog,
	}
	for _, q := range WatchdogQuestions {
		b = append(b, "watchdog."+q, "watchdog."+q+".true", "watchdog."+q+".false")
	}
	return b
}()

// Kinds of callee: who answers decides the register.
const (
	KindBusiness = "business" // shops, restaurants, offices: they don't know the owner
	KindPersonal = "personal" // friends and family
)

// ValidKind reports whether k is a kind of callee, or empty.
func ValidKind(k string) bool { return k == "" || k == KindBusiness || k == KindPersonal }

const maxOwner = 80 // characters

// Closing is how an engine ends a call, which decides the closing sentence of the prompt.
type Closing int

const (
	// ToolsThenGoodbye is for Sonic, which ends its turn as soon as it speaks: a goodbye before the
	// tools meant end_call never came.
	ToolsThenGoodbye Closing = iota
	// WithGoodbye is for engines driven by a full language model (ElevenLabs, OpenAI), which can
	// speak and call tools in the same turn: the call ends right after the goodbye.
	WithGoodbye
)

// Brief is what the agent is told about one call: who answers and why it calls. The caller of
// place_call writes it; Callee and Kind may come from the allowlist entry instead, Label always
// does.
type Brief struct {
	Callee    string // who answers, as the agent should refer to them; optional
	Kind      string // KindBusiness, KindPersonal or empty
	Label     string // the allowlist label, from the owner's point of view; optional
	Opening   string // the first sentence, spoken word for word; optional
	Objective string // why the agent calls: what to obtain or say, the preferred option included
	Limits    string // what is fine: margins, ceilings, defaults, flexibility; never said; optional
	Context   string // facts the agent knows and may share; optional
}

// Vars are the variables every block sees.
type Vars struct {
	Owner   string    // who the agent calls for
	Female  bool      // the voice's grammatical gender
	Now     time.Time // the call's start, in the configured time zone
	Closing string    // the engine's closing block, rendered (meant for the system block)
	Brief
}

// Rendered is everything one call needs from the prompt file.
type Rendered struct {
	System   string
	Cues     Cues
	Hints    Hints
	Tools    ToolTexts
	Watchdog []Question // in WatchdogQuestions order
}

// Cues are the lines the call engine sends while the call runs.
type Cues struct {
	Opening  string // the callee stays silent after answering
	WrapUp   string // 30 seconds before the time limit
	Final    string // at the time limit
	Watchdog string // the watchdog decided the call should wrap up
}

// Question is one of the watchdog's yes/no questions.
type Question struct {
	Name, Instructions, True, False string
}

// Hints are the next step Sonic gets in its tool results.
type Hints struct {
	EndCall string // after report_outcome: now call end_call
	Goodbye string // after end_call: now say goodbye
}

// ToolTexts are the descriptions of the agent's tools and of their fields.
type ToolTexts struct {
	ReportOutcome string
	Result        string
	Summary       string
	Details       string
	EndCall       string
	Reason        string
}

// Set is a loaded prompt file: the built-in blocks with the owner's overrides.
type Set struct {
	t      *template.Template
	Source string // the file it came from; empty for the built-in one
}

var builtin = mustBuiltin()

func mustBuiltin() *Set {
	t, err := template.New("builtin").Option("missingkey=error").Funcs(funcs).Parse(builtinText)
	if err != nil {
		panic(err)
	}
	s := &Set{t: t}
	if err := s.check(); err != nil {
		panic(err)
	}
	return s
}

// Builtin returns the built-in prompt file.
func Builtin() *Set { return builtin }

// Load reads a prompt file. An empty path means the built-in one.
func Load(path string) (*Set, error) {
	if path == "" {
		return builtin, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("prompt file: %w", err)
	}
	s, err := Parse(path, string(b))
	if err != nil {
		return nil, err
	}
	s.Source = path
	return s, nil
}

// maxFile bounds a prompt file.
const maxFile = 64 << 10

// Parse reads a prompt file's text. It must define the system block, may define any other block,
// and has nothing outside blocks but comments and blank space. Every block is rendered with sample
// briefs, so a mistake shows now and not on a call.
func Parse(name, text string) (*Set, error) {
	if len(text) > maxFile {
		return nil, fmt.Errorf("prompt file %s: larger than %d KiB", name, maxFile>>10)
	}
	own, err := template.New(name).Option("missingkey=error").Funcs(funcs).Parse(text)
	if err != nil {
		return nil, fmt.Errorf("prompt file: %w", err)
	}
	if strings.TrimSpace(textOutsideBlocks(own)) != "" {
		return nil, fmt.Errorf("prompt file %s: text outside {{define}} blocks", name)
	}
	merged, err := builtin.t.Clone()
	if err != nil {
		return nil, err
	}
	hasSystem := false
	for _, t := range own.Templates() {
		if t.Name() == name || t.Tree == nil {
			continue
		}
		if !slices.Contains(blocks, t.Name()) {
			return nil, fmt.Errorf("prompt file %s: unknown block %q", name, t.Name())
		}
		hasSystem = hasSystem || t.Name() == BlockSystem
		if _, err := merged.AddParseTree(t.Name(), t.Tree); err != nil {
			return nil, fmt.Errorf("prompt file %s: %w", name, err)
		}
	}
	if !hasSystem {
		return nil, fmt.Errorf("prompt file %s: no %q block", name, BlockSystem)
	}
	s := &Set{t: merged}
	if err := s.check(); err != nil {
		return nil, fmt.Errorf("prompt file %s: %w", name, err)
	}
	return s, nil
}

// textOutsideBlocks returns the text of the file itself, without its define blocks and comments.
func textOutsideBlocks(t *template.Template) string {
	if t.Tree == nil || t.Tree.Root == nil {
		return ""
	}
	var b strings.Builder
	for _, n := range t.Tree.Root.Nodes {
		switch n := n.(type) {
		case *parse.TextNode:
			b.Write(n.Text)
		case *parse.CommentNode:
		default:
			b.WriteString(n.String())
		}
	}
	return b.String()
}

// check renders every block with sample briefs: every field set and the optional ones empty, each
// kind, both genders and both closings.
func (s *Set) check() error {
	full := Brief{Callee: "Callee", Label: "Label", Opening: "Opening.", Objective: "Objective.",
		Limits: "Limits.", Context: "Context."}
	now := time.Date(2026, 10, 23, 18, 30, 0, 0, time.UTC)
	for _, kind := range []string{"", KindBusiness, KindPersonal} {
		for _, b := range []Brief{full, {Objective: "Objective."}} {
			b.Kind = kind
			for _, female := range []bool{true, false} {
				for _, c := range []Closing{ToolsThenGoodbye, WithGoodbye} {
					if _, err := s.Render(c, Vars{Owner: "Owner", Female: female, Now: now, Brief: b}); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// Render renders one call's prompt. v.Closing is filled from c, the brief is trimmed, and a zero
// v.Now means now.
func (s *Set) Render(c Closing, v Vars) (*Rendered, error) {
	if err := ValidOwner(v.Owner); err != nil {
		return nil, err
	}
	if !ValidKind(v.Kind) {
		return nil, fmt.Errorf("prompts: kind %q: use %s or %s", v.Kind, KindBusiness, KindPersonal)
	}
	v.Owner = strings.TrimSpace(v.Owner)
	b := &v.Brief
	for _, f := range []*string{&b.Callee, &b.Label, &b.Opening, &b.Objective, &b.Limits, &b.Context} {
		*f = strings.TrimSpace(*f)
	}
	if v.Now.IsZero() {
		v.Now = time.Now()
	}
	var err error
	exec := func(name string, oneLine bool) string {
		if err != nil {
			return ""
		}
		var out strings.Builder
		if e := s.t.ExecuteTemplate(&out, name, v); e != nil {
			err = fmt.Errorf("prompts: %w", e)
			return ""
		}
		text := strings.TrimSpace(out.String())
		switch {
		case text == "":
			err = fmt.Errorf("prompts: block %q renders empty", name)
		case oneLine && strings.ContainsAny(text, "\r\n"):
			err = fmt.Errorf("prompts: block %q must be one line", name)
		}
		return text
	}
	closing := BlockToolsThenGoodbye
	if c == WithGoodbye {
		closing = BlockWithGoodbye
	}
	v.Closing = exec(closing, false)
	r := &Rendered{
		System: exec(BlockSystem, false),
		Cues: Cues{Opening: exec(BlockCueOpening, true), WrapUp: exec(BlockCueWrapUp, true), Final: exec(BlockCueFinal, true),
			Watchdog: exec(BlockCueWatchdog, true)},
		Hints: Hints{EndCall: exec(BlockNextEndCall, true), Goodbye: exec(BlockNextGoodbye, true)},
		Tools: ToolTexts{
			ReportOutcome: exec(BlockReportOutcome, false), Result: exec(BlockResult, false),
			Summary: exec(BlockSummary, false), Details: exec(BlockDetails, false),
			EndCall: exec(BlockEndCall, false), Reason: exec(BlockReason, false),
		},
	}
	for _, q := range WatchdogQuestions {
		b := "watchdog." + q
		r.Watchdog = append(r.Watchdog, Question{Name: q, Instructions: exec(b, false), True: exec(b+".true", true), False: exec(b+".false", true)})
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

// ValidOwner checks the owner's name: one line, not empty, at most 80 characters.
func ValidOwner(owner string) error {
	owner = strings.TrimSpace(owner)
	switch {
	case owner == "":
		return errors.New("prompts: the owner's name is empty")
	case strings.ContainsAny(owner, "\r\n"):
		return errors.New("prompts: the owner's name spans several lines")
	case utf8.RuneCountInString(owner) > maxOwner:
		return errors.New("prompts: the owner's name is too long")
	}
	return nil
}

// funcs pick day and month names from a list in the prompt's language, since Go formats dates in
// English only: {{weekday .Now "domenica lunedì …"}} (Sunday first), {{month .Now "gennaio …"}}.
var funcs = template.FuncMap{
	"weekday": func(t time.Time, names string) (string, error) {
		return pick(names, int(t.Weekday()), 7, "weekday")
	},
	"month": func(t time.Time, names string) (string, error) {
		return pick(names, int(t.Month())-1, 12, "month")
	},
}

func pick(names string, i, n int, fn string) (string, error) {
	f := strings.Fields(names)
	if len(f) != n {
		return "", fmt.Errorf("%s needs %d names, got %d", fn, n, len(f))
	}
	return f[i], nil
}
