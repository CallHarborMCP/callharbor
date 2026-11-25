package api

import (
	"sort"
	"strings"
	"testing"

	"github.com/CallHarborMCP/callharbor/internal/telephonyprofile"
)

// TestTelephonyReasonEnumsMatchServiceCodes pins the documented connectivity_reason
// enums to the service's reason constants. The profile and validation schemas
// and the MCP output schema must list exactly the codes the service can store.
func TestTelephonyReasonEnumsMatchServiceCodes(t *testing.T) {
	want := []string{
		telephonyprofile.ReasonNone,
		telephonyprofile.ReasonSecretsUnavailable,
		telephonyprofile.ReasonInvalidCredentials,
		telephonyprofile.ReasonApplicationNotFound,
		telephonyprofile.ReasonApplicationInactive,
		telephonyprofile.ReasonNumberNotFound,
		telephonyprofile.ReasonNumberInactive,
		telephonyprofile.ReasonNumberNotOnApp,
		telephonyprofile.ReasonProviderUnreachable,
	}
	sort.Strings(want)

	d, _ := loadOpenAPI(t)
	for _, name := range []string{"TelephonyProfile", "TelephonyValidation"} {
		s, ok := d.Components.Schemas[name].(map[string]any)
		if !ok {
			t.Fatalf("%s missing", name)
		}
		props := s["properties"].(map[string]any)
		reason := props["connectivity_reason"].(map[string]any)
		raw, _ := reason["enum"].([]any)
		got := make([]string, 0, len(raw))
		for _, v := range raw {
			got = append(got, v.(string))
		}
		sort.Strings(got)
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("%s.connectivity_reason enum = %q, want %q", name, got, want)
		}
	}
}
