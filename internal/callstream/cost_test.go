package callstream

import (
	"testing"

	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
)

func TestProviderCostFromReportMixedCurrencyIsUnknown(t *testing.T) {
	rep := telnyx.CostReport{Status: telnyx.CostStatusSuccess, TotalCost: "0.0106", Parts: []telnyx.CostPart{
		{Cost: "0.0050", Currency: "USD"}, {Cost: "0.0040", Currency: "EUR"},
	}}
	got := ProviderCostFromReport(rep, "call.cost|m")
	if got.AmountMicros != nil || got.Currency != "" {
		t.Fatalf("mixed currency stored a figure: %+v", got)
	}
	if got.Status != telnyx.CostStatusSuccess {
		t.Fatalf("status = %q, want success kept", got.Status)
	}
}

func TestProviderCostFromReportMissingCurrencyIsUnknown(t *testing.T) {
	rep := telnyx.CostReport{Status: telnyx.CostStatusSuccess, TotalCost: "0.0106", Parts: []telnyx.CostPart{{Cost: "0.0106"}}}
	if got := ProviderCostFromReport(rep, "e"); got.AmountMicros != nil {
		t.Fatalf("unnamed currency stored a figure: %+v", got)
	}
}

func TestProviderCostFromReportSuccessIsExactMicros(t *testing.T) {
	rep := telnyx.CostReport{Status: telnyx.CostStatusSuccess, TotalCost: "0.0106", Parts: []telnyx.CostPart{
		{Cost: "0.0050", Currency: "usd"}, {Cost: "0.0038", Currency: "USD"}, {Cost: "0.0018", Currency: "USD"},
	}}
	got := ProviderCostFromReport(rep, "e")
	if got.AmountMicros == nil || *got.AmountMicros != 10600 || got.Currency != "USD" {
		t.Fatalf("cost = %+v, want 10600 micros USD", got)
	}
}

func TestProviderCostFromReportErrorStoresNoAmount(t *testing.T) {
	rep := telnyx.CostReport{Status: telnyx.CostStatusError, TotalCost: "0.0106", Parts: []telnyx.CostPart{{Cost: "0.0106", Currency: "USD"}}}
	got := ProviderCostFromReport(rep, "e")
	if got.AmountMicros != nil || got.Status != telnyx.CostStatusError {
		t.Fatalf("error report = %+v, want no amount", got)
	}
}
