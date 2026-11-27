package callstream

import "github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"

// unconfirmedPlayout reports whether any agent item ended with playout the leg
// never confirmed. It reads through the sink lock, so it is race-free.
func unconfirmedPlayout(s *telnyx.BridgeSink) bool {
	return s.AnyUnconfirmed()
}
