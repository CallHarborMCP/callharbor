package telnyx

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
)

const applicationBody = `{"data":{"id":"1293384261075731499","application_name":"call-router","active":true,"record_type":"call_control_application"}}`

const numberBody = `{"data":[{"id":"num-1","phone_number":"+13125550100","status":"active","connection_id":"1293384261075731499","record_type":"phone_number"}],"meta":{"total_results":1}}`

func TestGetCallControlApplicationReadsOnlyAndMatchesSchema(t *testing.T) {
	f, c := newFake(t, http.StatusOK, applicationBody)
	app, err := c.GetCallControlApplication(context.Background(), "1293384261075731499")
	if err != nil {
		t.Fatal(err)
	}
	if app.ID != "1293384261075731499" || !app.Active {
		t.Fatalf("app = %+v", app)
	}
	calls := f.recorded()
	if len(calls) != 1 || calls[0].method != http.MethodGet {
		t.Fatalf("calls = %+v, want one GET", calls)
	}
	if calls[0].path != "/v2/call_control_applications/1293384261075731499" {
		t.Fatalf("path %s", calls[0].path)
	}
	if len(calls[0].body) != 0 {
		t.Fatalf("GET carried a body: %s", calls[0].body)
	}
	if err := compileSchema(t, "/components/schemas/CallControlApplication").Validate(instance(t, []byte(`{"id":"1293384261075731499","application_name":"call-router","active":true}`))); err != nil {
		t.Fatalf("fixture violates CallControlApplication: %v", err)
	}
}

func TestGetCallControlApplicationInactiveAndNotFound(t *testing.T) {
	_, c := newFake(t, http.StatusOK, `{"data":{"id":"app-1","active":false}}`)
	app, err := c.GetCallControlApplication(context.Background(), "app-1")
	if err != nil || app.Active {
		t.Fatalf("app=%+v err=%v, want inactive", app, err)
	}

	_, c = newFake(t, http.StatusNotFound, `{"errors":[{"detail":"not found"}]}`)
	if _, err := c.GetCallControlApplication(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err %v, want ErrNotFound", err)
	}
}

func TestGetCallControlApplicationRejectsMalformed(t *testing.T) {
	for name, body := range map[string]string{
		"not json": `{"data":`,
		"no data":  `{"meta":{}}`,
		"no id":    `{"data":{"active":true}}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, c := newFake(t, http.StatusOK, body)
			if _, err := c.GetCallControlApplication(context.Background(), "app"); !errors.Is(err, ErrBadResponse) {
				t.Fatalf("err %v, want ErrBadResponse", err)
			}
		})
	}
}

func TestGetCallControlApplicationRequiresID(t *testing.T) {
	f, c := newFake(t, http.StatusOK, applicationBody)
	if _, err := c.GetCallControlApplication(context.Background(), ""); !errors.Is(err, ErrInvalidClient) {
		t.Fatalf("err %v", err)
	}
	if n := len(f.recorded()); n != 0 {
		t.Fatalf("empty id reached the network %d times", n)
	}
}

func TestFindPhoneNumberFiltersExactE164(t *testing.T) {
	f, c := newFake(t, http.StatusOK, numberBody)
	n, err := c.FindPhoneNumber(context.Background(), "+13125550100")
	if err != nil {
		t.Fatal(err)
	}
	if n.ID != "num-1" || n.Status != "active" || n.ConnectionID != "1293384261075731499" {
		t.Fatalf("number = %+v", n)
	}
	calls := f.recorded()
	if calls[0].method != http.MethodGet || calls[0].path != "/v2/phone_numbers" {
		t.Fatalf("call = %+v", calls[0])
	}
	if !strings.Contains(calls[0].query, "filter%5Bphone_number%5D=%2B13125550100") {
		t.Fatalf("query %q does not filter the exact number", calls[0].query)
	}
	if err := compileSchema(t, "/components/schemas/numbers_PhoneNumberDetailed").Validate(instance(t, []byte(
		`{"id":"num-1","record_type":"phone_number","phone_number":"+13125550100","country_iso_alpha2":"US","status":"active","tags":[],"external_pin":null,"created_at":"2026-11-25T10:00:00.000Z","phone_number_type":"local","purchased_at":"2026-11-25T10:00:00.000Z","deletion_lock_enabled":false,"connection_id":"1293384261075731499"}`))); err != nil {
		t.Fatalf("fixture violates numbers_PhoneNumberDetailed: %v", err)
	}
}

func TestFindPhoneNumberIgnoresNonExactMatches(t *testing.T) {
	_, c := newFake(t, http.StatusOK, `{"data":[{"id":"other","phone_number":"+13125550199","status":"active"}]}`)
	if _, err := c.FindPhoneNumber(context.Background(), "+13125550100"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err %v, want ErrNotFound", err)
	}
}

func TestFindPhoneNumberEmptyIsNotFound(t *testing.T) {
	_, c := newFake(t, http.StatusOK, `{"data":[],"meta":{"total_results":0}}`)
	if _, err := c.FindPhoneNumber(context.Background(), "+13125550100"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err %v, want ErrNotFound", err)
	}
}

func TestFindPhoneNumberRejectsMalformed(t *testing.T) {
	_, c := newFake(t, http.StatusOK, `{"meta":{}}`)
	if _, err := c.FindPhoneNumber(context.Background(), "+13125550100"); !errors.Is(err, ErrBadResponse) {
		t.Fatalf("err %v, want ErrBadResponse", err)
	}
	_, c = newFake(t, http.StatusOK, numberBody)
	if _, err := c.FindPhoneNumber(context.Background(), ""); !errors.Is(err, ErrInvalidClient) {
		t.Fatalf("err %v", err)
	}
}

func TestAPIErrorIsTypedAndRedacted(t *testing.T) {
	const secretBody = `{"errors":[{"detail":"echo unit-test-placeholder-token"}]}`
	_, c := newFake(t, http.StatusForbidden, secretBody)
	_, err := c.GetCallControlApplication(context.Background(), "app-1")
	if !errors.Is(err, ErrAPI) {
		t.Fatalf("err %v, want ErrAPI", err)
	}
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusForbidden {
		t.Fatalf("err %v, want *APIError with status 403", err)
	}
	if strings.Contains(err.Error(), testAPIKey) || strings.Contains(err.Error(), "echo") {
		t.Fatalf("error leaked response or key: %q", err.Error())
	}
}

func TestAPIErrorOmitsQueryString(t *testing.T) {
	_, c := newFake(t, http.StatusUnauthorized, `{}`)
	_, err := c.FindPhoneNumber(context.Background(), "+13125550100")
	if !errors.Is(err, ErrAPI) {
		t.Fatalf("err %v", err)
	}
	if strings.Contains(err.Error(), "+1312") || strings.Contains(err.Error(), "filter") {
		t.Fatalf("error carries the query: %q", err.Error())
	}
}

func TestContractSchemasCoverLookups(t *testing.T) {
	var doc struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	b, err := os.ReadFile(contractPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/call_control_applications/{id}", "/phone_numbers"} {
		if _, ok := doc.Paths[p]; !ok {
			t.Errorf("pinned extract lacks %s", p)
		}
	}
}
