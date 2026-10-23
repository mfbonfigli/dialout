package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"strings"
	"time"

	"github.com/mfbonfigli/dialout/internal/app"
	"github.com/mfbonfigli/dialout/internal/call"
	"github.com/mfbonfigli/dialout/internal/config"
	"github.com/mfbonfigli/dialout/internal/policy"
	"github.com/mfbonfigli/dialout/prompts"
)

// cmdCall places one call from this machine: the same engine and policy as the server, with the
// conversation printed as it happens and the transcript (and the audio, if asked) stored.
func cmdCall(args []string) int {
	fl := flag.NewFlagSet("call", flag.ContinueOnError)
	cfgPath := fl.String("config", "", "configuration file (default: $DIALOUT_CONFIG, then the usual places)")
	to := fl.String("to", "", "number to call (+39…) or allowlist label")
	objective := fl.String("objective", "", "why the agent calls, preferred option included; the agent may say it (required)")
	limits := fl.String("limits", "", "private rules for deciding: margins, ceilings, defaults; never said (optional)")
	opening := fl.String("opening", "", "the first sentence, word for word (optional)")
	context_ := fl.String("context", "", "other facts the agent may share (optional)")
	callee := fl.String("callee", "", "who answers, e.g. \"la pizzeria Da Mario\" (default: the allowlist entry's name)")
	kind := fl.String("kind", "", "business or personal (default: the allowlist entry's kind)")
	engineName := fl.String("engine", "", "sonic, elevenlabs or openai (default: engine.name)")
	voiceName := fl.String("voice", "", "the engine's voice (default: the configured one)")
	record := fl.Bool("record", false, "record the audio too (default: storage.record_audio)")
	noStorage := fl.Bool("no-storage", false, "store nothing on disk: no transcript, no audio (default: storage.enabled)")
	promptFile := fl.String("prompt", "", "prompt file for this call (default: prompt.file)")
	minutes := fl.Int("max-minutes", 0, "maximum talk time in minutes; 0 = the policy maximum")
	noTunnel := fl.Bool("no-tunnel", false, "start no cloudflared: tunnel.hostname already reaches tunnel.listen")
	yes := fl.Bool("yes", false, "don't ask for confirmation")
	verbose := fl.Bool("v", false, "debug logs (including cloudflared's)")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	if fl.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "unexpected arguments: %v\n", fl.Args())
		return 2
	}

	path, err := config.Find(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	// One-call overrides.
	if *engineName != "" {
		cfg.Engine.Name = *engineName
		if err := cfg.Check(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	}
	if flagSet(fl, "record") {
		cfg.Storage.RecordAudio = *record
	}
	if *noStorage {
		cfg.Storage.Enabled, cfg.Storage.RecordAudio = false, false
	}
	if *promptFile != "" {
		if cfg.Prompts, err = prompts.Load(*promptFile); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	}

	// The same policy as the server. The history isn't shared: the server's limits don't count
	// calls made here.
	d := policy.Decide(cfg.PolicyConfig(), policy.Request{
		To: *to, Callee: *callee, Kind: *kind, Opening: *opening, Objective: *objective, Limits: *limits,
		Context: *context_, MaxMinutes: *minutes,
	}, policy.State{}, time.Now())
	if !d.Allowed {
		fmt.Fprintf(os.Stderr, "🚫 call refused (%s): %s\n", d.Reason, d.Detail)
		return 2
	}

	level := slog.LevelWarn
	log := newLogger(*verbose, level)
	a, err := app.Build(cfg, app.Options{NoTunnel: *noTunnel, Log: log})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	brief := prompts.Brief{Callee: d.Callee, Kind: d.CalleeKind, Label: d.Label, Opening: *opening,
		Objective: *objective, Limits: *limits, Context: *context_}
	cc := a.CallConfig(cfg, func(text string) { fmt.Println(text) }, nil)
	if *voiceName != "" {
		cc.Voice = *voiceName
	}

	who := d.Number
	if d.Label != "" {
		who = d.Label + " " + d.Number
	}
	fmt.Printf("Calling %s (%s, %s), from %s\n", who, d.Country, d.Kind, cfg.Twilio.From)
	if brief.Callee != "" {
		fmt.Printf("  callee:    %s (%s)\n", brief.Callee, orDash(brief.Kind))
	}
	if brief.Opening != "" {
		fmt.Printf("  opening:   %s\n", brief.Opening)
	}
	fmt.Printf("  objective: %s\n", brief.Objective)
	if brief.Limits != "" {
		fmt.Printf("  limits:    %s\n", brief.Limits)
	}
	if brief.Context != "" {
		fmt.Printf("  context:   %s\n", brief.Context)
	}
	voiceDesc := "voice " + cc.Voice
	if cc.Voice == "" {
		voiceDesc = "the agent's own voice"
	}
	fmt.Printf("  for %s, %s with %s, at most %d min\n", cfg.Owner, a.Engine.Name(), voiceDesc, d.MaxSeconds/60)
	if cfg.Prompts.Source != "" {
		fmt.Printf("  prompt: %s\n", cfg.Prompts.Source)
	}
	switch {
	case *noTunnel:
		fmt.Printf("  ingress: %s (already running)\n", cfg.Tunnel.Hostname)
	case cfg.Tunnel.Hostname != "":
		fmt.Printf("  tunnel: %s\n", cfg.Tunnel.Hostname)
	default:
		fmt.Println("  tunnel: quick, on a random trycloudflare.com name")
	}
	if a.Watchdog != nil {
		fmt.Printf("  watchdog: %s\n", cfg.Watchdog.Model)
	}
	switch {
	case !cfg.Storage.Enabled:
		fmt.Println("  storage: off (nothing written to disk)")
	case cfg.Storage.RecordAudio:
		fmt.Println("  storage: transcript and audio")
	default:
		fmt.Println("  storage: transcript only")
	}
	if !*yes && !confirm("Go ahead? [y/N] ") {
		fmt.Println("Cancelled.")
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go func() {
		<-ctx.Done()
		stop() // a second Ctrl-C quits at once
		fmt.Println("\nHanging up… (Ctrl-C again to quit now)")
	}()

	// A run killed in the middle of a call leaves its spool: finish it first, as the server does.
	if recs, err := a.Recover(ctx, nil); err != nil {
		fmt.Fprintln(os.Stderr, "recovering the spool:", err)
	} else {
		for _, r := range recs {
			fmt.Printf("Recovered the interrupted call %s: %s (%v)\n", r.CallID, a.Where(r.Result.Prefix), r.Err)
		}
	}
	streamsStop, err := a.Serve()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer streamsStop()

	started := time.Now()
	cc.Observe = printer(started)
	decision, _ := json.Marshal(d)
	res := call.Run(ctx, cc, call.Params{
		CallID:      newCallID(),
		To:          d.Number,
		Label:       d.Label,
		Brief:       brief,
		MaxDuration: time.Duration(d.MaxSeconds) * time.Second,
		RingTimeout: time.Duration(cfg.Policy.Limits.RingSeconds) * time.Second,
		Policy:      decision,
	})

	fmt.Println()
	fmt.Printf("Status: %s", res.Status)
	if res.Answered {
		fmt.Printf(", on the line %s (%s)", res.Talk.Round(time.Second), res.EndReason)
	}
	fmt.Println()
	if o := res.Outcome; o != nil {
		fmt.Printf("Outcome: %s. %s\n", o.Result, o.Summary)
		if o.Details != "" {
			fmt.Printf("Details: %s\n", o.Details)
		}
	}
	if res.Recording.RecordingKey != "" {
		fmt.Println("Recording:", a.Where(res.Recording.RecordingKey))
	}
	if res.Recording.Prefix != "" {
		fmt.Println("Transcript and meta:", a.Where(res.Recording.Prefix))
	}
	if len(res.Usage) > 0 {
		keys := make([]string, 0, len(res.Usage))
		for k := range res.Usage {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		var parts []string
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s=%d", k, res.Usage[k]))
		}
		fmt.Printf("Usage (%s): %s\n", a.Engine.Name(), strings.Join(parts, ", "))
	}
	if res.RecordingErr != nil {
		fmt.Fprintln(os.Stderr, "Recording kept in the spool (recovered at the next start):", res.RecordingErr)
	}
	if res.Err != nil {
		fmt.Fprintln(os.Stderr, "Error:", res.Err)
		return 1
	}
	return 0
}

// printer prints the call's progress with the time since the command started.
func printer(start time.Time) func(call.Event) {
	icons := map[string]string{
		"callee": "👤", "agent": "🤖", "cue": "💬", "interrupted": "✋", "outcome": "📋", "watchdog": "🐕",
	}
	return func(e call.Event) {
		el := time.Since(start)
		stamp := fmt.Sprintf("[%02d:%02d]", int(el.Minutes()), int(el.Seconds())%60)
		switch e.Kind {
		case "interrupted":
			fmt.Println(stamp, icons[e.Kind], "interrupted")
		case "callee", "agent", "cue", "outcome", "watchdog":
			fmt.Println(stamp, icons[e.Kind], e.Text)
		default:
			fmt.Println(stamp, "·", e.Text)
		}
	}
}

func confirm(prompt string) bool {
	fmt.Print(prompt)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes", "s", "si", "sì":
		return true
	}
	return false
}

// flagSet reports whether the flag was given on the command line.
func flagSet(fl *flag.FlagSet, name string) bool {
	set := false
	fl.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return set
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// newCallID is sortable and readable: 20261022-153012-a1b2c3 (UTC).
func newCallID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b)
}
