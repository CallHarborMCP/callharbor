package callstream

import (
	"testing"

	"github.com/CallHarborMCP/callharbor/internal/audio"
	"github.com/CallHarborMCP/callharbor/internal/realtime"
	"github.com/CallHarborMCP/callharbor/internal/storage"
)

// A refused attach must free the call's session claim, so the call is not
// left busy. Attach takes ownership of release, and each error path releases it
// and closes the provider session. The leg format is invalid, so bridge
// construction fails before any run starts.
func TestAttachErrorReleasesSessionClaim(t *testing.T) {
	m := &Media{}
	sess := newFakeSession()

	release, err := m.sessions.claim("call_1")
	if err != nil {
		t.Fatal(err)
	}
	badLeg := audio.Format{}
	_, err = m.attach(sess, storage.MediaCall{ID: "call_1"}, badLeg, audio.Format{}, realtime.Capabilities{}, nil, release)
	if err == nil {
		t.Fatal("attach accepted an invalid leg format")
	}
	select {
	case <-sess.closed:
	default:
		t.Fatal("refused attach did not close the provider session")
	}
	if _, err := m.sessions.claim("call_1"); err != nil {
		t.Fatalf("claim after refused attach = %v, want the call's slot free", err)
	}
}
