package policy

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mfbonfigli/dialout/prompts"
)

// Mode is the dialling mode: who, among the dialable numbers, may be called.
type Mode string

const (
	// ModeAllowlist calls only the numbers on the allowlist.
	ModeAllowlist Mode = "allowlist"
	// ModeOpen calls any dialable number. Numbers not on the allowlist get the calling hours and
	// the per-number limit on top.
	ModeOpen Mode = "open"
)

// Config is the policy in force.
type Config struct {
	Enabled   bool
	Mode      Mode
	Dialable  Dialable
	Allowlist []Entry  // already validated with ValidateAllowlist
	Blocklist []string // numbers or prefixes never called, already normalised with NormalizeBlock

	// Open mode, for numbers not on the allowlist.
	Hours              Hours          // when they may be called; the zero value: any time
	Location           *time.Location // the time zone of Hours; time.Local when nil
	MaxPerNumberPerDay int            // calls to the same number in 24 h; 0: no limit

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
	To         string // a number (international form, or national in the default country) or an allowlist label
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
	Active bool     // a call is in progress (one concurrent call)
	Placed []Placed // calls placed (successful or not) in the last 24 h at least
}

// Placed is one call placed: the number (E.164) and when.
type Placed struct {
	Number string
	At     time.Time
}

// Reason codes. They are returned to the caller and logged; keep them stable.
const (
	ReasonOK             = "ok"
	ReasonDisabled       = "disabled"
	ReasonMaintenance    = "maintenance"
	ReasonBadMode        = "unsupported_mode"
	ReasonBadNumber      = "invalid_number"
	ReasonCountry        = "country_not_allowed"
	ReasonNumberType     = "number_type_not_allowed"
	ReasonBlocked        = "blocked"
	ReasonNotAllowlisted = "not_allowlisted"
	ReasonBusy           = "call_in_progress"
	ReasonHourlyLimit    = "hourly_limit"
	ReasonDailyLimit     = "daily_limit"
	ReasonNumberLimit    = "number_daily_limit"
	ReasonOutsideHours   = "outside_hours"
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
	Country    string // ISO region code, set with Number
	Label      string // allowlist label, when listed
	Kind       Kind   // the number's type
	Callee     string // who answers: the request's, else the allowlist entry's name
	CalleeKind string // business, personal or empty: the request's, else the entry's
	MaxSeconds int    // call length to enforce
}

func deny(reason, detail string) Decision { return Decision{Reason: reason, Detail: detail} }

// numberReason maps a Classify error to its reason code.
func numberReason(err error) string {
	switch {
	case errors.Is(err, ErrCountry):
		return ReasonCountry
	case errors.Is(err, ErrKind):
		return ReasonNumberType
	}
	return ReasonBadNumber
}

// Decide applies the checks in order. It never has side effects: the caller records the call in
// State only when it actually goes on to place it.
func Decide(cfg Config, req Request, st State, now time.Time) Decision {
	if !cfg.Enabled {
		return deny(ReasonDisabled, "phone calls are disabled (policy.enabled)")
	}
	if cfg.MaintenanceHold {
		return deny(ReasonMaintenance, "a maintenance window is about to start; try again after it")
	}
	if cfg.Mode != ModeAllowlist && cfg.Mode != ModeOpen {
		return deny(ReasonBadMode, "unsupported dialling mode "+string(cfg.Mode))
	}

	// Resolve the destination: an allowlist label (case-insensitive) or a number. Listed numbers
	// are classified again (defence in depth: the list is validated on load, but this keeps the
	// rule in one place).
	to := strings.TrimSpace(req.To)
	var entry *Entry
	for i, e := range cfg.Allowlist {
		if strings.EqualFold(e.Label, to) {
			entry, to = &cfg.Allowlist[i], e.Number
			break
		}
	}
	num, err := Classify(to, cfg.Dialable)
	if err != nil {
		return deny(numberReason(err), err.Error())
	}
	if entry == nil {
		for i, e := range cfg.Allowlist {
			if e.Number == num.E164 {
				entry = &cfg.Allowlist[i]
				break
			}
		}
	}
	d := Decision{Number: num.E164, Country: num.Country, Kind: num.Kind}
	refuse := func(reason, detail string) Decision {
		d.Reason, d.Detail = reason, detail
		return d
	}
	for _, b := range cfg.Blocklist {
		if strings.HasPrefix(num.E164, b) {
			return refuse(ReasonBlocked, num.E164+" is on the blocklist")
		}
	}
	if entry != nil {
		d.Label = entry.Label
	} else if cfg.Mode == ModeAllowlist {
		return refuse(ReasonNotAllowlisted, num.E164+" is not on the allowlist")
	}

	if st.Active {
		return refuse(ReasonBusy, "another call is in progress")
	}
	var lastHour, lastDay, toNumber int
	for _, p := range st.Placed {
		age := now.Sub(p.At)
		if age < 0 {
			continue
		}
		if age < time.Hour {
			lastHour++
		}
		if age < 24*time.Hour {
			lastDay++
			if p.Number == num.E164 {
				toNumber++
			}
		}
	}
	if cfg.MaxPerHour > 0 && lastHour >= cfg.MaxPerHour {
		return refuse(ReasonHourlyLimit, "hourly call limit reached")
	}
	if cfg.MaxPerDay > 0 && lastDay >= cfg.MaxPerDay {
		return refuse(ReasonDailyLimit, "daily call limit reached")
	}
	if entry == nil { // open mode, a number not on the allowlist
		if cfg.MaxPerNumberPerDay > 0 && toNumber >= cfg.MaxPerNumberPerDay {
			return refuse(ReasonNumberLimit, "this number was already called "+strconv.Itoa(toNumber)+" times in the last 24 hours")
		}
		loc := cfg.Location
		if loc == nil {
			loc = time.Local
		}
		if !cfg.Hours.Contains(now.In(loc)) {
			return refuse(ReasonOutsideHours, "numbers not on the allowlist may be called only "+cfg.Hours.String()+" ("+loc.String()+")")
		}
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
		return refuse(ReasonCallee, "callee "+err.Error())
	}
	if !prompts.ValidKind(calleeKind) {
		return refuse(ReasonKind, "kind must be business or personal")
	}
	if utf8.RuneCountInString(req.Opening) > cfg.MaxOpening {
		return refuse(ReasonOpening, "opening must be at most "+strconv.Itoa(cfg.MaxOpening)+" characters")
	}
	obj := strings.TrimSpace(req.Objective)
	if obj == "" || utf8.RuneCountInString(obj) > cfg.MaxObjective {
		return refuse(ReasonObjective, "objective must be non-empty and at most "+strconv.Itoa(cfg.MaxObjective)+" characters")
	}
	if utf8.RuneCountInString(req.Context) > cfg.MaxContext {
		return refuse(ReasonContext, "context must be at most "+strconv.Itoa(cfg.MaxContext)+" characters")
	}
	if utf8.RuneCountInString(req.Limits) > cfg.MaxLimits {
		return refuse(ReasonLimits, "limits must be at most "+strconv.Itoa(cfg.MaxLimits)+" characters")
	}

	maxSec := cfg.MaxCallSeconds
	if req.MaxMinutes < 0 || req.MaxMinutes*60 > cfg.MaxCallSeconds {
		return refuse(ReasonDuration, "max_minutes must be between 1 and "+strconv.Itoa(cfg.MaxCallSeconds/60))
	}
	if req.MaxMinutes > 0 {
		maxSec = req.MaxMinutes * 60
	}

	d.Allowed, d.Reason, d.Callee, d.CalleeKind, d.MaxSeconds = true, ReasonOK, callee, calleeKind, maxSec
	return d
}

// Hours is a daily window, e.g. 09:00-21:00. A window whose end comes before its start crosses
// midnight. The zero value is any time.
type Hours struct {
	From, To int // minutes after midnight
	set      bool
}

// ParseHours reads "HH:MM-HH:MM". An empty string is any time.
func ParseHours(s string) (Hours, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Hours{}, nil
	}
	from, to, ok := strings.Cut(s, "-")
	f, err1 := clock(from)
	t, err2 := clock(to)
	if !ok || err1 != nil || err2 != nil || f == t {
		return Hours{}, fmt.Errorf("hours %q: use HH:MM-HH:MM, e.g. 09:00-21:00", s)
	}
	return Hours{From: f, To: t, set: true}, nil
}

func clock(s string) (int, error) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return 0, err
	}
	return t.Hour()*60 + t.Minute(), nil
}

// Contains reports whether t's time of day falls in the window.
func (h Hours) Contains(t time.Time) bool {
	if !h.set {
		return true
	}
	m := t.Hour()*60 + t.Minute()
	if h.From < h.To {
		return m >= h.From && m < h.To
	}
	return m >= h.From || m < h.To
}

func (h Hours) String() string {
	if !h.set {
		return "any time"
	}
	return fmt.Sprintf("%02d:%02d-%02d:%02d", h.From/60, h.From%60, h.To/60, h.To%60)
}
