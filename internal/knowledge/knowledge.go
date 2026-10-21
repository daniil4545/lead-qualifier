// Package knowledge - the catalog knowledge base for search_kb and its check before
// start: anything in the base the model may repeat to the lead verbatim.
package knowledge

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/daniil4545/lead-qualifier/internal/config"
)

// IndexFile - the base table of contents: goes into the system prompt, not search_kb.
const IndexFile = "index.md"

// PreloadFile - the frequent questions section: if present, the turn gets it right away,
// without a search_kb call. Otherwise the critic has nothing to confirm an answer to a typical question.
const PreloadFile = "faq.md"

// Set - the knowledge base: the table of contents and section bodies by file name. The keys of Files
// are the search_kb whitelist.
type Set struct {
	Index string
	Files map[string]string
}

// Load reads dir/knowledge. Only .md is allowed in the base: a foreign file would go
// into search bypassing the checks.
func Load(dir string) (Set, error) {
	base := filepath.Join(dir, "knowledge")
	entries, err := os.ReadDir(base)
	if err != nil {
		return Set{}, fmt.Errorf("read knowledge dir: %w", err)
	}
	set := Set{Files: map[string]string{}}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".md") {
			return Set{}, fmt.Errorf("knowledge: only .md files are allowed, found %s", entry.Name())
		}
		data, err := os.ReadFile(filepath.Join(base, entry.Name())) //nolint:gosec // G304: a file of the operator's catalog directory
		if err != nil {
			return Set{}, fmt.Errorf("read knowledge file %s: %w", entry.Name(), err)
		}
		if entry.Name() == IndexFile {
			set.Index = string(data)
			continue
		}
		set.Files[entry.Name()] = string(data)
	}
	if set.Index == "" {
		return Set{}, fmt.Errorf("knowledge: %s is required", IndexFile)
	}
	return set, nil
}

// LoadStopNames reads the dictionary of third-party names and brands: a line is an entry,
// # is a comment.
func LoadStopNames(dir string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "stop_names.txt")) //nolint:gosec // G304: a file of the operator's catalog directory
	if err != nil {
		return nil, fmt.Errorf("read stop names: %w", err)
	}
	var names []string
	for _, line := range strings.Split(string(data), "\n") {
		entry := strings.TrimSpace(line)
		if entry == "" || strings.HasPrefix(entry, "#") {
			continue
		}
		names = append(names, entry)
	}
	return names, nil
}

// LinkPattern - links with a scheme, t.me, @handles and bare domains.
var LinkPattern = regexp.MustCompile(`(?i)https?://\S+|t\.me/\S+|@\w+|[a-zа-я0-9-]+(?:\.[a-zа-я0-9-]+)*\.(?:[a-z]{2,}|рф)(?:/\S*)?`)

// LinkKey normalizes a link to a comparison key: @handle and t.me to "t.me/<channel>",
// anything else to the host without scheme and path. The official domain passes with any path.
func LinkKey(token string) string {
	t := strings.ToLower(strings.Trim(token, ` .,;:!?()[]{}<>"'`+"«»"))
	if strings.HasPrefix(t, "@") {
		handle := strings.Trim(t[1:], "/")
		if handle == "" {
			return ""
		}
		return "t.me/" + handle
	}
	for _, prefix := range []string{"https://", "http://", "//"} {
		t = strings.TrimPrefix(t, prefix)
	}
	host, path := t, ""
	if index := strings.IndexByte(t, '/'); index >= 0 {
		host, path = t[:index], strings.Trim(t[index+1:], "/")
	}
	host = strings.TrimPrefix(host, "www.")
	if host == "t.me" || host == "telegram.me" {
		segment, _, _ := strings.Cut(path, "/")
		if segment == "" {
			return "t.me"
		}
		return "t.me/" + segment
	}
	return host
}

// LinkWhitelist - the catalog link keys: only these may be repeated by the bot.
func LinkWhitelist(catalog config.Catalog) map[string]bool {
	whitelist := make(map[string]bool, len(catalog.Links))
	for _, link := range catalog.Links {
		if key := LinkKey(link); key != "" {
			whitelist[key] = true
		}
	}
	return whitelist
}

var (
	lintMoney   = regexp.MustCompile(`(?i)(\d[\d\s\x{00A0}]*)(₽|рубл|руб|р\.|тысяч|тыс|млн|миллион|к(?:[^\p{L}]|$))`)
	lintPhone   = regexp.MustCompile(`\+?\d[\d\-\s().]{9,}\d`)
	lintEmail   = regexp.MustCompile(`\S+@\S+\.\S+`)
	lintIndexed = regexp.MustCompile(`(?m)^-\s+(\S+\.md)`)
)

// Lint checks that the base can be shown to a lead: amounts are only catalog tariff prices,
// links are only catalog links, no phones, e-mail or stop names,
// the table of contents lists exactly the existing sections. An empty answer means the base is clean.
func Lint(set Set, catalog config.Catalog, stopNames []string) []string {
	var problems []string
	prices := map[string]bool{}
	for _, tariff := range catalog.Tariffs {
		prices[fmt.Sprint(tariff.Price)] = true
	}
	whitelist := LinkWhitelist(catalog)
	files := map[string]string{IndexFile: set.Index}
	for name, content := range set.Files {
		files[name] = content
	}
	for name, content := range files {
		problems = append(problems, lintFile(name, content, prices, whitelist, stopNames)...)
	}
	listed := map[string]bool{}
	for _, match := range lintIndexed.FindAllStringSubmatch(set.Index, -1) {
		listed[match[1]] = true
		if _, ok := set.Files[match[1]]; !ok {
			problems = append(problems, fmt.Sprintf("%s lists missing file %s", IndexFile, match[1]))
		}
	}
	for name := range set.Files {
		if !listed[name] {
			problems = append(problems, fmt.Sprintf("%s does not list %s", IndexFile, name))
		}
	}
	return problems
}

func lintFile(name, content string, prices, whitelist map[string]bool, stopNames []string) []string {
	var problems []string
	for _, match := range lintMoney.FindAllStringSubmatch(content, -1) {
		amount := digitsOnly(match[1])
		if strings.TrimSpace(match[2]) != "₽" || !prices[amount] {
			problems = append(problems, fmt.Sprintf("%s: amount %q is not a catalog price written as digits with ₽",
				name, strings.TrimSpace(match[0])))
		}
	}
	if match := lintPhone.FindString(content); match != "" {
		problems = append(problems, fmt.Sprintf("%s: phone-like text %q", name, strings.TrimSpace(match)))
	}
	if match := lintEmail.FindString(content); match != "" {
		problems = append(problems, fmt.Sprintf("%s: email-like text %q", name, match))
	}
	for _, token := range LinkPattern.FindAllString(content, -1) {
		// Section names of the base look like domains but are not links.
		key := LinkKey(token)
		if strings.HasSuffix(key, ".md") || whitelist[key] {
			continue
		}
		problems = append(problems, fmt.Sprintf("%s: link outside catalog %q", name, token))
	}
	lower := strings.ToLower(content)
	for _, stop := range stopNames {
		if strings.Contains(lower, strings.ToLower(stop)) {
			problems = append(problems, fmt.Sprintf("%s: stop name %q", name, stop))
		}
	}
	return problems
}

func digitsOnly(value string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, value)
}
