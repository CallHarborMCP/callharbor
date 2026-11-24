package telnyxfake

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
	"github.com/CallHarborMCP/callharbor/internal/voicebridge"
)

func TestSignedStreamDeliversDTMFAndErrorFrames(t *testing.T) {
	priv, pub := newKeys(t)
	now := time.Now()
	v, err := telnyx.NewVerifier(pub, telnyx.DefaultTolerance, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	dtmf := make(chan string, 4)
	errs := make(chan int64, 4)
	var me *telnyx.MediaEndpoint
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		me.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	streamURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/media?token=frames"
	me, err = telnyx.NewMediaEndpoint(v, streamURL, func(start telnyx.Frame, send telnyx.Send) (telnyx.MediaSink, error) {
		b, err := voicebridge.New(start.Format, pcmuLeg, 5*time.Millisecond, 2*time.Second)
		if err != nil {
			return nil, err
		}
		sink, err := telnyx.NewBridgeSink(b, send)
		if err != nil {
			return nil, err
		}
		sink.OnDTMF = func(d string) { dtmf <- d }
		sink.OnError = func(f telnyx.Frame) { errs <- f.ErrCode }
		return sink, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := DialMedia(ctx, streamURL, priv, now)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	start := []byte(`{"event":"start","stream_id":"frames-1","sequence_number":"1","start":{"call_control_id":"v3:frames","client_state":"","media_format":{"encoding":"PCMU","sample_rate":8000,"channels":1}}}`)
	for _, f := range [][]byte{
		start,
		[]byte(`{"event":"dtmf","stream_id":"frames-1","dtmf":{"digit":"#"}}`),
		[]byte(`{"event":"error","stream_id":"frames-1","payload":{"code":100003,"title":"malformed_frame"}}`),
	} {
		if err := s.Send(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case d := <-dtmf:
		if d != "#" {
			t.Fatalf("dtmf %q, want #", d)
		}
	case <-ctx.Done():
		t.Fatal("dtmf frame never reached the call layer")
	}
	select {
	case c := <-errs:
		if c != 100003 {
			t.Fatalf("error code %d, want 100003", c)
		}
	case <-ctx.Done():
		t.Fatal("error frame never reached the call layer")
	}
}
