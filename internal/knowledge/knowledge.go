// Package knowledge - база знаний каталога для search_kb и её проверка перед
// стартом: всё, что лежит в базе, модель может повторить лиду дословно.
package knowledge

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/daniil4545/lead-qualifier/internal/config"
)

// IndexFile - оглавление базы: идёт в system prompt, а не в search_kb.
const IndexFile = "index.md"

// PreloadFile - раздел частых вопросов: если он есть, ход получает его сразу,
// без вызова search_kb. Иначе критику нечем подтвердить ответ на типовой вопрос.
const PreloadFile = "faq.md"

// Set - база знаний: оглавление и тела разделов по имени файла. Ключи Files и
// есть белый список search_kb.
type Set struct {
	Index string
	Files map[string]string
}

// Load читает dir/knowledge. В базе допустимы только .md: посторонний файл ушёл
// бы в поиск мимо проверок.
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
		data, err := os.ReadFile(filepath.Join(base, entry.Name()))
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

// LoadStopNames читает словарь имён и брендов третьих лиц: строка - запись,
// # - комментарий.
func LoadStopNames(dir string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "stop_names.txt"))
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

// LinkPattern - ссылки со схемой, t.me, @хендлы и голые домены.
var LinkPattern = regexp.MustCompile(`(?i)https?://\S+|t\.me/\S+|@\w+|[a-zа-я0-9-]+(?:\.[a-zа-я0-9-]+)*\.(?:[a-z]{2,}|рф)(?:/\S*)?`)

// LinkKey нормализует ссылку до ключа сверки: @хендл и t.me - к «t.me/<канал>»,
// прочее - к хосту без схемы и пути. Официальный домен с любым путём проходит.
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

// LinkWhitelist - ключи ссылок каталога: только их бот вправе повторить.
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

// Lint проверяет, что базу можно показывать лиду: суммы - только цены тарифов
// каталога, ссылки - только ссылки каталога, без телефонов, почты и стоп-имён,
// оглавление перечисляет ровно существующие разделы. Пустой ответ - база чиста.
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
		// Имена разделов базы похожи на домены, но ссылками не являются.
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
