package policy

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mfbonfigli/dialout/prompts"
)

// Mode is the dialling mode. Phase 1 implements only ModeAllowlist; any other value refuses calls.
type Mode string

const ModeAllowlist Mode = "allowlist"

// Config is the policy in force: the kill switch, the mode, the allowlist and the limits.
type Config struct {
	Enabled   bool
	Mode      Mode
	Allowlist []Entry // already validated with ValidateAllowlist

	MaxPerHour      int
	MaxPerDay       int
	MaxCallee       int // characters
	MaxOpening      int // characters
	MaxObjective    int // characters
	MaxContext      int // characters
	MaxLimits       int // characters
	MaxCallSeconds  int
	MaintenanceHold bool // a maintenance window has been announced
}

// Request is what the caller of place_call asks for.
type Request struct {
	To         string // E.164, any common Italian format, or an allowlist label
	Callee     string // who answers; optional, overrides the allowlist entry's name
	Kind       string // business or personal; optional, overrides the allowlist entry's kind
	Opening    string // how the agent opens the call; optional
	Objective  string // what to obtain, in words the agent may say
	Limits     string // the private rules for deciding (margins, ceilings); never said to the callee
	Context    string // facts the agent may say if asked
	MaxMinutes int    // 0 = policy maximum
}

// State is the call history the limits are checked against.
type State struct {
	Active  bool        // a call is in progress (one concurrent call)
	Started []time.Time // start times of calls placed (successful or not) in the last 24 h at least
}

// Reason codes. They are returned to the caller and logged; keep them stable.
const (
	ReasonOK             = "ok"
	ReasonDisabled       = "disabled"
	ReasonMaintenance    = "maintenance"
	ReasonBadMode        = "unsupported_mode"
	ReasonBadNumber      = "invalid_number"
	ReasonNotAllowlisted = "not_allowlisted"
	ReasonBusy           = "call_in_progress"
	ReasonHourlyLimit    = "hourly_limit"
	ReasonDailyLimit     = "daily_limit"
	ReasonCallee         = "invalid_callee"
	ReasonKind           = "invalid_kind"
	ReasonOpening        = "invalid_opening"
	ReasonObjective      = "invalid_objective"
	ReasonContext        = "invalid_context"
	ReasonLimits         = "invalid_limits"
	ReasonDuration       = "invalid_duration"
)

// Decision is the outcome of Decide.
type Decision struct {
	Allowed    bool
	Reason     string // one of the Reason* codes
	Detail     string // human-readable explanation
	Number     string // E.164, set when the number was resolved
	Label      string // allowlist label, when known
	Kind       Kind   // the number's type
	Callee     string // who answers: the request's, else the allowlist entry's name
	CalleeKind string // business, personal or empty: the request's, else the entry's
	MaxSeconds int    // call length to enforce
}

func deny(reason, detail string) Decision { return Decision{Reason: reason, Detail: detail} }

// Decide applies the checks in order. It never has side effects: the caller records
// the call in State only when it actually goes on to place it.
func Decide(cfg Config, req Request, st State, now time.Time) Decision {
	if !cfg.Enabled {
		return deny(ReasonDisabled, "phone calls are disabled (policy.enabled)")
	}
	if cfg.MaintenanceHold {
		return deny(ReasonMaintenance, "a maintenance window is about to start; try again after it")
	}
	if cfg.Mode != ModeAllowlist {
		return deny(ReasonBadMode, "unsupported dialling mode "+string(cfg.Mode))
	}

	// Resolve the destination: an allowlist label (case-insensitive) or a number.
	to := strings.TrimSpace(req.To)
	var number, label string
	var kind Kind
	var entry *Entry
	for i, e := range cfg.Allowlist {
		if strings.EqualFold(e.Label, to) {
			number, label, entry = e.Number, e.Label, &cfg.Allowlist[i]
			break
		}
	}
	if number == "" {
		n, k, err := NormalizeItalian(to)
		if err != nil {
			return deny(ReasonBadNumber, err.Error())
		}
		number, kind = n, k
	}
	// Re-check the number type even for allowlist entries (defence in depth: the list is
	// validated on load, but this keeps the rule in one place).
	n, k, err := NormalizeItalian(number)
	if err != nil {
		return deny(ReasonBadNumber, err.Error())
	}
	number, kind = n, k

	if label == "" {
		for i, e := range cfg.Allowlist {
			if e.Number == number {
				label, entry = e.Label, &cfg.Allowlist[i]
				break
			}
		}
	}
	if label == "" { // mode allowlist: only listed numbers
		return Decision{Reason: ReasonNotAllowlisted, Detail: number + " is not on the allowlist", Number: number, Kind: kind}
	}

	if st.Active {
		return deny(ReasonBusy, "another call is in progress")
	}
	var lastHour, lastDay int
	for _, t := range st.Started {
		age := now.Sub(t)
		if age < 0 {
			continue
		}
		if age < time.Hour {
			lastHour++
		}
		if age < 24*time.Hour {
			lastDay++
		}
	}
	if cfg.MaxPerHour > 0 && lastHour >= cfg.MaxPerHour {
		return deny(ReasonHourlyLimit, "hourly call limit reached")
	}
	if cfg.MaxPerDay > 0 && lastDay >= cfg.MaxPerDay {
		return deny(ReasonDailyLimit, "daily call limit reached")
	}

	callee, calleeKind := strings.TrimSpace(req.Callee), strings.TrimSpace(req.Kind)
	if entry != nil {
		if callee == "" {
			callee = entry.Name
		}
		if calleeKind == "" {
			calleeKind = entry.Kind
		}
	}
	if err := checkCallee(callee, cfg.MaxCallee); err != nil {
		return deny(ReasonCallee, "callee "+err.Error())
	}
	if !prompts.ValidKind(calleeKind) {
		return deny(ReasonKind, "kind must be business or personal")
	}
	if utf8.RuneCountInString(req.Opening) > cfg.MaxOpening {
		return deny(ReasonOpening, "opening must be at most "+itoa(cfg.MaxOpening)+" characters")
	}
	obj := strings.TrimSpace(req.Objective)
	if obj == "" || utf8.RuneCountInString(obj) > cfg.MaxObjective {
		return deny(ReasonObjective, "objective must be non-empty and at most "+itoa(cfg.MaxObjective)+" characters")
	}
	if utf8.RuneCountInString(req.Context) > cfg.MaxContext {
		return deny(ReasonContext, "context must be at most "+itoa(cfg.MaxContext)+" characters")
	}
	if utf8.RuneCountInString(req.Limits) > cfg.MaxLimits {
		return deny(ReasonLimits, "limits must be at most "+itoa(cfg.MaxLimits)+" characters")
	}

	maxSec := cfg.MaxCallSeconds
	if req.MaxMinutes < 0 || req.MaxMinutes*60 > cfg.MaxCallSeconds {
		return deny(ReasonDuration, "max_minutes must be between 1 and "+itoa(cfg.MaxCallSeconds/60))
	}
	if req.MaxMinutes > 0 {
		maxSec = req.MaxMinutes * 60
	}

	return Decision{Allowed: true, Reason: ReasonOK, Number: number, Label: label, Kind: kind,
		Callee: callee, CalleeKind: calleeKind, MaxSeconds: maxSec}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
