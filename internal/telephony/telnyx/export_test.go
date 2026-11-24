package telnyx

func NewMediaSessionForTest(cfg StreamConfig, callControlID string) (*MediaSession, error) {
	return newMediaSession(callControlID, cfg)
}
