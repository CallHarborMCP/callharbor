package service

import (
	"unicode"
	"unicode/utf8"
)

// MaxIdempotencyKeyLength is the longest accepted idempotency key, in runes.
const MaxIdempotencyKeyLength = 128

// ValidateIdempotencyKey accepts a key of 1 to MaxIdempotencyKeyLength runes.
// Every rune must be printable and must not be whitespace, so control
// characters and blanks can never reach storage or logs unseen.
func ValidateIdempotencyKey(key string) error {
	if !utf8.ValidString(key) {
		return invalidInput("idempotency key must be valid UTF-8")
	}
	n := utf8.RuneCountInString(key)
	if n < 1 || n > MaxIdempotencyKeyLength {
		return invalidInput("idempotency key must be 1 to %d characters", MaxIdempotencyKeyLength)
	}
	for _, r := range key {
		if !unicode.IsPrint(r) || unicode.IsSpace(r) {
			return invalidInput("idempotency key contains a non-printable or whitespace character")
		}
	}
	return nil
}
