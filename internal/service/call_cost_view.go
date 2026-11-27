package service

import (
	"github.com/CallHarborMCP/callharbor/internal/config"
	"github.com/CallHarborMCP/callharbor/internal/storage"
)

// Cost status and source values a CallView reports. They are public, so they
// are fixed strings.
const (
	CostKnown       = "known"
	CostEstimated   = "estimated"
	CostUnavailable = "unavailable"

	CostSourceProvider = "provider"
	CostSourceEstimate = "estimate"
)

// CostView is the cost a call shows. Status is known, estimated or unavailable.
// A known cost is the provider's reported charge. An estimated cost is the
// pre-dial figure from the operator's rate. Amount is a decimal string, never a
// float, and is null when the status is unavailable.
type CostView struct {
	Status   string  `json:"status"`
	Amount   *string `json:"amount"`
	Currency *string `json:"currency"`
	Source   *string `json:"source"`
}

// costView applies the rule: a known provider cost wins; otherwise an estimate
// that was recorded; otherwise unavailable. A provider error does not win, so
// it falls through to the estimate. Both figures stay in the store, so this is
// a read-time choice and never overwrites either one.
func costView(c storage.Call) CostView {
	if c.Provider.Status == "success" && c.Provider.AmountMicros != nil && c.Provider.Currency != "" {
		return figureView(CostKnown, CostSourceProvider, c.Provider)
	}
	if c.Estimate.Status == "estimated" && c.Estimate.AmountMicros != nil && c.Estimate.Currency != "" {
		return figureView(CostEstimated, CostSourceEstimate, c.Estimate)
	}
	return CostView{Status: CostUnavailable}
}

func figureView(status, source string, f storage.CostFigure) CostView {
	amount := config.FormatMicros(*f.AmountMicros)
	cur := f.Currency
	src := source
	return CostView{Status: status, Amount: &amount, Currency: &cur, Source: &src}
}
