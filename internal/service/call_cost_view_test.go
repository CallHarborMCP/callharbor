package service

import (
	"testing"

	"github.com/CallHarborMCP/callharbor/internal/storage"
)

func micro(v int64) *int64 { return &v }

// TestCostViewRule covers the three statuses and the precedence between them:
// a known provider cost wins, then an estimate, then unavailable. An error from
// the provider does not win, so it falls through to the estimate.
func TestCostViewRule(t *testing.T) {
	cases := []struct {
		name       string
		call       storage.Call
		wantStatus string
		wantAmount *string
		wantSource *string
	}{
		{
			name:       "known provider cost",
			call:       storage.Call{Provider: storage.CostFigure{Status: "success", AmountMicros: micro(10600), Currency: "USD"}},
			wantStatus: CostKnown, wantAmount: strp("0.0106"), wantSource: strp(CostSourceProvider),
		},
		{
			name: "provider wins over estimate",
			call: storage.Call{
				Provider: storage.CostFigure{Status: "success", AmountMicros: micro(10600), Currency: "USD"},
				Estimate: storage.CostFigure{Status: "estimated", AmountMicros: micro(50000), Currency: "USD"},
			},
			wantStatus: CostKnown, wantAmount: strp("0.0106"), wantSource: strp(CostSourceProvider),
		},
		{
			name: "provider error falls back to estimate",
			call: storage.Call{
				Provider: storage.CostFigure{Status: "error"},
				Estimate: storage.CostFigure{Status: "estimated", AmountMicros: micro(50000), Currency: "USD"},
			},
			wantStatus: CostEstimated, wantAmount: strp("0.05"), wantSource: strp(CostSourceEstimate),
		},
		{
			name:       "estimate only",
			call:       storage.Call{Estimate: storage.CostFigure{Status: "estimated", AmountMicros: micro(2000000), Currency: "USD"}},
			wantStatus: CostEstimated, wantAmount: strp("2"), wantSource: strp(CostSourceEstimate),
		},
		{
			name:       "nothing recorded",
			call:       storage.Call{},
			wantStatus: CostUnavailable,
		},
		{
			name:       "mixed-currency success has no amount, so unavailable",
			call:       storage.Call{Provider: storage.CostFigure{Status: "success"}},
			wantStatus: CostUnavailable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := costView(tc.call)
			if got.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", got.Status, tc.wantStatus)
			}
			if !eqStr(got.Amount, tc.wantAmount) || !eqStr(got.Source, tc.wantSource) {
				t.Fatalf("amount=%v source=%v, want amount=%v source=%v",
					deref(got.Amount), deref(got.Source), deref(tc.wantAmount), deref(tc.wantSource))
			}
			if tc.wantStatus == CostUnavailable && (got.Amount != nil || got.Currency != nil || got.Source != nil) {
				t.Fatalf("unavailable must have null fields: %+v", got)
			}
		})
	}
}

func strp(s string) *string { return &s }

func eqStr(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}
