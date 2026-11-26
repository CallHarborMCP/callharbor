package telnyx

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/CallHarborMCP/callharbor/internal/audio"
)

// dialAllowlist is the complete set of JSON keys CallHarbor may send in a
// dial body. Telnyx is telephony-only (#114): any new key is a boundary change
// and must be added here deliberately.
var dialAllowlist = map[string]bool{
	"connection_id":                          true,
	"to":                                     true,
	"from":                                   true,
	"webhook_url":                            true,
	"webhook_url_method":                     true,
	"command_id":                             true,
	"client_state":                           true,
	"timeout_secs":                           true,
	"time_limit_secs":                        true,
	"stream_url":                             true,
	"stream_track":                           true,
	"stream_codec":                           true,
	"stream_bidirectional_mode":              true,
	"stream_bidirectional_codec":             true,
	"stream_bidirectional_sampling_rate":     true,
	"stream_bidirectional_target_legs":       true,
	"stream_auth_token":                      true, // only when set
	"stream_establish_before_call_originate": true,
}

// forbiddenAddonFields are pinned CallRequest properties for Telnyx paid
// add-ons and AI features. None may ever appear in a dial body (#114).
var forbiddenAddonFields = []string{
	"answering_machine_detection",
	"answering_machine_detection_config",
	"assistant",
	"transcription",
	"transcription_config",
	"record",
	"record_channels",
	"record_format",
	"record_max_length",
	"record_timeout_secs",
	"record_track",
	"record_trim",
	"record_custom_file_name",
	"conversation_relay_config",
	"dialogflow_config",
	"enable_dialogflow",
	"deepfake_detection",
	"sound_modifications",
	"audio_url",
	"media_name",
	"send_silence_when_idle",
}

// officialCallRequestProperties reads the pinned CallRequest property names
// directly from the upstream extract, so the forbidden list can be checked
// against what Telnyx really defines.
func officialCallRequestProperties(t *testing.T) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(contractPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Components struct {
			Schemas struct {
				CallRequest struct {
					Properties map[string]json.RawMessage `json:"properties"`
				} `json:"CallRequest"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	props := doc.Components.Schemas.CallRequest.Properties
	if len(props) == 0 {
		t.Fatal("pinned CallRequest has no properties")
	}
	out := make(map[string]bool, len(props))
	for k := range props {
		out[k] = true
	}
	return out
}

// sortedKeys returns the keys of set in sorted order.
func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k, v := range set {
		if v {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// jsonObjectKeys returns the sorted top-level keys of a JSON object.
func jsonObjectKeys(t *testing.T, b []byte) []string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("body is not a JSON object: %v: %s", err, b)
	}
	set := make(map[string]bool, len(m))
	for k := range m {
		set[k] = true
	}
	return sortedKeys(set)
}

// assertWithinBoundary fails if a dial body carries a key outside the
// allowlist or any forbidden add-on key.
func assertWithinBoundary(t *testing.T, body []byte) {
	t.Helper()
	for _, k := range jsonObjectKeys(t, body) {
		if !dialAllowlist[k] {
			t.Errorf("dial body key %q is outside the telephony-only allowlist: %s", k, body)
		}
	}
	for _, bad := range forbiddenAddonFields {
		if strings.Contains(string(body), `"`+bad+`"`) {
			t.Errorf("dial body carries forbidden add-on key %q: %s", bad, body)
		}
	}
}

func TestForbiddenAddonFieldsExistInPinnedSchema(t *testing.T) {
	props := officialCallRequestProperties(t)
	for _, f := range forbiddenAddonFields {
		if !props[f] {
			t.Errorf("forbidden field %q is not a pinned CallRequest property; the list is stale", f)
		}
	}
	for k := range dialAllowlist {
		if !props[k] {
			t.Errorf("allowlisted key %q is not a pinned CallRequest property", k)
		}
	}
	for _, f := range forbiddenAddonFields {
		if dialAllowlist[f] {
			t.Errorf("%q is both allowlisted and forbidden", f)
		}
	}
}

func TestDialBodyKeysAreExactlyAllowlisted(t *testing.T) {
	for _, codec := range []audio.Format{
		{Encoding: audio.PCMU, SampleRate: 8000},
		{Encoding: audio.PCMA, SampleRate: 8000},
		{Encoding: audio.L16, SampleRate: 16000, LittleEndian: true},
	} {
		t.Run(string(codec.Encoding), func(t *testing.T) {
			p := validDial()
			p.Codec = codec
			r, err := BuildDialRequest(p)
			if err != nil {
				t.Fatal(err)
			}
			b, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			// stream_auth_token is omitted because validDial sets none.
			want := map[string]bool{}
			for k := range dialAllowlist {
				want[k] = true
			}
			delete(want, "stream_auth_token")
			if got := jsonObjectKeys(t, b); !reflect.DeepEqual(got, sortedKeys(want)) {
				t.Fatalf("dial keys\n got %v\nwant %v\nbody %s", got, sortedKeys(want), b)
			}
			assertWithinBoundary(t, b)
		})
	}
}

func TestDialBodyIncludesAuthTokenOnlyWhenSet(t *testing.T) {
	p := validDial()
	p.StreamAuthToken = "opaque-token"
	r, err := BuildDialRequest(p)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	keys := jsonObjectKeys(t, b)
	found := false
	for _, k := range keys {
		if k == "stream_auth_token" {
			found = true
		}
	}
	if !found {
		t.Fatalf("stream_auth_token missing when set: %s", b)
	}
	assertWithinBoundary(t, b)
}

func TestDialRequestJSONFieldsAreAllowlisted(t *testing.T) {
	rt := reflect.TypeOf(DialRequest{})
	got := map[string]bool{}
	for i := range rt.NumField() {
		name, _, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ",")
		if name == "" || name == "-" {
			t.Fatalf("DialRequest field %s has no json name", rt.Field(i).Name)
		}
		got[name] = true
	}
	for _, k := range sortedKeys(got) {
		if !dialAllowlist[k] {
			t.Errorf("DialRequest json field %q is not allowlisted; add it to dialAllowlist only after a telephony-boundary review", k)
		}
	}
	for k := range dialAllowlist {
		if !got[k] {
			t.Errorf("allowlisted key %q has no DialRequest field", k)
		}
	}
}

func TestDialParamsHasNoAddonFields(t *testing.T) {
	rt := reflect.TypeOf(DialParams{})
	for i := range rt.NumField() {
		name := strings.ToLower(rt.Field(i).Name)
		for _, bad := range []string{"amd", "machine", "transcri", "record", "assistant"} {
			if strings.Contains(name, bad) {
				t.Errorf("DialParams field %s looks like a forbidden Telnyx add-on (%s)", rt.Field(i).Name, bad)
			}
		}
	}
}

func TestHangupAndDialBodiesStayWithinBoundary(t *testing.T) {
	f, c := newFake(t, http.StatusCreated, acceptedDial)
	ctx := t.Context()
	if _, err := c.Dial(ctx, validDial()); err != nil {
		t.Fatal(err)
	}
	if err := c.Hangup(ctx, "v3:abc", "call-7f3a:hangup"); err != nil {
		t.Fatal(err)
	}
	calls := f.recorded()
	if len(calls) != 2 {
		t.Fatalf("requests %d, want dial and hangup", len(calls))
	}
	dial, hangup := calls[0], calls[1]
	if dial.path != "/v2/calls" {
		t.Fatalf("first request %s, want dial", dial.path)
	}
	if hangup.path != "/v2/calls/v3:abc/actions/hangup" {
		t.Fatalf("second request %s, want hangup", hangup.path)
	}
	if got := string(hangup.body); got != `{"command_id":"call-7f3a:hangup"}` {
		t.Fatalf("hangup body %s, want exactly command_id", got)
	}
	assertWithinBoundary(t, dial.body)
}

// A signed call.machine.detection.ended webhook is acknowledged but never
// dispatched and never recorded as a receipt. The harness fails every dispatch,
// so a 200 proves dispatch did not run.
func TestMachineDetectionWebhookIsAckedWithoutDispatch(t *testing.T) {
	h := newIngressHarness(t)
	h.fail = errors.New("dispatch must not run for machine detection")
	body := loadWebhook(t, "callMachineDetectionEnded")
	if code := h.post(t, body, Sign(h.priv, h.now, body)); code != http.StatusOK {
		t.Fatalf("status %d, want 200 ack", code)
	}
	if len(h.signals) != 0 || len(h.receipts.seen) != 0 {
		t.Fatalf("machine detection changed state: signals=%d receipts=%d", len(h.signals), len(h.receipts.seen))
	}
}
