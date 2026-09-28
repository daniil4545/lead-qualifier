package qualify

import (
	"cmp"
	"slices"

	"github.com/daniil4545/lead-qualifier/internal/config"
)

// Rules - правила решения одного каталога: тарифы, сегменты ниш и поводы отказа.
// Строится один раз при старте; все пороги и цены приходят из каталога.
type Rules struct {
	tariffs      []config.Tariff // по возрастанию порога
	segments     map[string]config.Segment
	reasons      map[string]config.RefusalReason
	minThreshold int64
	prices       map[int64]bool
}

// NicheUnclear - код неподтверждённого профиля: классификатор не разложил
// услугу либо назвал значение вне перечня. Порог у него - минимальный тарифный.
const NicheUnclear = "unclear"

// NewRules собирает правила из проверенного каталога.
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

// TariffFor - тариф лида по нижней границе выручки: цена консервативна. Порог
// включительный. Выручка ниже минимального порога или непонятная - тарифа нет,
// и цену бот не называет.
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

// IsCatalogPrice - сумма совпадает с ценой какого-либо тарифа каталога.
func (r Rules) IsCatalogPrice(amount int64) bool {
	return r.prices[amount]
}

// Prices - цены всех тарифов каталога.
func (r Rules) Prices() []int64 {
	prices := make([]int64, 0, len(r.tariffs))
	for _, tariff := range r.tariffs {
		prices = append(prices, tariff.Price)
	}
	return prices
}

// Segments и Reasons - перечни каталога для классификатора ниши.
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

// NicheVerdict - вердикт классификатора ниши. Недоверенный ввод: Go принимает
// только коды каталога.
type NicheVerdict struct {
	Direction        string
	OffProfileReason string
}

// NormalizeVerdict приводит вердикт к кодам каталога. Направление вне перечня -
// сбой классификатора, а не решение: повод отказа при нём не действует, цена
// ошибки классификатора - встреча, а не потерянный лид.
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

// ApplyVerdict кладёт нормализованный вердикт в факты, привязав его к текущей
// услуге: поправка услуги лидом делает вердикт устаревшим.
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

// NicheKnown - вердикт посчитан для текущей услуги. Без него гейт пропустил бы
// любого, поэтому окна до классификации не предлагаются.
func NicheKnown(facts Facts) bool {
	service := facts.Value(FactService)
	return service == "" || facts.Value(factNicheOf) == NormalizeText(service)
}

// Segment - направление лида и повод отказа. Повод действует, только если
// целевого направления нет: смесь решается в пользу встречи.
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

// Mixed - лид назвал и нецелевую нишу, и целевое направление.
func (r Rules) Mixed(facts Facts) bool {
	if facts.Value(factNicheOff) == "" {
		return false
	}
	_, target := r.segments[facts.Value(factNiche)]
	return target
}

// SegmentName и ReasonName - человекочитаемые имена для заметок менеджеру.
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

// Threshold - минимальная выручка сегмента для встречи.
func (r Rules) Threshold(segment string) int64 {
	if found, ok := r.segments[segment]; ok && found.Threshold > 0 {
		return found.Threshold
	}
	return r.minThreshold
}

// Gate - исход гейта записи на встречу.
type Gate string

const (
	GateOffer      Gate = "offer"
	GateTooEarly   Gate = "too_early"
	GateOffProfile Gate = "off_profile"
	// GateMixedNiche - смесь ниш: встреча с пометкой менеджеру, без вопроса лиду.
	GateMixedNiche Gate = "mixed_niche"
)

// AllowsMeeting - гейт пропускает к окнам.
func (g Gate) AllowsMeeting() bool {
	return g == GateOffer || g == GateMixedNiche
}

// Gate решает исход по собранным фактам. Порядок: ниша, затем выручка - лиду
// из исключённой ниши бессмысленно предлагать вернуться с большей выручкой.
// Выручка сравнивается по верхней границе: гейт пропускающий, цена - нет.
// Неопределённость (непонятная выручка) решается в пользу встречи.
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

// Причины, по которым окна и бронь не положены лиду.
const (
	BlockMeetingScheduled = "meeting_scheduled"
	BlockNicheUnknown     = "niche_unknown"
	BlockFactsMissing     = "facts_missing"
	BlockGateClosed       = "gate_closed"
)

// MeetingBlock - рубеж записи: окна и бронь только после вердикта ниши, при
// полном наборе фактов, открытом гейте и без назначенной встречи. Пустая
// строка - можно. Проверяют и агент (ответом инструмента), и dialog до брони:
// вывод модели недоверенный.
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
