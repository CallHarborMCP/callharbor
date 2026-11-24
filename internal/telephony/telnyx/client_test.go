package telnyx

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const testAPIKey = "KEY_test_0123456789abcdef"

type fakeTelnyx struct {
	mu       sync.Mutex
	calls    []recorded
	status   int
	response string
}

type recorded struct {
	method, path, auth string
	body               []byte
}

func (f *fakeTelnyx) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.calls = append(f.calls, recorded{r.Method, r.URL.EscapedPath(), r.Header.Get("Authorization"), body})
	status, resp := f.status, f.response
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, resp)
}

func (f *fakeTelnyx) recorded() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.calls...)
}

func newFake(t *testing.T, status int, response string) (*fakeTelnyx, *Client) {
	t.Helper()
	f := &fakeTelnyx{status: status, response: response}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL+"/v2", testAPIKey, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	return f, c
}

const acceptedDial = `{"data":{"call_control_id":"v3:abc","call_leg_id":"leg-1","call_session_id":"sess-1","is_alive":false,"record_type":"call"}}`

func TestDialSendsDocumentedRequestAndReturnsIDs(t *testing.T) {
	f, c := newFake(t, http.StatusCreated, acceptedDial)
	res, err := c.Dial(context.Background(), validDial())
	if err != nil {
		t.Fatal(err)
	}
	if res.CallControlID != "v3:abc" || res.CallLegID != "leg-1" || res.CallSessionID != "sess-1" {
		t.Fatalf("ids %+v", res)
	}
	calls := f.recorded()
	if len(calls) != 1 {
		t.Fatalf("dial attempts %d, want exactly 1 (no automatic retry)", len(calls))
	}
	if calls[0].method != http.MethodPost || calls[0].path != "/v2/calls" {
		t.Fatalf("request %s %s", calls[0].method, calls[0].path)
	}
	if calls[0].auth != "Bearer "+testAPIKey {
		t.Fatalf("auth header %q", calls[0].auth)
	}
	if err := compileSchema(t, "/components/schemas/CallRequest").Validate(instance(t, calls[0].body)); err != nil {
		t.Fatalf("sent body violates official CallRequest: %v\n%s", err, calls[0].body)
	}
	var sent map[string]any
	_ = json.Unmarshal(calls[0].body, &sent)
	if sent["command_id"] != "call-7f3a:dial" {
		t.Fatalf("command_id %v", sent["command_id"])
	}
	if sent["stream_bidirectional_mode"] != "rtp" || sent["stream_establish_before_call_originate"] != true {
		t.Fatalf("media stream not configured before answer: %s", calls[0].body)
	}
}

func TestDialRetryReusesCommandIDForTelnyxIdempotency(t *testing.T) {
	f, c := newFake(t, http.StatusCreated, acceptedDial)
	p := validDial()
	for range 2 {
		if _, err := c.Dial(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
	calls := f.recorded()
	var a, b map[string]any
	_ = json.Unmarshal(calls[0].body, &a)
	_ = json.Unmarshal(calls[1].body, &b)
	if a["command_id"] != b["command_id"] || a["command_id"] == "" {
		t.Fatalf("command_id changed across dials: %v vs %v", a["command_id"], b["command_id"])
	}
}

func TestDialRejectsInvalidParamsWithoutNetworkCall(t *testing.T) {
	f, c := newFake(t, http.StatusCreated, acceptedDial)
	p := validDial()
	p.To = "not-e164"
	if _, err := c.Dial(context.Background(), p); !errors.Is(err, ErrInvalidDial) {
		t.Fatalf("err %v", err)
	}
	if n := len(f.recorded()); n != 0 {
		t.Fatalf("invalid dial reached the network %d times", n)
	}
}

func TestDialAPIErrorDoesNotLeakKey(t *testing.T) {
	_, c := newFake(t, http.StatusUnauthorized, `{"errors":[{"detail":"bad key"}]}`)
	_, err := c.Dial(context.Background(), validDial())
	if !errors.Is(err, ErrAPI) {
		t.Fatalf("err %v, want ErrAPI", err)
	}
	if strings.Contains(err.Error(), testAPIKey) {
		t.Fatal("error leaked API key")
	}
}

func TestDialMalformedOrIncompleteResponse(t *testing.T) {
	cases := map[string]string{
		"not json":        `{"data":`,
		"no data":         `{"meta":{}}`,
		"missing leg id":  `{"data":{"call_control_id":"v3:abc"}}`,
		"missing ctrl id": `{"data":{"call_leg_id":"leg-1"}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, c := newFake(t, http.StatusCreated, body)
			if _, err := c.Dial(context.Background(), validDial()); !errors.Is(err, ErrBadResponse) {
				t.Fatalf("err %v, want ErrBadResponse", err)
			}
		})
	}
}

func TestDialHonorsContextCancellation(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	t.Cleanup(func() { close(block); srv.Close() })
	c, err := NewClient(srv.URL+"/v2", testAPIKey, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.Dial(ctx, validDial()); err == nil {
		t.Fatal("dial succeeded despite cancelled context")
	}
}

func TestHangupMatchesOfficialActionAndSchema(t *testing.T) {
	f, c := newFake(t, http.StatusOK, `{"data":{"result":"ok"}}`)
	if err := c.Hangup(context.Background(), "v3:abc", "call-7f3a:hangup"); err != nil {
		t.Fatal(err)
	}
	calls := f.recorded()
	if calls[0].path != "/v2/calls/v3:abc/actions/hangup" {
		t.Fatalf("path %s", calls[0].path)
	}
	if err := compileSchema(t, "/components/schemas/HangupRequest").Validate(instance(t, calls[0].body)); err != nil {
		t.Fatalf("hangup body violates HangupRequest: %v", err)
	}
}

func TestHangupEscapesCallControlID(t *testing.T) {
	f, c := newFake(t, http.StatusOK, `{}`)
	if err := c.Hangup(context.Background(), "v3:a/../b", "cmd"); err != nil {
		t.Fatal(err)
	}
	if got := f.recorded()[0].path; strings.Contains(got, "/../") {
		t.Fatalf("path traversal not escaped: %s", got)
	}
}

func TestHangupRequiresIDs(t *testing.T) {
	_, c := newFake(t, http.StatusOK, `{}`)
	if err := c.Hangup(context.Background(), "", "cmd"); !errors.Is(err, ErrInvalidDial) {
		t.Fatalf("err %v", err)
	}
	if err := c.Hangup(context.Background(), "v3:abc", ""); !errors.Is(err, ErrInvalidDial) {
		t.Fatalf("err %v", err)
	}
}

func TestNewClientRejectsUnsafeBaseURL(t *testing.T) {
	cases := map[string]string{
		"remote plaintext": "http://api.example.com/v2",
		"userinfo":         "https://u:p@api.telnyx.com/v2",
		"query":            "https://api.telnyx.com/v2?x=1",
		"ftp":              "ftp://api.telnyx.com/v2",
		"no host":          "https:///v2",
	}
	for name, u := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewClient(u, testAPIKey, nil); !errors.Is(err, ErrInvalidClient) {
				t.Fatalf("accepted %s: %v", u, err)
			}
		})
	}
	if _, err := NewClient("http://127.0.0.1:9999/v2", testAPIKey, nil); err != nil {
		t.Fatalf("loopback plaintext rejected: %v", err)
	}
	if _, err := NewClient(DefaultBaseURL, "", nil); !errors.Is(err, ErrInvalidClient) {
		t.Fatalf("empty key accepted: %v", err)
	}
}
