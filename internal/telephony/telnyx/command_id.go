package telnyx

import (
	"crypto/sha256"
	"encoding/hex"
)

// CommandID derives the Telnyx command id for one call's dial. The same call
// always produces the same id, so a retried dial is idempotent at Telnyx. It is
// a digest of the call id, so the call id itself is not sent twice.
func CommandID(callID string) string {
	sum := sha256.Sum256([]byte("callharbor-dial\x00" + callID))
	return hex.EncodeToString(sum[:16])
}
