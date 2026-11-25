package callstream

import (
	"github.com/CallHarborMCP/callharbor/internal/service"
)

// ErrInstructionsTooLarge reports that the rendered session instructions exceed
// the configured byte limit. It is the service error, so MakeCall and the media
// path refuse the same text with the same error.
var ErrInstructionsTooLarge = service.ErrInstructionsTooLarge

// Render builds the session instructions for a media session. The framing is
// defined once, in service.RenderInstructions, so queue-time and media-time
// checks cannot disagree.
func Render(profileInstructions, canonicalBriefJSON string, maxBytes int) (string, error) {
	return service.RenderInstructions(profileInstructions, canonicalBriefJSON, maxBytes)
}
