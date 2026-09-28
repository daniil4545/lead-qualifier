package agent

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/daniil4545/lead-qualifier/internal/config"
	"github.com/daniil4545/lead-qualifier/internal/knowledge"
	"github.com/daniil4545/lead-qualifier/internal/qualify"
)

// Prompts - system-промпты трёх ролей: агент диалога, критик и классификатор
// ниши. Имя компании, персона и её род подставляются из каталога, чтобы в
// тексте промптов не было литералов клиента.
type Prompts struct {
	System string
	Critic string
	Niche  string
}

// LoadPrompts читает dir/prompts и собирает system prompt агента вместе с
// оглавлением базы знаний.
func LoadPrompts(dir string, catalog config.Catalog, kb knowledge.Set) (Prompts, error) {
	return LoadPromptsFrom(filepath.Join(dir, "prompts"), catalog, kb)
}

// LoadPromptsFrom - то же из произвольной папки промптов: prompteval так
// сравнивает кандидата с промптами каталога на той же базе знаний.
func LoadPromptsFrom(promptsDir string, catalog config.Catalog, kb knowledge.Set) (Prompts, error) {
	gender := "мужском"
	if catalog.Persona.Gender == config.GenderFemale {
		gender = "женском"
	}
	render := strings.NewReplacer(
		"{company}", catalog.Company,
		"{persona}", catalog.Persona.Name,
		"{gender}", gender,
		"{meeting_minutes}", fmt.Sprint(catalog.MeetingMinutes),
	)
	read := func(name string) (string, error) {
		data, err := os.ReadFile(filepath.Join(promptsDir, name))
		if err != nil {
			return "", fmt.Errorf("read prompt %s: %w", name, err)
		}
		return strings.TrimSpace(render.Replace(string(data))), nil
	}
	var prompts Prompts
	var err error
	if prompts.System, err = read("agent.md"); err != nil {
		return Prompts{}, err
	}
	prompts.System += "\n\n" + strings.TrimSpace(kb.Index)
	if prompts.Critic, err = read("critic.md"); err != nil {
		return Prompts{}, err
	}
	if prompts.Niche, err = read("niche.md"); err != nil {
		return Prompts{}, err
	}
	return prompts, nil
}

// FromCatalog собирает агента каталога: база знаний, стоп-имена, промпты,
// правила и guard. База проверяется до старта: всё, что в ней лежит, модель
// вправе повторить лиду. promptsDir пустой - промпты каталога.
func FromCatalog(catalogDir, promptsDir string, cfg config.OpenRouter, catalog config.Catalog,
	httpClient *http.Client) (*Client, error) {
	kb, err := knowledge.Load(catalogDir)
	if err != nil {
		return nil, err
	}
	stopNames, err := knowledge.LoadStopNames(catalogDir)
	if err != nil {
		return nil, err
	}
	if problems := knowledge.Lint(kb, catalog, stopNames); len(problems) > 0 {
		return nil, fmt.Errorf("knowledge base is not lead-safe: %s", strings.Join(problems, "; "))
	}
	if promptsDir == "" {
		promptsDir = filepath.Join(catalogDir, "prompts")
	}
	prompts, err := LoadPromptsFrom(promptsDir, catalog, kb)
	if err != nil {
		return nil, err
	}
	rules := qualify.NewRules(catalog)
	guard := NewGuard(stopNames, knowledge.LinkWhitelist(catalog), rules.IsCatalogPrice)
	return New(cfg, catalog, prompts, kb, guard, rules, httpClient), nil
}
