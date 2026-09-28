package qualify

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Facts - снимок фактов кейса. Ключи пяти фактов лида фиксированы кодом, плюс
// три служебных ключа вердикта ниши (niche, niche_off, niche_of).
type Facts map[string]any

const (
	FactService = "service"
	FactRevenue = "monthly_revenue"
	FactRole    = "role"
	FactIsOwner = "is_owner"
	FactCity    = "city"

	factNiche    = "niche"
	factNicheOff = "niche_off"
	factNicheOf  = "niche_of"
)

// RequiredFacts - что обязано быть собрано до гейта записи.
var RequiredFacts = []string{FactService, FactRevenue, FactRole, FactIsOwner, FactCity}

// maxFactChars - потолок длины значения: факт уходит в доверенный контекст
// модели, и длинный текст там был бы произвольным payload.
const maxFactChars = 256

// roleOwner - роль, выведенная из подтверждённого владения.
const roleOwner = "собственник"

// FactDelta - недоверенная дельта фактов из save_facts. nil - поле не трогать.
type FactDelta struct {
	Service *string `json:"service"`
	Revenue *string `json:"monthly_revenue"`
	Role    *string `json:"role"`
	IsOwner *bool   `json:"is_owner"`
	City    *string `json:"city"`
}

// Names - имена полей дельты без значений: для лога.
func (d FactDelta) Names() []string {
	names := []string{}
	set := []bool{d.Service != nil, d.Revenue != nil, d.Role != nil, d.IsOwner != nil, d.City != nil}
	for index, name := range RequiredFacts {
		if set[index] {
			names = append(names, name)
		}
	}
	return names
}

// Value - строковое значение факта; пусто, если факта нет или он не строка.
func (f Facts) Value(key string) string {
	value, _ := f[key].(string)
	return strings.TrimSpace(value)
}

// Known копирует только непустые факты.
func (f Facts) Known() Facts {
	known := make(Facts, len(f))
	for name, value := range f {
		if !isEmptyFact(value) {
			known[name] = value
		}
	}
	return known
}

// Missing - обязательные факты, которых нет в снимке.
func (f Facts) Missing() []string {
	var missing []string
	for _, name := range RequiredFacts {
		if value, ok := f[name]; !ok || isEmptyFact(value) {
			missing = append(missing, name)
		}
	}
	return missing
}

func isEmptyFact(value any) bool {
	switch v := value.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(v) == ""
	default:
		return false
	}
}

// DeriveOwnerRole: подтверждённый собственник без названной роли получает роль
// «собственник». Обратного вывода нет: роль «директор» не делает лида
// собственником, это решает только его прямой ответ.
func DeriveOwnerRole(facts Facts) {
	owner, ok := facts[FactIsOwner].(bool)
	if !ok || !owner {
		return
	}
	if role, exists := facts[FactRole]; exists && !isEmptyFact(role) {
		return
	}
	facts[FactRole] = roleOwner
}

// Merge накладывает дельту на доверенный снимок и возвращает новый снимок.
// Поправка лида побеждает прежнее значение: гейт после неё пересчитывается.
func Merge(trusted Facts, delta FactDelta) Facts {
	merged := trusted.Known()
	mergeString(merged, FactService, delta.Service)
	mergeString(merged, FactRevenue, delta.Revenue)
	mergeRole(merged, delta.Role, delta.IsOwner)
	if delta.IsOwner != nil {
		merged[FactIsOwner] = *delta.IsOwner
	}
	mergeString(merged, FactCity, delta.City)
	DeriveOwnerRole(merged)
	return merged
}

func mergeString(merged Facts, name string, value *string) {
	if value == nil {
		return
	}
	if clean := Sanitize(*value); clean != "" {
		merged[name] = clean
	}
}

// mergeRole: «собственник» поверх подтверждённого владения ничего не уточняет
// и затёр бы настоящую должность лида.
func mergeRole(merged Facts, value *string, ownerDelta *bool) {
	if value == nil {
		return
	}
	clean := Sanitize(*value)
	if clean == "" {
		return
	}
	ownerConfirmed, _ := merged[FactIsOwner].(bool)
	ownerDenied := ownerDelta != nil && !*ownerDelta
	if ownerConfirmed && !ownerDenied && NormalizeText(clean) == roleOwner {
		return
	}
	merged[FactRole] = clean
}

// Differ - ход изменил факты лида. Вердикт ниши из сравнения исключён: его
// пишет код, а не лид.
func Differ(base, merged Facts) bool {
	baseline := dialogFacts(base)
	DeriveOwnerRole(baseline)
	return !reflect.DeepEqual(dialogFacts(merged), baseline)
}

func dialogFacts(source Facts) Facts {
	facts := source.Known()
	for _, key := range []string{factNiche, factNicheOff, factNicheOf} {
		delete(facts, key)
	}
	return facts
}

// Validate проверяет значения до применения снимка: длину и формат выручки.
func Validate(facts Facts) error {
	for name, value := range facts {
		if text, ok := value.(string); ok && utf8.RuneCountInString(text) > maxFactChars {
			return fmt.Errorf("fact %s is too long", name)
		}
	}
	revenue, ok := facts[FactRevenue].(string)
	if !ok {
		return nil
	}
	lower, upper, isRange := strings.Cut(revenue, "-")
	if !isRange {
		if _, err := parseAmount(revenue); err != nil {
			return errors.New("monthly_revenue must be an integer or range")
		}
		return nil
	}
	lowerValue, lowerErr := parseAmount(lower)
	upperValue, upperErr := parseAmount(upper)
	if lowerErr != nil || upperErr != nil || lowerValue > upperValue {
		return errors.New("monthly_revenue must be an ordered integer range")
	}
	return nil
}

// revenueBounds - нижняя и верхняя граница названной выручки.
func revenueBounds(facts Facts) (lower, upper int64, ok bool) {
	revenue := facts.Value(FactRevenue)
	if revenue == "" {
		return 0, 0, false
	}
	low, high, isRange := strings.Cut(revenue, "-")
	if !isRange {
		high = low
	}
	lowValue, lowErr := parseAmount(strings.TrimSpace(low))
	highValue, highErr := parseAmount(strings.TrimSpace(high))
	if lowErr != nil || highErr != nil {
		return 0, 0, false
	}
	return lowValue, highValue, true
}

func parseAmount(value string) (int64, error) {
	if value == "" {
		return 0, errors.New("empty amount")
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return 0, errors.New("non-decimal character")
		}
	}
	return strconv.ParseInt(value, 10, 64)
}

// Sanitize приводит значение факта к безопасной строке: факты - данные, а не
// инструкции. Невидимые символы удаляются, переносы схлопываются в пробел.
func Sanitize(value string) string {
	var builder strings.Builder
	builder.Grow(len(value))
	for _, symbol := range value {
		switch {
		case symbol == '\n' || symbol == '\r' || symbol == '\t' || unicode.In(symbol, unicode.Zl, unicode.Zp):
			builder.WriteRune(' ')
		case unicode.IsControl(symbol) || unicode.In(symbol, unicode.Cf):
			continue
		default:
			builder.WriteRune(symbol)
		}
	}
	return strings.TrimSpace(builder.String())
}

// NormalizeText - нижний регистр и одиночные пробелы: форма для сравнения.
func NormalizeText(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}
