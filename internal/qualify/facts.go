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

// Facts - the snapshot of case facts. The keys of the five lead facts are fixed by the code, plus
// three service keys of the niche verdict (niche, niche_off, niche_of).
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

// RequiredFacts - what must be collected before the booking gate.
var RequiredFacts = []string{FactService, FactRevenue, FactRole, FactIsOwner, FactCity}

// maxFactChars - the ceiling of a value's length: a fact goes into the trusted context of
// the model, and long text there would be an arbitrary payload.
const maxFactChars = 256

// roleOwner - the role derived from confirmed ownership.
const roleOwner = "собственник"

// FactDelta - an untrusted facts delta from save_facts. nil means leave the field alone.
type FactDelta struct {
	Service *string `json:"service"`
	Revenue *string `json:"monthly_revenue"`
	Role    *string `json:"role"`
	IsOwner *bool   `json:"is_owner"`
	City    *string `json:"city"`
}

// Names - the delta field names without values: for the log.
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

// Value - the string value of a fact; empty if there is no fact or it is not a string.
func (f Facts) Value(key string) string {
	value, _ := f[key].(string)
	return strings.TrimSpace(value)
}

// Known copies only non-empty facts.
func (f Facts) Known() Facts {
	known := make(Facts, len(f))
	for name, value := range f {
		if !isEmptyFact(value) {
			known[name] = value
		}
	}
	return known
}

// Missing - the required facts absent from the snapshot.
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

// DeriveOwnerRole: a confirmed owner without a named role gets the role
// "owner". There is no reverse inference: the role "director" does not make the lead
// an owner, only their direct answer does.
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

// Merge applies the delta to the trusted snapshot and returns a new snapshot.
// The lead's correction beats the previous value: the gate is recomputed after it.
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

// mergeRole: "owner" on top of confirmed ownership clarifies nothing
// and would erase the lead's real job title.
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

// Differ - the turn changed the lead's facts. The niche verdict is excluded from the comparison: it
// is written by the code, not by the lead.
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

// Validate checks the values before the snapshot is applied: length and revenue format.
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

// revenueBounds - the lower and upper bound of the named revenue.
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

// Sanitize brings a fact value to a safe string: facts are data, not
// instructions. Invisible characters are removed, line breaks collapse into a space.
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

// NormalizeText - lower case and single spaces: the form for comparison.
func NormalizeText(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}
