package agent

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/daniil4545/lead-qualifier/internal/qualify"
)

// Claim grounding. A model reply can claim something about state that only the code changes:
// "booked you for Thursday", "moved the meeting", "the manager is waiting", "I will write on
// the 20th". If the turn outcome or the database does not back the claim, the lead must not
// read it. The registry maps a claim class to a lexicon and a grounding predicate:
//
//	C1 booking created      - a live booking whose time matches every time token of the sentence
//	C2 moved or cancelled   - never: only the code moves or cancels, and it answers with its own text
//	C3 manager is on the line - never: a manager gets the case only through a handoff outcome
//	C8 dated return         - never: the service has no scheduler and no follow-ups
//
// A draft with an ungrounded claim goes back for one regeneration with a remark; if the claim
// survives, the sentence is cut (the same last resort as in the guard: a ragged remainder reads
// worse than a lost phrase). The lexicon is Russian because the lead-facing text is Russian.

// claimMaxRegen - regenerations per turn caused by claims.
const claimMaxRegen = 1

// claimGrounding - what backs the claims at the moment of the candidate.
type claimGrounding struct {
	Live     bool      // the lead has a live booking
	StartsAt time.Time // its start
	// Now is the turn moment: "today" and "tomorrow" are counted from it. The zero value matches
	// nothing.
	Now      time.Time
	Location *time.Location
}

// newClaimGrounding builds the grounding from the turn snapshot. A booking whose end has passed is
// not live: "booked you for Thursday" after it would announce a booking that does not exist.
func newClaimGrounding(input qualify.DecisionInput, location *time.Location) claimGrounding {
	grounding := claimGrounding{Now: input.Now, Location: location}
	if meeting := input.Meeting; meeting != nil && meeting.Status == qualify.MeetingScheduled &&
		(input.Now.IsZero() || meeting.End.After(input.Now)) {
		grounding.Live = true
		grounding.StartsAt = meeting.Start
	}
	return grounding
}

type claimRule struct {
	class string
	// find returns the start of the claim in a sentence or -1.
	find     func(sentence string) int
	grounded func(sentence string, g claimGrounding) bool
}

func never(string, claimGrounding) bool { return false }

var claimRegistry = []claimRule{
	{class: "C1", find: findBooked, grounded: bookedGrounded},
	{class: "C2", find: findMoved, grounded: never},
	{class: "C3", find: findManagerOnLine, grounded: never},
	{class: "C8", find: findDatedReturn, grounded: never},
}

// dropClaims removes sentences with an ungrounded claim and returns the rest with the class of
// every removed sentence. A question is not a claim; a negation or a condition within the same
// comma clause, up to three words before the verb, lets the sentence pass.
func dropClaims(text string, g claimGrounding) (string, []string) {
	if text == "" {
		return text, nil
	}
	var kept strings.Builder
	var classes []string
	for _, sentence := range splitSentences(text) {
		if class := ungroundedClaim(sentence, g); class != "" {
			classes = append(classes, class)
			continue
		}
		kept.WriteString(sentence)
	}
	return strings.TrimSpace(kept.String()), classes
}

func ungroundedClaim(sentence string, g claimGrounding) string {
	if strings.Contains(sentence, "?") {
		return ""
	}
	for _, rule := range claimRegistry {
		at := rule.find(sentence)
		if at < 0 || negatedBefore(sentence, at) || rule.grounded(sentence, g) {
			continue
		}
		return rule.class
	}
	return ""
}

var claimNegations = []string{"не", "ни", "ничего", "если", "пока", "вдруг"}

// negatedBefore: a negation or a condition stands within three words before the claim, inside its
// comma clause. "Не переживайте, записал..." is cut off by the comma.
func negatedBefore(sentence string, at int) bool {
	prefix := sentence[:at]
	if cut := strings.LastIndexAny(prefix, ",;:—"); cut >= 0 {
		prefix = prefix[cut+1:]
	}
	words := strings.FieldsFunc(strings.ToLower(prefix), func(r rune) bool { return !unicode.IsLetter(r) })
	if len(words) > 3 {
		words = words[len(words)-3:]
	}
	return slices.ContainsFunc(words, func(word string) bool { return slices.Contains(claimNegations, word) })
}

// firstIndex returns the start of the last capture group of the first match or -1.
func firstIndex(pattern *regexp.Regexp, sentence string) int {
	if match := pattern.FindStringSubmatchIndex(sentence); match != nil {
		return match[len(match)-2]
	}
	return -1
}

// splitSentences cuts text at .!? and a newline. A dot between digits (23.08) is not a sentence
// end, otherwise a numeric date would be torn in half.
func splitSentences(text string) []string {
	runes := []rune(text)
	var sentences []string
	start := 0
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if r != '.' && r != '!' && r != '?' && r != '\n' {
			continue
		}
		if r == '.' && i > 0 && i+1 < len(runes) && unicode.IsDigit(runes[i-1]) && unicode.IsDigit(runes[i+1]) {
			continue
		}
		for i < len(runes) && strings.ContainsRune(".!?\n", runes[i]) {
			i++
		}
		sentences = append(sentences, string(runes[start:i]))
		start = i
		i--
	}
	if start < len(runes) {
		sentences = append(sentences, string(runes[start:]))
	}
	return sentences
}

// --- C1: booking created ---

var (
	bookedVerb = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(запис(?:ал|ала|али)|забронировал(?:а|и)?|записан(?:а|ы|о)?|назначил(?:а|и)?|назначен(?:а|ы|о)?|ждём\s+вас|ждем\s+вас|жду\s+вас)(?:[^\p{L}]|$)`)
	bookedFor  = regexp.MustCompile(`(?i)вас\s+на(?:\s|$)|на\s+(?:нашу\s+|эту\s+)?встречу`)
)

// findBooked: a booking verb with a time token or "вас на" beside it. "Записала: Москва" and
// "Записала, SMM" stay facts.
func findBooked(sentence string) int {
	at := firstIndex(bookedVerb, sentence)
	if at < 0 {
		return -1
	}
	if len(claimTimeTokens(sentence)) == 0 && !bookedFor.MatchString(sentence) {
		return -1
	}
	return at
}

// bookedGrounded: a live booking exists and every time token of the sentence matches its weekday,
// hour, day of month, month or "today/tomorrow".
func bookedGrounded(sentence string, g claimGrounding) bool {
	if !g.Live || g.StartsAt.IsZero() || g.Location == nil {
		return false
	}
	local := g.StartsAt.In(g.Location)
	for _, token := range claimTimeTokens(sentence) {
		if !token.matches(local, g.Now, g.Location) {
			return false
		}
	}
	return true
}

// --- time tokens ---

type claimTokenKind int

const (
	tokenClock claimTokenKind = iota
	tokenWeekday
	tokenDayOfMonth
	tokenMonth
	tokenRelDay // a - shift from the turn day: today 0, tomorrow 1, the day after 2
	tokenHour   // a bare hour "в 12": compared with the round hour of the booking
)

type claimToken struct {
	kind   claimTokenKind
	a, b   int  // clock: hour, minutes; weekday, day, month: a
	dotted bool // the clock is written with a dot and may be a dd.mm date as well
}

func (t claimToken) matches(local, now time.Time, location *time.Location) bool {
	switch t.kind {
	case tokenClock:
		if t.a == local.Hour() && t.b == local.Minute() {
			return true
		}
		return t.dotted && t.a == local.Day() && t.b == int(local.Month())
	case tokenWeekday:
		return t.a == int(local.Weekday())
	case tokenDayOfMonth:
		return t.a == local.Day()
	case tokenMonth:
		return t.a == int(local.Month())
	case tokenHour:
		// An hour without minutes names a round hour: a booking at 12:30 is not confirmed by "в 12".
		return t.a == local.Hour() && local.Minute() == 0
	case tokenRelDay:
		if now.IsZero() {
			return false
		}
		year, month, day := qualify.StartOfDay(now, location).AddDate(0, 0, t.a).Date()
		localYear, localMonth, localDay := local.Date()
		return year == localYear && month == localMonth && day == localDay
	}
	return false
}

var (
	clockToken   = regexp.MustCompile(`^(\d{1,2})([:.])(\d{2})$`)
	dayOfMonthRe = regexp.MustCompile(`(?:^|\D)(\d{1,2})-го(?:[^\p{L}]|$)`)
	weekdayShort = map[string]int{"вс": 0, "пн": 1, "вт": 2, "ср": 3, "чт": 4, "пт": 5, "сб": 6}
	relativeDays = map[string]int{"сегодня": 0, "завтра": 1, "послезавтра": 2}
)

// weekdayForms and monthForms - whole words with case endings: a root with a word boundary, so
// that "субботник" and "Мартин" do not become a day and a month.
const (
	weekdayForms = `воскресень(?:е|я|ю|ем)|понедельник(?:а|у|е|ом)?|вторник(?:а|у|е|ом)?|сред(?:а|ы|у|е|ой)|` +
		`четверг(?:а|у|е|ом)?|пятниц(?:а|ы|у|е|ей)|суббот(?:а|ы|у|е|ой)`
	monthForms = `январ(?:ь|я|ю|е|ём)|феврал(?:ь|я|ю|е|ём)|март(?:а|у|е|ом)?|апрел(?:ь|я|ю|е|ём)|ма(?:й|я|ю|е|ем)|` +
		`июн(?:ь|я|ю|е|ем)|июл(?:ь|я|ю|е|ем)|август(?:а|у|е|ом)?|сентябр(?:ь|я|ю|е|ём)|октябр(?:ь|я|ю|е|ём)|` +
		`ноябр(?:ь|я|ю|е|ём)|декабр(?:ь|я|ю|е|ём)`
)

var (
	weekdayWordRe = regexp.MustCompile(`^(?:` + weekdayForms + `)$`)
	monthWordRe   = regexp.MustCompile(`^(?:` + monthForms + `)$`)
	// The order is the month number: the first root is January and so on.
	monthRoots   = []string{"январ", "феврал", "март", "апрел", "ма", "июн", "июл", "август", "сентябр", "октябр", "ноябр", "декабр"}
	weekdayRoots = []struct {
		root string
		day  int
	}{{"воскресень", 0}, {"понедельник", 1}, {"вторник", 2}, {"сред", 3}, {"четверг", 4}, {"пятниц", 5}, {"суббот", 6}}
)

func monthOf(word string) int {
	if !monthWordRe.MatchString(word) {
		return 0
	}
	for i, root := range monthRoots {
		if strings.HasPrefix(word, root) {
			return i + 1
		}
	}
	return 0
}

func weekdayOf(word string) (int, bool) {
	if day, ok := weekdayShort[word]; ok {
		return day, true
	}
	if !weekdayWordRe.MatchString(word) {
		return 0, false
	}
	for _, entry := range weekdayRoots {
		if strings.HasPrefix(word, entry.root) {
			return entry.day, true
		}
	}
	return 0, false
}

// bareHourRe: a preposition and a number up to three digits, so that "на 350" is not an hour.
// What follows the number is decided by bareHours.
var bareHourRe = regexp.MustCompile(`(?:^|[^\p{L}\d])(?:в|на|к)\s+(\d{1,3})`)

// hourFollowers - words after a bare hour that make it an hour and not a quantity: "в 12 часов",
// "на 12 по Москве". "На 15 минут" and "на 12 человек" are not among them.
var hourFollowers = []string{"час", "часа", "часов", "утра", "вечера", "дня", "ночи", "по", "мск", "и", "или"}

// bareHours returns the hours named without minutes: "в 12", "на 15 часов".
func bareHours(lower string) []int {
	var hours []int
	for _, match := range bareHourRe.FindAllStringSubmatchIndex(lower, -1) {
		digits := lower[match[2]:match[3]]
		hour, _ := strconv.Atoi(digits)
		if len(digits) > 2 || hour > 23 {
			continue
		}
		rest := []rune(lower[match[3]:])
		if len(rest) > 0 {
			next := rest[0]
			minutes := (next == ':' || next == '.') && len(rest) > 1 && unicode.IsDigit(rest[1])
			if unicode.IsDigit(next) || unicode.IsLetter(next) || next == '-' || minutes {
				continue
			}
		}
		if follower := wordAfterSpace(string(rest)); follower == "" || slices.Contains(hourFollowers, follower) {
			hours = append(hours, hour)
		}
	}
	return hours
}

// wordAfterSpace: the word right after a space; punctuation instead of it gives "".
func wordAfterSpace(rest string) string {
	trimmed := strings.TrimLeft(rest, " \t ")
	if len(trimmed) == len(rest) {
		return ""
	}
	end := strings.IndexFunc(trimmed, func(r rune) bool { return !unicode.IsLetter(r) })
	if end < 0 {
		return trimmed
	}
	return trimmed[:end]
}

// claimTimeTokens: "12:30", "12.30", a bare hour "в 12", "сегодня/завтра", a weekday (also "чт"),
// "20-го", a month.
func claimTimeTokens(sentence string) []claimToken {
	lower := strings.ToLower(sentence)
	var tokens []claimToken
	for _, match := range dayOfMonthRe.FindAllStringSubmatch(lower, -1) {
		day, _ := strconv.Atoi(match[1])
		tokens = append(tokens, claimToken{kind: tokenDayOfMonth, a: day})
	}
	lower = dayOfMonthRe.ReplaceAllString(lower, " ")
	for _, hour := range bareHours(lower) {
		tokens = append(tokens, claimToken{kind: tokenHour, a: hour})
	}
	for _, word := range strings.FieldsFunc(lower, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != ':' && r != '.'
	}) {
		word = strings.Trim(word, ".:")
		if match := clockToken.FindStringSubmatch(word); match != nil {
			hour, _ := strconv.Atoi(match[1])
			minute, _ := strconv.Atoi(match[3])
			tokens = append(tokens, claimToken{kind: tokenClock, a: hour, b: minute, dotted: match[2] == "."})
			continue
		}
		if shift, ok := relativeDays[word]; ok {
			tokens = append(tokens, claimToken{kind: tokenRelDay, a: shift})
			continue
		}
		if day, ok := weekdayOf(word); ok {
			tokens = append(tokens, claimToken{kind: tokenWeekday, a: day})
			continue
		}
		if month := monthOf(word); month > 0 {
			tokens = append(tokens, claimToken{kind: tokenMonth, a: month})
		}
	}
	return tokens
}

// --- C2: moved or cancelled ---

var movedVerb = regexp.MustCompile(
	`(?i)(?:^|[^\p{L}])(перен[её]с(?:ла|ли)?|отменил(?:а|и)?|перезаписал(?:а|и)?)(?:[^\p{L}]|$)`)

func findMoved(sentence string) int { return firstIndex(movedVerb, sentence) }

// --- C3: the manager is on the line ---

var managerOnLine = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(менеджер)\p{L}*[^.!?]{0,40}?(?:ждёт|ждет|ждут|ожида(?:ет|ют)|на\s+связи|напиш(?:ет|ут)|` +
		`свяж(?:ется|утся)|позвон(?:ит|ят)|выйд(?:ет|ут)\s+на\s+связь)`),
	regexp.MustCompile(`(?i)(свяж(?:ется|утся)|напиш(?:ет|ут)|позвон(?:ит|ят))\s+(?:с\s+)?вам`),
}

func findManagerOnLine(sentence string) int {
	for _, pattern := range managerOnLine {
		if at := firstIndex(pattern, sentence); at >= 0 {
			return at
		}
	}
	return -1
}

// --- C8: a dated return ---

var (
	returnVerb = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(вернус\p{L}*|вернём\p{L}*|вернемс\p{L}*|напиш(?:у|ем)|свяж(?:усь|емся))(?:[^\p{L}]|$)`)
	// Every root sits after a word boundary: "понимая" and "самая" do not contain "мая".
	returnDate = regexp.MustCompile(`(?i)(?:^|[^\p{L}\d])(?:\d{1,2}-?го|числа|завтра|послезавтра|` + monthForms + `|` +
		`(?:в|во|до|к)\s+(?:` + weekdayForms + `)|на\s+(?:следующей|этой)\s+неделе|` +
		`через\s+(?:\d+\s+|пару\s+|несколько\s+)?(?:дн\p{L}*|день|недел\p{L}*|месяц\p{L}*|час\p{L}*|минут\p{L}*))` +
		`(?:[^\p{L}\d]|$)`)
)

// findDatedReturn: "вернусь/напишу/свяжусь" with a deadline.
func findDatedReturn(sentence string) int {
	at := firstIndex(returnVerb, sentence)
	if at < 0 || !returnDate.MatchString(sentence) {
		return -1
	}
	return at
}

// --- the day the lead named, for windows built by the code ---

// leadDayRange returns the local day the lead named in words (a weekday, "завтра"): the nearest
// such date from today. A day inside a clause with a negation ("сегодня не смогу", "кроме
// вторника") is rejected and never chosen, even if another clause names it again. If the lead
// named nothing, rejected everything or named several different days, the bounds are zero: the
// code shows the nearest windows and does not guess a day.
func leadDayRange(leadText string, now time.Time, location *time.Location) (time.Time, time.Time) {
	today := qualify.StartOfDay(now, location)
	var chosen time.Time
	rejected := map[time.Time]bool{}
	var accepted [][]string
	for _, clause := range clauseSplit.Split(strings.ToLower(leadText), -1) {
		words := strings.FieldsFunc(clause, func(r rune) bool { return !unicode.IsLetter(r) })
		if !slices.ContainsFunc(words, func(word string) bool { return slices.Contains(dayRejections, word) }) {
			accepted = append(accepted, words)
			continue
		}
		for _, word := range words {
			if day, ok := leadWordDay(word, today); ok {
				rejected[day] = true
			}
		}
	}
	for _, words := range accepted {
		for _, word := range words {
			day, ok := leadWordDay(word, today)
			if !ok || rejected[day] {
				continue
			}
			if !chosen.IsZero() && !chosen.Equal(day) {
				return time.Time{}, time.Time{}
			}
			chosen = day
		}
	}
	return chosen, chosen
}

var (
	clauseSplit   = regexp.MustCompile(`[,;.!?—\n]|\s+(?:но|а)\s+`)
	dayRejections = []string{"не", "нет", "ни", "кроме", "невозможно"}
)

func leadWordDay(word string, today time.Time) (time.Time, bool) {
	if shift, ok := relativeDays[word]; ok {
		return today.AddDate(0, 0, shift), true
	}
	if weekday, ok := weekdayOf(word); ok {
		return today.AddDate(0, 0, (weekday-int(today.Weekday())+7)%7), true
	}
	return time.Time{}, false
}
