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

// Prompts - the system prompts of three roles: the dialog agent, the critic and the niche
// classifier. The company name, persona and its gender are substituted from the catalog so that
// no customer literals appear in the prompt text.
type Prompts struct {
	System string
	Critic string
	Niche  string
}

// LoadPrompts reads dir/prompts and builds the agent system prompt together with
// the knowledge base table of contents.
func LoadPrompts(dir string, catalog config.Catalog, kb knowledge.Set) (Prompts, error) {
	return LoadPromptsFrom(filepath.Join(dir, "prompts"), catalog, kb)
}

// LoadPromptsFrom - the same from an arbitrary prompts folder: prompteval uses it
// to compare a candidate with the catalog prompts on the same knowledge base.
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

// FromCatalog builds the catalog agent: knowledge base, stop names, prompts,
// rules and guard. The base is validated before start: anything in it the model
// may repeat to the lead. An empty promptsDir means the catalog prompts.
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
