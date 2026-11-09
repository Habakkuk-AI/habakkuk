package memory

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// Memory notes and the user profile are plain files that end up in prompts,
// backups and other sessions, so obvious credentials are refused before
// they are written. This is a guard against accidents, not a scanner: it
// looks for well-known key formats, private keys, "password: ..." style
// assignments and long random-looking tokens.

var secretPatterns = []struct {
	what string
	re   *regexp.Regexp
}{
	{"a private key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"an API key", regexp.MustCompile(`\b(sk|pk|rk)-(ant-|proj-|live-|test-)?[A-Za-z0-9_-]{20,}`)},
	{"an AWS access key", regexp.MustCompile(`\b(AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{"a GitHub token", regexp.MustCompile(`\b(ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{30,}|\bgithub_pat_[A-Za-z0-9_]{30,}`)},
	{"a Slack token", regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}`)},
	{"a Google API key", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`)},
	{"a Stripe key", regexp.MustCompile(`\b(sk|rk)_(live|test)_[A-Za-z0-9]{16,}`)},
	{"a JSON web token", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)},
	{"a credential in a URL", regexp.MustCompile(`[a-z][a-z0-9+.-]*://[^\s/:@]+:[^\s/@]{3,}@`)},
	{"a password or secret", regexp.MustCompile(`(?i)\b(password|passwd|passcode|pwd|pin|api[_ -]?key|access[_ -]?token|auth[_ -]?token|private[_ -]?key|client[_ -]?secret|secret[_ -]?key)\b\s*(is\s*)?[:=]\s*\S{4,}`)},
	{"a bearer token", regexp.MustCompile(`(?i)\bauthorization:\s*(bearer|basic)\s+\S{8,}`)},
}

var reToken = regexp.MustCompile(`[A-Za-z0-9+/_=-]{32,}`)

// CheckSecrets returns an error naming the kind of secret text seems to
// contain, or nil.
func CheckSecrets(text string) error {
	for _, p := range secretPatterns {
		if p.re.MatchString(text) {
			return fmt.Errorf("refused: this looks like it contains %s; never store secrets in memory (keep them in $HABAKKUK_HOME/.env or a password manager)", p.what)
		}
	}
	for _, tok := range reToken.FindAllString(text, -1) {
		if randomLooking(tok) {
			return fmt.Errorf("refused: %q looks like a key or token; never store secrets in memory", tok[:6]+"…")
		}
	}
	if cardNumber(text) {
		return fmt.Errorf("refused: this looks like it contains a payment card number; never store secrets in memory")
	}
	return nil
}

// randomLooking reports whether a long run without spaces mixes upper case,
// lower case and digits the way generated keys do. Hex digests, which are
// all one case, and ordinary words are let through.
func randomLooking(tok string) bool {
	if strings.Contains(tok, "/") && strings.Count(tok, "/") > 2 {
		return false // a path
	}
	var upper, lower, digit int
	for _, r := range tok {
		switch {
		case unicode.IsUpper(r):
			upper++
		case unicode.IsLower(r):
			lower++
		case unicode.IsDigit(r):
			digit++
		}
	}
	return upper >= 3 && lower >= 3 && digit >= 3
}

var reCard = regexp.MustCompile(`\b(?:\d[ -]?){13,19}\b`)

// cardNumber reports whether text holds a digit run of card length that
// passes the Luhn check.
func cardNumber(text string) bool {
	for _, m := range reCard.FindAllString(text, -1) {
		digits := strings.Map(func(r rune) rune {
			if r >= '0' && r <= '9' {
				return r
			}
			return -1
		}, m)
		if len(digits) < 13 || len(digits) > 19 {
			continue
		}
		sum, double := 0, false
		for i := len(digits) - 1; i >= 0; i-- {
			d := int(digits[i] - '0')
			if double {
				if d *= 2; d > 9 {
					d -= 9
				}
			}
			sum += d
			double = !double
		}
		if sum%10 == 0 {
			return true
		}
	}
	return false
}
