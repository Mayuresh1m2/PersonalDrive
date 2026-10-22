package engine

import (
	"fmt"
	"time"
)

// ClassPricing is approximate AWS us-east-1 pricing (USD). It's used for
// estimates shown before restores and prunes, not billing. Override in
// config if your provider or region differs.
type ClassPricing struct {
	StoragePerGBMonth float64
	PutPer1000        float64
	GetPer1000        float64
	MinDays           int
	// Retrieval by tier: per-GB and per-1000-requests, plus typical wait.
	Retrieval map[string]TierPricing
}

type TierPricing struct {
	PerGB      float64
	Per1000    float64
	TypicalETA time.Duration
}

type Pricing struct {
	Classes     map[string]ClassPricing
	EgressPerGB float64
}

// DefaultPricing reflects AWS us-east-1 list prices in 2025–2026. Verify
// current prices; they change.
func DefaultPricing() Pricing {
	return Pricing{
		EgressPerGB: 0.09,
		Classes: map[string]ClassPricing{
			"STANDARD":    {StoragePerGBMonth: 0.023, PutPer1000: 0.005, GetPer1000: 0.0004},
			"STANDARD_IA": {StoragePerGBMonth: 0.0125, PutPer1000: 0.01, GetPer1000: 0.001, MinDays: 30, Retrieval: map[string]TierPricing{"": {PerGB: 0.01}}},
			"GLACIER_IR":  {StoragePerGBMonth: 0.004, PutPer1000: 0.02, GetPer1000: 0.01, MinDays: 90, Retrieval: map[string]TierPricing{"": {PerGB: 0.03}}},
			"GLACIER": {StoragePerGBMonth: 0.0036, PutPer1000: 0.03, GetPer1000: 0.0004, MinDays: 90, Retrieval: map[string]TierPricing{
				"Expedited": {PerGB: 0.03, Per1000: 10, TypicalETA: 5 * time.Minute},
				"Standard":  {PerGB: 0.01, Per1000: 0.05, TypicalETA: 5 * time.Hour},
				"Bulk":      {PerGB: 0, Per1000: 0, TypicalETA: 12 * time.Hour},
			}},
			"DEEP_ARCHIVE": {StoragePerGBMonth: 0.00099, PutPer1000: 0.05, GetPer1000: 0.0004, MinDays: 180, Retrieval: map[string]TierPricing{
				"Standard": {PerGB: 0.02, Per1000: 0.10, TypicalETA: 12 * time.Hour},
				"Bulk":     {PerGB: 0.0025, Per1000: 0.025, TypicalETA: 48 * time.Hour},
			}},
			"LOCAL": {},
		},
	}
}

func (p Pricing) class(c string) ClassPricing {
	if c == "" {
		c = "STANDARD"
	}
	if cp, ok := p.Classes[c]; ok {
		return cp
	}
	return p.Classes["STANDARD"]
}

// MinDays returns the minimum billed storage duration for a class.
func (p Pricing) MinDays(class string) int { return p.class(class).MinDays }

// Estimate is a restore cost breakdown.
type Estimate struct {
	Retrieval float64       `json:"retrieval_usd"`
	Requests  float64       `json:"requests_usd"`
	Egress    float64       `json:"egress_usd"`
	Total     float64       `json:"total_usd"`
	ETA       time.Duration `json:"eta"`
	Notes     []string      `json:"notes,omitempty"`
}

func (e Estimate) String() string {
	return fmt.Sprintf("~$%.2f (retrieval $%.2f + requests $%.2f + download $%.2f), wait up to %s",
		e.Total, e.Retrieval, e.Requests, e.Egress, humanDuration(e.ETA))
}

func humanDuration(d time.Duration) string {
	switch {
	case d == 0:
		return "none"
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}

const gb = 1 << 30

// estimate computes costs for thawing archived packs (by full object size),
// GET requests and downloaded bytes.
func (p Pricing) estimate(local bool, thaw map[string]int64, thawCount map[string]int, tier string, gets int, download int64) Estimate {
	var e Estimate
	if local {
		return e
	}
	for class, bytes := range thaw {
		cp := p.class(class)
		tp, ok := cp.Retrieval[tier]
		if !ok {
			tp = cp.Retrieval[""]
			if tier != "" && len(cp.Retrieval) > 1 {
				e.Notes = append(e.Notes, fmt.Sprintf("tier %q is not available for %s", tier, class))
			}
		}
		e.Retrieval += float64(bytes) / gb * tp.PerGB
		e.Requests += float64(thawCount[class]) / 1000 * tp.Per1000
		e.ETA = max(e.ETA, tp.TypicalETA)
	}
	e.Requests += float64(gets) / 1000 * p.class("STANDARD").GetPer1000
	e.Egress = float64(download) / gb * p.EgressPerGB
	e.Total = e.Retrieval + e.Requests + e.Egress
	return e
}
