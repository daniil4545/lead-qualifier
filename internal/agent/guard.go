package agent

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/daniil4545/lead-qualifier/internal/knowledge"
)

// Guard - deterministic filters for free model text. The ready texts
// of the code do not pass through it. A false positive costs a regeneration, a miss is
// a leak, so the filters are stricter than necessary.
type Guard struct {
	stopPatterns  []*regexp.Regexp
	linkWhitelist map[string]bool
	isPrice       func(int64) bool
}

// Violation - one violation. Quote goes only into the decision audit, not the log.
type Violation struct {
	Category string // price | name | contact | link | length | internal_rule | service_echo
	Quote    string
}

// maxOutboundChars - the length ceiling of a reply to the lead.
const maxOutboundChars = 4000

// NewGuard builds the filters from the stop dictionary, catalog links and tariff prices.
func NewGuard(stopNames []string, linkWhitelist map[string]bool, isPrice func(int64) bool) Guard {
	return Guard{
		stopPatterns:  buildStopPatterns(stopNames),
		linkWhitelist: linkWhitelist,
		isPrice:       isPrice,
	}
}

var (
	// A group of digits with an optional unit: a bare amount in a price context
	// is caught the same way as an amount with "₽".
	moneyPattern = regexp.MustCompile(`(?i)(\d[\d\s\x{00A0}]*)\s*(млн|миллион\p{L}*|тысяч\p{L}*|тыс\.?|т\.\s*р\.?|рубл\p{L}*|руб\.?|₽|rub\p{L}*|к|k|р\.)?`)
	// An amount in words: the bot states catalog prices in digits, so a spelled-out amount in a
	// price context is off-canon.
	spelledMoneyPattern = regexp.MustCompile(`(?i)\p{L}+\s+(тысяч\p{L}*|миллион\p{L}*)`)
	phonePattern        = regexp.MustCompile(`\+?\d[\d\s().\-]{7,}\d`)
	emailPattern        = regexp.MustCompile(`\S+@\S+\.\S+`)
)

// priceContextKeys next to an amount mean the bot is naming a price.
var priceContextKeys = []string{"цен", "стоит", "стоимост", "оплат", "тариф", "рассрочк", "прайс"}

// revenueContextKeys next to an amount mean a retelling of the lead's revenue, not a price.
var revenueContextKeys = []string{"выручк", "оборот", "зарабат", "доход"}

// Word boundary for Cyrillic: \b in RE2 works only for ASCII.
const (
	boundaryLeft  = `(?:^|[^\p{L}\p{N}_])`
	boundaryRight = `(?:[^\p{L}\p{N}_]|$)`
)

// Case endings of surnames on top of the stem, Cyrillic and transliteration.
const (
	declensionSuffixes      = `(?:ому|ами|ых|ом|ым|ой|ах|а|у|е|ю|ы)?`
	latinDeclensionSuffixes = `(?:omu|ami|akh|ykh|om|ym|oy|yu|ya|a|u|e|y)?`
)

var translitTable = map[rune]string{
	'а': "a", 'б': "b", 'в': "v", 'г': "g", 'д': "d", 'е': "e", 'ё': "e",
	'ж': "zh", 'з': "z", 'и': "i", 'й': "y", 'к': "k", 'л': "l", 'м': "m",
	'н': "n", 'о': "o", 'п': "p", 'р': "r", 'с': "s", 'т': "t", 'у': "u",
	'ф': "f", 'х': "kh", 'ц': "ts", 'ч': "ch", 'ш': "sh", 'щ': "shch",
	'ъ': "", 'ы': "y", 'ь': "", 'э': "e", 'ю': "yu", 'я': "ya",
}

// buildStopPatterns: a single Cyrillic word is a surname, caught with
// its cases and in transliteration; anything else is a whole phrase.
func buildStopPatterns(names []string) []*regexp.Regexp {
	var patterns []*regexp.Regexp
	for _, name := range names {
		entry := replaceYo(strings.TrimSpace(name))
		if entry == "" {
			continue
		}
		if !isSingleCyrillicWord(entry) {
			patterns = append(patterns, regexp.MustCompile(`(?i)`+boundaryLeft+`(`+literalPhrase(entry)+`)`+boundaryRight))
			continue
		}
		stem := surnameStem(entry)
		patterns = append(patterns,
			regexp.MustCompile(`(?i)`+boundaryLeft+`(`+regexp.QuoteMeta(stem)+declensionSuffixes+`)`+boundaryRight))
		if translit := transliterate(stem); translit != strings.ToLower(stem) {
			patterns = append(patterns,
				regexp.MustCompile(`(?i)`+boundaryLeft+`(`+regexp.QuoteMeta(translit)+latinDeclensionSuffixes+`)`+boundaryRight))
		}
	}
	return patterns
}

// Check checks a draft and returns all violations. tariffPrice is the price
// of the lead's tariff computed by the code; 0 means the tariff is unknown and any amount in a
// price context is forbidden. leadName is the lead's name: a stop surname matching it
// is not a violation, the bot may address the lead.
func (g Guard) Check(text string, tariffPrice int64, leadName string) []Violation {
	if strings.TrimSpace(text) == "" {
		return []Violation{{Category: "length"}}
	}
	var violations []Violation
	if utf8.RuneCountInString(text) > maxOutboundChars {
		violations = append(violations, Violation{Category: "length"})
	}
	nameText := replaceYo(text)
	for _, pattern := range g.stopPatterns {
		match := pattern.FindStringSubmatch(nameText)
		if match == nil || nameMatchesLead(match[1], leadName) {
			continue
		}
		violations = append(violations, Violation{Category: "name", Quote: strings.TrimSpace(match[1])})
	}
	violations = append(violations, g.priceViolations(text, tariffPrice)...)
	violations = append(violations, internalRuleViolations(text)...)
	violations = append(violations, contactViolations(text)...)
	violations = append(violations, g.linkViolations(text)...)
	return violations
}

func nameMatchesLead(quote, leadName string) bool {
	folded := strings.ToLower(replaceYo(quote))
	for _, token := range strings.Fields(strings.ToLower(replaceYo(leadName))) {
		if len([]rune(token)) >= 3 && (strings.Contains(folded, token) || strings.Contains(token, folded)) {
			return true
		}
	}
	return false
}

// priceViolations: in a price context only the price of the lead's tariff is legitimate.
// A retelling of the lead's revenue is not flagged, except a catalog price next to a price
// word: that is a named price, and the word "revenue" nearby does not hide it.
func (g Guard) priceViolations(text string, tariffPrice int64) []Violation {
	var violations []Violation
	for _, loc := range moneyPattern.FindAllStringSubmatchIndex(text, -1) {
		digits := digitsOnly(text[loc[2]:loc[3]])
		if digits == "" {
			continue
		}
		unit := ""
		if loc[4] >= 0 {
			unit = text[loc[4]:loc[5]]
		}
		value, ok := scaleAmount(digits, unit)
		if !ok {
			continue
		}
		window := strings.ToLower(contextWindow(text, loc[0], loc[1], 80))
		priceContext := containsAny(window, priceContextKeys)
		if containsAny(window, revenueContextKeys) && !(priceContext && g.isPrice(value)) {
			continue
		}
		if strings.TrimSpace(unit) == "" {
			// A bare amount counts only in a price context and when it does not look like a year.
			if !priceContext || len(digits) < 4 || (value >= 1900 && value <= 2100) {
				continue
			}
		} else if !priceContext && !unitIsRuble(unit) {
			continue
		}
		if tariffPrice > 0 && value == tariffPrice {
			continue
		}
		violations = append(violations, Violation{Category: "price", Quote: strings.TrimSpace(text[loc[0]:loc[1]])})
	}
	for _, loc := range spelledMoneyPattern.FindAllStringIndex(text, -1) {
		window := strings.ToLower(contextWindow(text, loc[0], loc[1], 80))
		if containsAny(window, revenueContextKeys) || !containsAny(window, priceContextKeys) {
			continue
		}
		violations = append(violations, Violation{Category: "price", Quote: strings.TrimSpace(text[loc[0]:loc[1]])})
	}
	return violations
}

// internalRulePatterns - the inner kitchen the lead must not hear:
// the revenue acceptance threshold and service mechanics. A combination is caught, not a single word.
var internalRulePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(бер[её]м|работаем|принимаем|подходят|берут)\p{L}*[^.!?]{0,60}(выручк|оборот)\p{L}*[^.!?]{0,20}от\s*\d`),
	regexp.MustCompile(`(?i)порог\p{L}*[^.!?]{0,40}(выручк|оборот)|(выручк|оборот)\p{L}*[^.!?]{0,40}порог`),
	regexp.MustCompile(`(?i)` + boundaryLeft + `(в|из)\s+(наш\p{L}*\s+)?баз\p{L}*\s+(нет|не\s|есть|данн|информац)`),
	regexp.MustCompile(`(?i)техническ\p{L}*\s+сбо\p{L}*`),
	regexp.MustCompile(`(?i)(все\s+)?данн\p{L}*\s+собра\p{L}*`),
}

func internalRuleViolations(text string) []Violation {
	var violations []Violation
	for _, pattern := range internalRulePatterns {
		if quote := pattern.FindString(text); quote != "" {
			violations = append(violations, Violation{Category: "internal_rule", Quote: strings.TrimSpace(quote)})
		}
	}
	return violations
}

// serviceEchoPatterns - the model answers a service remark instead of the lead.
// The remark sits in history with the user role, and the model takes it for a lead message.
var serviceEchoPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)не\s+по\s+адресу`),
	regexp.MustCompile(`(?i)адресован\p{L}*\s+не\s+мне|не\s+мне\s+адресован\p{L}*`),
	regexp.MustCompile(`(?i)служебн\p{L}*\s+сообщени\p{L}*`),
	regexp.MustCompile(`(?i)редактор\p{L}*[^.!?]{0,40}(отклон|замечани|перепис|перепиш)|(отклон|замечани|перепис|перепиш)\p{L}*[^.!?]{0,40}редактор`),
}

// serviceEchoViolations is checked only on a regenerated draft: the
// first one has no service remark in history yet.
func serviceEchoViolations(text string) []Violation {
	for _, pattern := range serviceEchoPatterns {
		if quote := pattern.FindString(text); quote != "" {
			return []Violation{{Category: "service_echo", Quote: strings.TrimSpace(quote)}}
		}
	}
	return nil
}

// contactViolations catches phones and e-mail. An amount does not count as a phone:
// it needs a phone sign and no currency unit after it.
func contactViolations(text string) []Violation {
	var violations []Violation
	for _, loc := range phonePattern.FindAllStringIndex(text, -1) {
		quote := text[loc[0]:loc[1]]
		if followedByMoneyUnit(text, loc[1]) || !phoneLike(quote) {
			continue
		}
		violations = append(violations, Violation{Category: "contact", Quote: strings.TrimSpace(quote)})
	}
	if quote := emailPattern.FindString(text); quote != "" {
		violations = append(violations, Violation{Category: "contact", Quote: strings.TrimSpace(quote)})
	}
	return violations
}

func (g Guard) linkViolations(text string) []Violation {
	var violations []Violation
	for _, token := range knowledge.LinkPattern.FindAllString(text, -1) {
		key := knowledge.LinkKey(token)
		if key == "" || g.linkWhitelist[key] {
			continue
		}
		violations = append(violations, Violation{Category: "link", Quote: strings.TrimSpace(token)})
	}
	return violations
}

// Multipliers of the currency units "thousand" and "million".
const (
	unitThousand = 1_000
	unitMillion  = 1_000_000
)

// scaleAmount converts an amount with a unit into whole rubles.
func scaleAmount(digits, unit string) (int64, bool) {
	base, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, false
	}
	u := strings.ToLower(strings.TrimSpace(unit))
	switch {
	case strings.HasPrefix(u, "млн") || strings.HasPrefix(u, "миллион"):
		return base * unitMillion, true
	case strings.HasPrefix(u, "тыс") || strings.HasPrefix(u, "т.") || u == "к" || u == "k":
		return base * unitThousand, true
	default:
		return base, true
	}
}

func unitIsRuble(unit string) bool {
	u := strings.ToLower(strings.TrimSpace(unit))
	return strings.Contains(u, "₽") || strings.HasPrefix(u, "руб") || strings.HasPrefix(u, "rub") ||
		u == "р." || strings.HasPrefix(u, "т.")
}

func followedByMoneyUnit(text string, from int) bool {
	rest := strings.ToLower(strings.TrimLeft(text[from:], " \t"))
	for _, unit := range []string{"₽", "руб", "тыс", "млн", "миллион"} {
		if strings.HasPrefix(rest, unit) {
			return true
		}
	}
	return false
}

// phoneLike: 10-15 digits and a phone sign - a leading +, 7 or 8, parentheses
// or a hyphen. A retold amount ("10 000 000") has none of these.
func phoneLike(quote string) bool {
	digits := digitsOnly(quote)
	if len(digits) < 10 || len(digits) > 15 {
		return false
	}
	hasSeparator := strings.ContainsAny(quote, "()-")
	leading := strings.HasPrefix(strings.TrimSpace(quote), "+") || digits[0] == '7' || digits[0] == '8'
	return hasSeparator || leading
}

// contextWindow - the text around a match, trimmed by runes.
func contextWindow(text string, start, end, radius int) string {
	low := max(start-radius, 0)
	for low > 0 && !utf8.RuneStart(text[low]) {
		low--
	}
	high := min(end+radius, len(text))
	for high < len(text) && !utf8.RuneStart(text[high]) {
		high++
	}
	return text[low:high]
}

func containsAny(haystack string, keys []string) bool {
	for _, key := range keys {
		if strings.Contains(haystack, key) {
			return true
		}
	}
	return false
}

func isSingleCyrillicWord(entry string) bool {
	if strings.ContainsAny(entry, " \t") {
		return false
	}
	for _, symbol := range entry {
		if symbol != '-' && !unicode.Is(unicode.Cyrillic, symbol) {
			return false
		}
	}
	return true
}

// surnameStem: for feminine surnames in -a/-ya and adjectives in -iy/-yy the ending
// is dropped so that cases are caught from the stem.
func surnameStem(word string) string {
	runes := []rune(word)
	n := len(runes)
	last := unicode.ToLower(runes[n-1])
	if last == 'а' || last == 'я' {
		return string(runes[:n-1])
	}
	if n >= 2 && last == 'й' {
		if previous := unicode.ToLower(runes[n-2]); previous == 'и' || previous == 'ы' {
			return string(runes[:n-2])
		}
	}
	return word
}

func transliterate(word string) string {
	var builder strings.Builder
	for _, symbol := range strings.ToLower(word) {
		if latin, ok := translitTable[symbol]; ok {
			builder.WriteString(latin)
			continue
		}
		builder.WriteRune(symbol)
	}
	return builder.String()
}

func literalPhrase(entry string) string {
	words := strings.Fields(entry)
	for index, word := range words {
		words[index] = regexp.QuoteMeta(word)
	}
	return strings.Join(words, `\s+`)
}

func digitsOnly(value string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, value)
}

func replaceYo(value string) string {
	return strings.NewReplacer("ё", "е", "Ё", "Е").Replace(value)
}
