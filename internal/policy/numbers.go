// Package policy decides whether a call may be placed. It is the safety core of dialout:
// everything here is pure (no I/O) so it can be tested exhaustively.
package policy

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/nyaruka/phonenumbers"

	"github.com/mfbonfigli/dialout/prompts"
)

// Kind is a number's type, from the country's numbering plan (libphonenumber's metadata).
type Kind string

const (
	KindFixed         Kind = "fixed"
	KindMobile        Kind = "mobile"
	KindFixedOrMobile Kind = "fixed_or_mobile" // where the plan can't tell them apart, e.g. the US
	KindTollFree      Kind = "toll_free"
)

// AllowableKinds are the only types a configuration may allow. Premium-rate, shared-cost, VoIP,
// personal, pager, UAN and voicemail numbers, and numbers of unknown type, are never called.
var AllowableKinds = []Kind{KindFixed, KindMobile, KindTollFree}

// Dialable is which numbers exist for dialout at all, in every mode: their countries and types.
type Dialable struct {
	Countries      []string // ISO 3166-1 region codes, e.g. IT, IE
	DefaultCountry string   // the country of numbers written without an international prefix
	Kinds          []Kind   // a subset of AllowableKinds
}

// allows reports whether a number of type k may be called. A fixed_or_mobile number may be when
// either type is allowed.
func (d Dialable) allows(k Kind) bool {
	if k == KindFixedOrMobile {
		return slices.Contains(d.Kinds, KindFixed) || slices.Contains(d.Kinds, KindMobile)
	}
	return slices.Contains(d.Kinds, k)
}

// Number is a number Classify accepted.
type Number struct {
	E164    string // e.g. +393331234567
	Country string // ISO region code
	Kind    Kind
}

// Errors returned by Classify. They are stable so callers can map them to reason codes.
var (
	ErrEmpty      = errors.New("empty number")
	ErrCharacters = errors.New("number contains characters other than digits, spaces, '.', '-', '(', ')' and a leading '+'")
	ErrInvalid    = errors.New("not a valid phone number")
	ErrEmergency  = errors.New("emergency numbers are never called")
	ErrCountry    = errors.New("country not allowed")
	ErrKind       = errors.New("number type not allowed")
)

// Classify turns user input such as "+39 333 123 4567", "0039 02 1234567" or "02-1234567" (in
// the default country) into E.164, and checks that it is a valid number of an allowed country and
// type. Refusing by default is deliberate: a number libphonenumber can't classify is not a
// number an agent should be able to dial.
func Classify(input string, d Dialable) (Number, error) {
	s := strings.TrimSpace(input)
	if s == "" {
		return Number{}, ErrEmpty
	}
	// Digits and separators only: no letters (vanity numbers), no extensions, no codes.
	for i, r := range s {
		switch {
		case r >= '0' && r <= '9', r == '+' && i == 0, r == ' ' || r == '.' || r == '-' || r == '(' || r == ')':
		default:
			return Number{}, ErrCharacters
		}
	}
	region := d.DefaultCountry
	if region == "" {
		region = "ZZ" // unknown: only numbers with an international prefix parse
	}
	if !strings.HasPrefix(s, "+") && phonenumbers.ConnectsToEmergencyNumber(s, region) {
		return Number{}, ErrEmergency
	}
	num, err := phonenumbers.Parse(s, region)
	if err != nil || !phonenumbers.IsValidNumber(num) {
		return Number{}, ErrInvalid
	}
	country := phonenumbers.GetRegionCodeForNumber(num)
	if !slices.Contains(d.Countries, country) {
		return Number{}, fmt.Errorf("%w: %s", ErrCountry, country)
	}
	var kind Kind
	switch t := phonenumbers.GetNumberType(num); t {
	case phonenumbers.FIXED_LINE:
		kind = KindFixed
	case phonenumbers.MOBILE:
		kind = KindMobile
	case phonenumbers.FIXED_LINE_OR_MOBILE:
		kind = KindFixedOrMobile
	case phonenumbers.TOLL_FREE:
		kind = KindTollFree
	default:
		return Number{}, fmt.Errorf("%w: %s", ErrKind, typeName(t))
	}
	if !d.allows(kind) {
		return Number{}, fmt.Errorf("%w: %s", ErrKind, kind)
	}
	return Number{E164: phonenumbers.Format(num, phonenumbers.E164), Country: country, Kind: kind}, nil
}

func typeName(t phonenumbers.PhoneNumberType) string {
	switch t {
	case phonenumbers.PREMIUM_RATE:
		return "premium_rate"
	case phonenumbers.SHARED_COST:
		return "shared_cost"
	case phonenumbers.VOIP:
		return "voip"
	case phonenumbers.PERSONAL_NUMBER:
		return "personal"
	case phonenumbers.PAGER:
		return "pager"
	case phonenumbers.UAN:
		return "uan"
	case phonenumbers.VOICEMAIL:
		return "voicemail"
	}
	return "unknown"
}

// ValidCountry reports whether c is a region code libphonenumber knows.
func ValidCountry(c string) bool { return phonenumbers.GetSupportedRegions()[c] }

// NormalizeBlock turns a blocklist entry, a number or a prefix in international form ("+39 02",
// "+44 20 7946 0000"), into its digits after the "+".
func NormalizeBlock(s string) (string, error) {
	var b strings.Builder
	for i, r := range strings.TrimSpace(s) {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '+' && i == 0:
			b.WriteRune(r)
		case r == ' ' || r == '.' || r == '-' || r == '(' || r == ')':
		default:
			return "", fmt.Errorf("blocklist entry %q: digits and separators only", s)
		}
	}
	out := b.String()
	if !strings.HasPrefix(out, "+") || len(out) < 2 || len(out) > 16 {
		return "", fmt.Errorf("blocklist entry %q: write it in international form, starting with +", s)
	}
	return out, nil
}

// Entry is one allowlisted number with a human label ("Alex (mobile)").
type Entry struct {
	Number string `json:"number" yaml:"number"`
	Label  string `json:"label" yaml:"label"`
	Name   string `json:"name,omitempty" yaml:"name"` // the default callee: how the agent refers to who answers
	Kind   string `json:"kind,omitempty" yaml:"kind"` // the default kind of callee: business or personal
}

// maxName bounds an entry's name, as the callee size limit's default does.
const maxName = 100

// ValidateAllowlist normalises every entry and rejects numbers that aren't dialable, empty or
// duplicate labels, duplicate numbers, and names or kinds that aren't valid callees.
func ValidateAllowlist(entries []Entry, d Dialable) ([]Entry, error) {
	out := make([]Entry, 0, len(entries))
	seenNum := map[string]bool{}
	seenLabel := map[string]bool{}
	for i, e := range entries {
		num, err := Classify(e.Number, d)
		if err != nil {
			return nil, fmt.Errorf("allowlist entry %d (%q): %w", i+1, e.Label, err)
		}
		n := num.E164
		label := strings.TrimSpace(e.Label)
		if label == "" {
			return nil, fmt.Errorf("allowlist entry %d (%s): empty label", i+1, n)
		}
		key := strings.ToLower(label)
		if seenNum[n] {
			return nil, fmt.Errorf("allowlist entry %d: duplicate number %s", i+1, n)
		}
		if seenLabel[key] {
			return nil, fmt.Errorf("allowlist entry %d: duplicate label %q", i+1, label)
		}
		name := strings.TrimSpace(e.Name)
		if err := checkCallee(name, maxName); err != nil {
			return nil, fmt.Errorf("allowlist entry %d (%q): name: %w", i+1, label, err)
		}
		if !prompts.ValidKind(e.Kind) {
			return nil, fmt.Errorf("allowlist entry %d (%q): kind %q: use business or personal", i+1, label, e.Kind)
		}
		seenNum[n], seenLabel[key] = true, true
		out = append(out, Entry{Number: n, Label: label, Name: name, Kind: e.Kind})
	}
	return out, nil
}

// checkCallee checks a callee: one line, at most max characters (no limit when max is 0).
func checkCallee(c string, max int) error {
	switch {
	case strings.ContainsAny(c, "\r\n"):
		return errors.New("must be one line")
	case max > 0 && utf8.RuneCountInString(c) > max:
		return fmt.Errorf("must be at most %d characters", max)
	}
	return nil
}
