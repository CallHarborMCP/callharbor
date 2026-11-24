package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// tokenBytes is the entropy of every opaque credential (codes, access and
// refresh tokens, grant IDs). 32 bytes is 256 bits from crypto/rand.
const tokenBytes = 32

// newOpaque returns a URL-safe random credential. Only its hash is stored.
func newOpaque(prefix string) (string, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("oauth: random token: %w", err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// hashToken is the at-rest form of an opaque credential. Credentials are
// high-entropy random values, so a plain SHA-256 is sufficient and lets the
// database look them up by hash.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
