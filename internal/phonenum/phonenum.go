// Package phonenum normalizes and compares the phone numbers that name accounts.
package phonenum

import (
	"errors"
	"strings"
)

// ErrInvalid reports input that is not a phone number in international format.
var ErrInvalid = errors.New("phone number must be in international format, e.g. +79001234567")

// Normalize turns user input into the canonical "+<digits>" form, so one number
// always maps to one account name. It accepts an optional leading "+" and
// spaces, dashes, dots and parentheses as separators.
func Normalize(input string) (string, error) {
	var digits strings.Builder
	for _, r := range strings.TrimPrefix(strings.TrimSpace(input), "+") {
		switch {
		case r >= '0' && r <= '9':
			digits.WriteRune(r)
		case r == ' ' || r == '-' || r == '.' || r == '(' || r == ')':
		default:
			return "", ErrInvalid
		}
	}
	// E.164 allows at most 15 digits; no real number is shorter than 7.
	if n := digits.Len(); n < 7 || n > 15 {
		return "", ErrInvalid
	}
	return "+" + digits.String(), nil
}

// Same reports whether a and b are the same number, ignoring "+", spaces and
// other formatting. Strings without digits never match.
func Same(a, b string) bool {
	a, b = digitsOf(a), digitsOf(b)
	return a != "" && a == b
}

func digitsOf(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, s)
}
