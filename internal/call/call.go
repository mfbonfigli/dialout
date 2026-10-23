// Package call runs one outbound call end to end: the tunnel, the Twilio call with inline TwiML,
// the Media Stream, the conversation (a voice.Engine), the bridge between them and the recording.
// The MCP server and the local `dialout call` command both use it. The policy check happens
// before, in the caller.
package call

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/mfbonfigli/dialout/internal/recorder"
	"github.com/mfbonfigli/dialout/internal/twilio"
	"github.com/mfbonfigli/dialout/internal/voice"
	"github.com/mfbonfigli/dialout/internal/watchdog"
	"github.com/mfbonfigli/dialout/prompts"
)

// Timing is the choreography of a call. Tests shorten it.
type Timing struct {
	Poll            time.Duration // Twilio status polling before the stream connects
	StreamWait      time.Duration // after the answer, the stream must connect within this
	OpeningCue      time.Duration // silence after the stream starts before the opening cue
	WrapUp          time.Duration // the wrap-up cue comes this long before the limit
	AgentQuiet      time.Duration // every mark back and no agent audio for this long: the agent is silent
	EndNoAudio      time.Duration // after end_call: hang up if no goodbye has started by then
	EndMax          time.Duration // after end_call: hang up in any case
	OutcomeIdle     time.Duration // report_outcome without end_call: hang up after this much silence
	Overtime        time.Duration // at the limit: time to answer the final cue before the line drops
	Drain           time.Duration // after the hang-up: wait this long for Sonic's last transcripts
	TimeLimitMargin time.Duration // Twilio's TimeLimit is the maximum plus this (a backstop, > Overtime)
	Tick            time.Duration
}

func DefaultTiming() Timing {
	return Timing{
		Poll:            2 * time.Second,
		StreamWait:      60 * time.Second,
		OpeningCue:      1500 * time.Millisecond,
		WrapUp:          30 * time.Second,
		AgentQuiet:      700 * time.Millisecond,
		EndNoAudio:      3 * time.Second,
		EndMax:          20 * time.Second,
		OutcomeIdle:     5 * time.Second,
		Overtime:        10 * time.Second,
		Drain:           5 * time.Second,
		TimeLimitMargin: 15 * time.Second,
		Tick:            100 * time.Millisecond,
	}
}

// maxCall bounds a call's maximum duration: below Sonic's 8-minute stream, the shortest session
// limit of the engines.
const maxCall = 7 * time.Minute

// Config is what every call shares.
type Config struct {
	Twilio   *twilio.Client
	From     string // caller ID
	Streams  *Streams
	Tunnel   Starter           // the public ingress for each call: Tunnel, QuickTunnel or NoTunnel
	Engine   voice.Engine      // who holds the conversation
	Recorder recorder.Config   // the transcript and meta.json, the audio if Recorder.Audio; nothing if Recorder.Disabled
	Owner    string            // who the agent calls for
	Voice    string            // the engine's voice; its default when empty
	Prompts  *prompts.Set      // the prompt file; the built-in one when nil
	Location *time.Location    // the time zone of the date in the prompt; time.Local when nil
	Watchdog *watchdog.Config  // hangs up on a voicemail, an automated menu or a hostile callee; nil: none
	Notify   func(text string) // short progress notices, e.g. printed by the local command; nil: none
	Observe  func(Event)       // live progress, e.g. printed by the local command; nil: none
	Timing   Timing            // DefaultTiming() when zero
	Log      *slog.Logger
}

// Params is one call, already allowed by the policy.
type Params struct {
	CallID      string
	To          string          // E.164
	Label       string          // allowlist label, when known
	Brief       prompts.Brief   // Brief.Label defaults to Label
	MaxDuration time.Duration   // talk time, from the start of the stream
	RingTimeout time.Duration   // before no-answer
	Policy      json.RawMessage // the policy decision, stored in meta.json as is
}

// Event is live progress: Kind is status, cue, callee, agent, interrupted, outcome or end.
type Event struct {
	Kind string
	Text string
}

// Line is a transcript line.
type Line struct {
	T       float64 `json:"t"` // seconds since the stream started
	Speaker string  `json:"speaker"`
	Text    string  `json:"text"`
}

// Result is how the call went.
type Result struct {
	CallID       string
	Status       string // Twilio's last status: completed, no-answer, busy, failed, canceled…
	TwilioSID    string
	Answered     bool
	Talk         time.Duration // from the start of the stream to the hang-up
	EndReason    string
	Outcome      *voice.Outcome
	Transcript   []Line
	Usage        voice.Usage
	Watchdog     *WatchdogReport // with Config.Watchdog
	Recording    recorder.Result
	Err          error // why the call failed, if it did
	RecordingErr error // the recording is kept in the spool for recovery
}

// WatchdogReport is what the watchdog did during a call.
type WatchdogReport struct {
	Checks  int              `json:"checks"`
	Errors  int              `json:"errors,omitempty"`
	Cost    float64          `json:"cost,omitempty"`
	Actions []WatchdogAction `json:"actions,omitempty"`
}

// WatchdogAction is one decision the watchdog acted on, with the answers behind it.
type WatchdogAction struct {
	T             float64            `json:"t"` // seconds since the stream started
	Action        string             `json:"action"`
	Reason        string             `json:"reason"`
	Probabilities map[string]float64 `json:"probabilities"`
}

// meta is the call's meta.json.
type meta struct {
	CallID        string          `json:"call_id"`
	Status        string          `json:"status"`
	To            string          `json:"to"`
	Label         string          `json:"label,omitempty"`
	Callee        string          `json:"callee,omitempty"`
	Kind          string          `json:"kind,omitempty"`
	Opening       string          `json:"opening,omitempty"`
	Objective     string          `json:"objective"`
	Limits        string          `json:"limits,omitempty"`
	Context       string          `json:"context,omitempty"`
	Engine        string          `json:"engine"`
	Voice         string          `json:"voice"`
	Audio         bool            `json:"audio"` // the audio was recorded
	MaxSeconds    int             `json:"max_seconds"`
	Policy        json.RawMessage `json:"policy,omitempty"`
	TwilioSID     string          `json:"twilio_sid,omitempty"`
	Created       time.Time       `json:"created"`
	Answered      *time.Time      `json:"answered,omitempty"`
	StreamStarted *time.Time      `json:"stream_started,omitempty"`
	Ended         *time.Time      `json:"ended,omitempty"`
	EndReason     string          `json:"end_reason,omitempty"`
	Outcome       *voice.Outcome  `json:"outcome,omitempty"`
	Usage         voice.Usage     `json:"usage,omitempty"`
	Watchdog      *WatchdogReport `json:"watchdog,omitempty"`
	Error         string          `json:"error,omitempty"`
}

// Run places the call and returns when it is over and recorded. Cancelling ctx hangs up (or
// cancels a call still ringing); the clean-up runs anyway.
func Run(ctx context.Context, cfg Config, p Params) *Result {
	if cfg.Timing == (Timing{}) {
		cfg.Timing = DefaultTiming()
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	r := &run{cfg: cfg, p: p, t: cfg.Timing, res: &Result{CallID: p.CallID}}
	r.run(ctx)
	return r.res
}

type run struct {
	cfg   Config
	p     Params
	t     Timing
	res   *Result
	meta  meta
	vc    voice.Config // the conversation, as the engine prepared it
	rec   *recorder.Recorder
	start time.Time // the stream's start
}

func (r *run) log() *slog.Logger { return r.cfg.Log.With("call_id", r.p.CallID) }

func (r *run) observe(kind, text string) {
	if r.cfg.Observe != nil {
		r.cfg.Observe(Event{Kind: kind, Text: text})
	}
}

func (r *run) notify(text string) {
	if r.cfg.Notify != nil {
		r.cfg.Notify(text)
	}
}

func (r *run) who() string {
	if r.p.Label != "" {
		return r.p.Label
	}
	return r.p.To
}

func (r *run) metaJSON() []byte {
	b, _ := json.Marshal(r.meta)
	return b
}

func (r *run) run(ctx context.Context) {
	bg := context.WithoutCancel(ctx) // for what must finish even after a hang-up request
	now := time.Now()
	r.meta = meta{
		CallID: r.p.CallID, Status: "queued", To: r.p.To, Label: r.p.Label, Callee: r.p.Brief.Callee, Kind: r.p.Brief.Kind,
		Opening: r.p.Brief.Opening, Objective: r.p.Brief.Objective, Limits: r.p.Brief.Limits, Context: r.p.Brief.Context,
		Engine: r.cfg.Engine.Name(), Voice: r.cfg.Voice, Audio: r.cfg.Recorder.Audio,
		MaxSeconds: int(r.p.MaxDuration / time.Second), Policy: r.p.Policy, Created: now.UTC(),
	}
	r.res.Status = "queued"

	if r.p.MaxDuration <= r.t.WrapUp || r.p.MaxDuration > maxCall {
		r.res.Err = fmt.Errorf("call: maximum duration %v out of range", r.p.MaxDuration)
		r.res.Status = "failed"
		return
	}
	// The engine checks the brief now, so a mistake fails before anyone is rung.
	brief := r.p.Brief
	if brief.Label == "" {
		brief.Label = r.p.Label
	}
	loc := r.cfg.Location
	if loc == nil {
		loc = time.Local
	}
	vc, err := r.cfg.Engine.Prepare(voice.Config{
		Owner: r.cfg.Owner, Voice: r.cfg.Voice, Brief: brief, Prompts: r.cfg.Prompts, Now: now.In(loc),
	})
	if err == nil && vc.Prompt == nil {
		err = fmt.Errorf("call: engine %s rendered no prompt", r.cfg.Engine.Name())
	}
	if err != nil {
		r.res.Err, r.res.Status = err, "failed"
		return
	}
	r.vc = vc
	r.meta.Voice = vc.Voice
	if r.cfg.Tunnel == nil {
		r.res.Err, r.res.Status = errors.New("call: no tunnel"), "failed"
		return
	}
	token, err := twilio.NewStreamToken()
	if err != nil {
		r.res.Err, r.res.Status = err, "failed"
		return
	}
	// The call's record (meta.json and the transcript; the audio with Recorder.Audio) starts
	// first, so every attempt leaves a meta.json. With Recorder.Disabled nothing is stored.
	r.rec, err = recorder.Start(r.cfg.Recorder, r.p.CallID, now, r.metaJSON())
	if err != nil {
		r.res.Err, r.res.Status = err, "failed"
		return
	}
	defer r.finish(bg)

	r.notify(fmt.Sprintf("📞 Calling %s: %s", r.who(), r.p.Brief.Objective))
	// The ingress comes up before anyone is rung, and gives the hostname for this call's TwiML: a
	// quick tunnel's is new every time.
	r.observe("status", "starting the tunnel")
	host, stop, err := r.cfg.Tunnel.Start(ctx)
	if err != nil {
		r.res.Err, r.res.Status = err, "failed"
		return
	}
	defer stop()
	r.observe("status", "tunnel on "+host)
	twiml, err := twilio.StreamTwiML("wss://"+host+"/twilio/stream/"+token, map[string]string{"call_id": r.p.CallID})
	if err != nil {
		r.res.Err, r.res.Status = err, "failed"
		return
	}
	exp := r.cfg.Streams.Expect(token, host, time.Now().Add(r.p.RingTimeout+r.t.StreamWait))
	defer exp.Cancel()

	call, err := r.cfg.Twilio.CreateCall(bg, twilio.CreateCallParams{
		To: r.p.To, From: r.cfg.From, TwiML: twiml,
		Timeout: r.p.RingTimeout, TimeLimit: r.p.MaxDuration + r.t.TimeLimitMargin,
	})
	if err != nil {
		r.res.Err, r.res.Status = err, "failed"
		return
	}
	r.res.TwilioSID, r.res.Status = call.SID, call.Status
	r.meta.TwilioSID = call.SID
	r.observe("status", "call created ("+call.SID+")")

	st := r.waitStream(ctx, bg, exp)
	if st == nil {
		return
	}
	defer st.Close(websocket.StatusNormalClosure, "")
	r.bridge(ctx, bg, st)
	r.hangUp(bg)
	r.finalStatus(bg)
}

// waitStream polls Twilio until the call is answered and its media stream connects. It returns
// nil when the call ended first (no answer, busy, failed), was cancelled, or never streamed.
func (r *run) waitStream(ctx, bg context.Context, exp *Expected) *Stream {
	poll := time.NewTicker(r.t.Poll)
	defer poll.Stop()
	var deadline <-chan time.Time
	status := ""
	for {
		select {
		case st := <-exp.C():
			return st
		case <-ctx.Done():
			r.res.EndReason = "canceled"
			r.hangUp(bg)
			r.finalStatus(bg)
			return nil
		case <-deadline:
			r.res.Err = errors.New("call: answered, but the media stream never connected")
			r.res.EndReason = "no_stream"
			r.hangUp(bg)
			r.finalStatus(bg)
			return nil
		case <-poll.C:
			c, err := r.cfg.Twilio.FetchCall(bg, r.res.TwilioSID)
			if err != nil {
				r.log().Warn("calls: polling the call", "err", err)
				continue
			}
			r.res.Status = c.Status
			if c.Status != status {
				status = c.Status
				r.observe("status", c.Status)
			}
			if c.Final() {
				r.res.EndReason = c.Status
				return nil
			}
			if c.Status == "in-progress" && !r.res.Answered {
				now := time.Now()
				r.res.Answered = true
				r.meta.Answered = ptr(now.UTC())
				exp.SetExpiry(now.Add(r.t.StreamWait))
				deadline = time.After(r.t.StreamWait)
			}
		}
	}
}

// bridge runs the conversation on an accepted stream.
func (r *run) bridge(ctx, bg context.Context, st *Stream) {
	streamSID, err := r.readStart(bg, st)
	if err != nil {
		r.res.Err = err
		r.res.EndReason = "invalid_stream"
		st.Close(websocket.StatusPolicyViolation, "unexpected stream")
		return
	}
	r.start = time.Now()
	r.res.Answered = true
	r.meta.StreamStarted = ptr(r.start.UTC())
	_ = r.rec.SetMeta(r.metaJSON())
	r.observe("status", "answered")

	sctx, cancel := context.WithCancel(bg)
	defer cancel()
	sess, err := r.cfg.Engine.Dial(sctx, r.vc)
	if err != nil {
		r.res.Err = fmt.Errorf("call: %w", err) // engine errors carry the engine's name already
		r.res.EndReason = r.cfg.Engine.Name() + " non disponibile"
		return
	}
	defer sess.Close()

	b := newBridge(r, st.WS, streamSID, sess)
	r.res.EndReason = b.run(ctx)
	r.res.Talk = time.Since(r.start)
	r.observe("status", "end: "+r.res.EndReason)

	// Hang up first, so the callee isn't left in silence; then let Sonic close and deliver its last
	// transcripts (the goodbye's text comes after its audio).
	r.hangUp(bg)
	_ = sess.End()
	b.drain(r.t.Drain)
	if err := sess.Close(); err != nil && r.res.Err == nil && !strings.Contains(r.res.EndReason, "sonic") {
		r.log().Warn("calls: sonic session", "err", err)
	}
}

// readStart reads the stream's first messages and checks that the start message is for this
// call: its CallSid, its account and the call_id parameter.
func (r *run) readStart(ctx context.Context, st *Stream) (streamSID string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for {
		_, data, err := st.WS.Read(ctx)
		if err != nil {
			return "", fmt.Errorf("call: reading the stream start: %w", err)
		}
		m, err := twilio.ParseInbound(data)
		if err != nil {
			return "", err
		}
		switch m.Event {
		case "connected":
			continue
		case "start":
			s := m.Start
			if s.CallSID != r.res.TwilioSID || s.AccountSID != r.cfg.Twilio.AccountSID || s.CustomParameters["call_id"] != r.p.CallID {
				r.log().Error("calls: stream for another call refused", "call_sid", s.CallSID, "account", s.AccountSID)
				return "", errors.New("call: the stream's start message names another call")
			}
			return s.StreamSID, nil
		default:
			return "", fmt.Errorf("call: unexpected %q before the stream start", m.Event)
		}
	}
}

// hangUp ends the call at Twilio; ending a call that is already over is fine.
func (r *run) hangUp(ctx context.Context) {
	if r.res.TwilioSID == "" {
		return
	}
	if c, err := r.cfg.Twilio.EndCall(ctx, r.res.TwilioSID); err != nil {
		r.log().Error("calls: hanging up", "err", err)
	} else {
		r.res.Status = c.Status
	}
}

// finalStatus waits briefly for Twilio's final status after the hang-up.
func (r *run) finalStatus(ctx context.Context) {
	for range 5 {
		c, err := r.cfg.Twilio.FetchCall(ctx, r.res.TwilioSID)
		if err == nil {
			r.res.Status = c.Status
			if c.Final() {
				return
			}
		}
		time.Sleep(r.t.Poll / 2)
	}
}

// finish writes the final meta.json, completes the recording and sends the closing notice.
func (r *run) finish(ctx context.Context) {
	now := time.Now().UTC()
	r.meta.Status = r.res.Status
	r.meta.Ended = &now
	r.meta.EndReason = r.res.EndReason
	r.meta.Outcome = r.res.Outcome
	r.meta.Usage = r.res.Usage
	r.meta.Watchdog = r.res.Watchdog
	if r.res.Err != nil {
		r.meta.Error = r.res.Err.Error()
	}
	res, err := r.rec.Finish(ctx, r.metaJSON())
	r.res.Recording = res
	if err != nil {
		r.res.RecordingErr = err
		r.log().Error("calls: recording kept in the spool for recovery", "err", err)
	}

	switch {
	case r.res.Err != nil:
		r.notify(fmt.Sprintf("⚠️ Call to %s failed: %v", r.who(), r.res.Err))
	case !r.res.Answered:
		r.notify(fmt.Sprintf("📵 %s didn't answer (%s)", r.who(), r.res.Status))
	default:
		text := fmt.Sprintf("✅ Call with %s ended (%s)", r.who(), r.res.Talk.Round(time.Second))
		if o := r.res.Outcome; o != nil {
			text += ": " + o.Result + ". " + o.Summary
		} else {
			text += ", no outcome reported"
		}
		if res.RecordingKey != "" {
			text += "\n" + res.RecordingKey
		}
		r.notify(text)
	}
}

func ptr[T any](v T) *T { return &v }
