// Package policy decides whether a call may be placed. It is the safety core of dialout:
// everything here is pure (no I/O) so it can be tested exhaustively.
package policy

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/mfbonfigli/dialout/prompts"
)

// Kind is the type of an Italian number. Only fixed-line and mobile numbers can ever be called.
type Kind string

const (
	KindFixed  Kind = "fixed"
	KindMobile Kind = "mobile"
)

// Errors returned by NormalizeItalian. They are stable so callers can map them to reason codes.
var (
	ErrEmpty      = errors.New("empty number")
	ErrCharacters = errors.New("number contains characters other than digits, spaces, '.', '-', '(', ')' and a leading '+'")
	ErrNotItalian = errors.New("not an Italian number (+39)")
	ErrNotAllowed = errors.New("not an Italian fixed-line or mobile number (premium, toll-free, short, emergency and service numbers are never called)")
	ErrLength     = errors.New("wrong length for an Italian fixed-line or mobile number")
)

// NormalizeItalian turns user input such as "+39 333 123 4567", "0039 02 1234567", "02-1234567" or
// "3331234567" into E.164 ("+393331234567") and classifies it. Anything that is not an Italian
// geographic (fixed-line) or mobile number is rejected.
//
// The rule follows the Italian numbering plan by prefix of the national number:
//   - "0"  geographic numbers, 6 to 11 digits including the leading 0 (kept after +39);
//   - "3"  mobile numbers, 9 or 10 digits.
//
// Every other prefix is refused, which excludes emergency and public-utility numbers (1xx),
// operator services (4xx), Internet access (7xx), toll-free (80x), shared cost (84x), premium
// rate (89x, 16x), VoIP (55) and anything else. Refusing by default is deliberate: a number type
// we don't recognise is not a number an agent should be able to dial.
func NormalizeItalian(input string) (string, Kind, error) {
	s := strings.TrimSpace(input)
	if s == "" {
		return "", "", ErrEmpty
	}
	var b strings.Builder
	for i, r := range s {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '+' && i == 0:
			b.WriteRune(r)
		case r == ' ' || r == '.' || r == '-' || r == '(' || r == ')':
			// separators
		default:
			return "", "", ErrCharacters
		}
	}
	d := b.String()

	var national string
	switch {
	case strings.HasPrefix(d, "+"):
		if !strings.HasPrefix(d, "+39") {
			return "", "", ErrNotItalian
		}
		national = d[3:]
	case strings.HasPrefix(d, "0039"):
		national = d[4:]
	case strings.HasPrefix(d, "00"):
		return "", "", ErrNotItalian // international prefix to another country
	default:
		national = d // dialled as a national number
	}
	if national == "" {
		return "", "", ErrLength
	}

	var kind Kind
	switch national[0] {
	case '0':
		if len(national) < 6 || len(national) > 11 {
			return "", "", ErrLength
		}
		kind = KindFixed
	case '3':
		if len(national) < 9 || len(national) > 10 {
			return "", "", ErrLength
		}
		kind = KindMobile
	default:
		return "", "", ErrNotAllowed
	}
	return "+39" + national, kind, nil
}

// Entry is one allowlisted number with a human label ("Marco (mobile)").
type Entry struct {
	Number string `json:"number" yaml:"number"`
	Label  string `json:"label" yaml:"label"`
	Name   string `json:"name,omitempty" yaml:"name"` // the default callee: how the agent refers to who answers
	Kind   string `json:"kind,omitempty" yaml:"kind"` // the default kind of callee: business or personal
}

// maxName bounds an entry's name, as the callee size limit's default does.
const maxName = 100

// ValidateAllowlist normalises every entry and rejects invalid numbers, empty or duplicate labels,
// duplicate numbers, and names or kinds that aren't valid callees.
func ValidateAllowlist(entries []Entry) ([]Entry, error) {
	out := make([]Entry, 0, len(entries))
	seenNum := map[string]bool{}
	seenLabel := map[string]bool{}
	for i, e := range entries {
		n, _, err := NormalizeItalian(e.Number)
		if err != nil {
			return nil, fmt.Errorf("allowlist entry %d (%q): %w", i+1, e.Label, err)
		}
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
