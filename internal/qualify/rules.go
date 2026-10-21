package qualify

import (
	"cmp"
	"slices"

	"github.com/daniil4545/lead-qualifier/internal/config"
)

// Rules - the decision rules of one catalog: tariffs, niche segments and refusal reasons.
// Built once at start; all thresholds and prices come from the catalog.
type Rules struct {
	tariffs      []config.Tariff // ascending by threshold
	segments     map[string]config.Segment
	reasons      map[string]config.RefusalReason
	minThreshold int64
	prices       map[int64]bool
}

// NicheUnclear - the code of an unconfirmed profile: the classifier did not map the
// service or named a value outside the list. Its threshold is the minimum tariff threshold.
const NicheUnclear = "unclear"

// NewRules builds the rules from a validated catalog.
func NewRules(catalog config.Catalog) Rules {
	rules := Rules{
		tariffs:      slices.Clone(catalog.Tariffs),
		segments:     make(map[string]config.Segment, len(catalog.Segments)),
		reasons:      make(map[string]config.RefusalReason, len(catalog.RefusalReasons)),
		minThreshold: catalog.MinTariffThreshold(),
		prices:       make(map[int64]bool, len(catalog.Tariffs)),
	}
	slices.SortFunc(rules.tariffs, func(a, b config.Tariff) int { return cmp.Compare(a.Threshold, b.Threshold) })
	for _, segment := range catalog.Segments {
		rules.segments[segment.Code] = segment
	}
	for _, reason := range catalog.RefusalReasons {
		rules.reasons[reason.Code] = reason
	}
	for _, tariff := range catalog.Tariffs {
		rules.prices[tariff.Price] = true
	}
	return rules
}

// TariffFor - the lead's tariff by the lower bound of revenue: the price is conservative. The threshold
// is inclusive. Revenue below the minimum threshold or unintelligible means no tariff,
// and the bot does not name a price.
func (r Rules) TariffFor(facts Facts) (config.Tariff, bool) {
	lower, _, ok := revenueBounds(facts)
	if !ok {
		return config.Tariff{}, false
	}
	var found config.Tariff
	matched := false
	for _, tariff := range r.tariffs {
		if lower >= tariff.Threshold {
			found, matched = tariff, true
		}
	}
	return found, matched
}

// IsCatalogPrice - the amount equals the price of some catalog tariff.
func (r Rules) IsCatalogPrice(amount int64) bool {
	return r.prices[amount]
}

// Prices - the prices of all catalog tariffs.
func (r Rules) Prices() []int64 {
	prices := make([]int64, 0, len(r.tariffs))
	for _, tariff := range r.tariffs {
		prices = append(prices, tariff.Price)
	}
	return prices
}

// Segments and Reasons - the catalog lists for the niche classifier.
func (r Rules) Segments() []config.Segment {
	segments := make([]config.Segment, 0, len(r.segments))
	for _, segment := range r.segments {
		segments = append(segments, segment)
	}
	slices.SortFunc(segments, func(a, b config.Segment) int { return cmp.Compare(a.Code, b.Code) })
	return segments
}

func (r Rules) Reasons() []config.RefusalReason {
	reasons := make([]config.RefusalReason, 0, len(r.reasons))
	for _, reason := range r.reasons {
		reasons = append(reasons, reason)
	}
	slices.SortFunc(reasons, func(a, b config.RefusalReason) int { return cmp.Compare(a.Code, b.Code) })
	return reasons
}

// NicheVerdict - the niche classifier verdict. Untrusted input: Go accepts
// only catalog codes.
type NicheVerdict struct {
	Direction        string
	OffProfileReason string
}

// NormalizeVerdict brings the verdict to catalog codes. A direction outside the list is a
// classifier failure, not a decision: the refusal reason is not in force then, and the cost of
// a classifier error is a meeting, not a lost lead.
func (r Rules) NormalizeVerdict(verdict NicheVerdict) (direction, reason string) {
	_, known := r.segments[verdict.Direction]
	switch {
	case known:
		direction = verdict.Direction
	case verdict.Direction != "" && verdict.Direction != NicheUnclear:
		return NicheUnclear, ""
	default:
		direction = NicheUnclear
	}
	if _, ok := r.reasons[verdict.OffProfileReason]; ok {
		reason = verdict.OffProfileReason
	}
	return direction, reason
}

// ApplyVerdict puts the normalized verdict into the facts, binding it to the current
// service: the lead correcting the service makes the verdict stale.
func (r Rules) ApplyVerdict(facts Facts, verdict NicheVerdict) {
	direction, reason := r.NormalizeVerdict(verdict)
	facts[factNiche] = direction
	facts[factNicheOf] = NormalizeText(facts.Value(FactService))
	if reason != "" {
		facts[factNicheOff] = reason
	} else {
		delete(facts, factNicheOff)
	}
}

// NicheKnown - the verdict is computed for the current service. Without it the gate would pass
// anyone, so windows are not offered before classification.
func NicheKnown(facts Facts) bool {
	service := facts.Value(FactService)
	return service == "" || facts.Value(factNicheOf) == NormalizeText(service)
}

// Segment - the lead's direction and the refusal reason. The reason applies only if
// there is no target direction: a mix is resolved in favor of a meeting.
func (r Rules) Segment(facts Facts) (segment, offReason string) {
	code := facts.Value(factNiche)
	_, target := r.segments[code]
	if !target {
		code = NicheUnclear
	}
	if reason := facts.Value(factNicheOff); reason != "" && !target {
		return "", reason
	}
	return code, ""
}

// Mixed - the lead named both a non-target niche and a target direction.
func (r Rules) Mixed(facts Facts) bool {
	if facts.Value(factNicheOff) == "" {
		return false
	}
	_, target := r.segments[facts.Value(factNiche)]
	return target
}

// SegmentName and ReasonName - human-readable names for manager notes.
func (r Rules) SegmentName(code string) string {
	if segment, ok := r.segments[code]; ok {
		return segment.Name
	}
	return "профиль не подтверждён"
}

func (r Rules) ReasonName(code string) string {
	if reason, ok := r.reasons[code]; ok {
		return reason.Name
	}
	return code
}

// Threshold - the minimum revenue of a segment for a meeting.
func (r Rules) Threshold(segment string) int64 {
	if found, ok := r.segments[segment]; ok && found.Threshold > 0 {
		return found.Threshold
	}
	return r.minThreshold
}

// Gate - the outcome of the meeting booking gate.
type Gate string

const (
	GateOffer      Gate = "offer"
	GateTooEarly   Gate = "too_early"
	GateOffProfile Gate = "off_profile"
	// GateMixedNiche - a mix of niches: a meeting with a note for the manager, no question to the lead.
	GateMixedNiche Gate = "mixed_niche"
)

// AllowsMeeting - the gate lets through to windows.
func (g Gate) AllowsMeeting() bool {
	return g == GateOffer || g == GateMixedNiche
}

// Gate decides the outcome from the collected facts. Order: niche, then revenue, since
// it is pointless to suggest that a lead from an excluded niche return with larger revenue.
// Revenue is compared by the upper bound: the gate is permissive, the price is not.
// Uncertainty (unintelligible revenue) is resolved in favor of a meeting.
func (r Rules) Gate(facts Facts) Gate {
	segment, reason := r.Segment(facts)
	if reason != "" {
		return GateOffProfile
	}
	if _, upper, ok := revenueBounds(facts); ok && upper < r.Threshold(segment) {
		return GateTooEarly
	}
	if r.Mixed(facts) {
		return GateMixedNiche
	}
	return GateOffer
}

// Reasons why windows and booking are not due to a lead.
const (
	BlockMeetingScheduled = "meeting_scheduled"
	BlockNicheUnknown     = "niche_unknown"
	BlockFactsMissing     = "facts_missing"
	BlockGateClosed       = "gate_closed"
)

// MeetingBlock - the booking boundary: windows and booking only after the niche verdict, with
// a full set of facts, an open gate and no appointed meeting. An empty
// string means allowed. Checked by both the agent (as a tool answer) and dialog before booking:
// model output is untrusted.
func (r Rules) MeetingBlock(status Status, facts Facts) string {
	switch {
	case status == StatusMeetingScheduled:
		return BlockMeetingScheduled
	case !NicheKnown(facts):
		return BlockNicheUnknown
	case len(facts.Missing()) > 0:
		return BlockFactsMissing
	case !r.Gate(facts).AllowsMeeting():
		return BlockGateClosed
	}
	return ""
}
