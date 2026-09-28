// Package config читает окружение и каталог клиента и проверяет их при старте.
// Каталог - папка с config.json, texts.json, промптами и базой знаний: всё, что
// отличает одну инсталляцию от другой, живёт там, а не в коде.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config - окружение процесса плюс разобранный каталог.
type Config struct {
	DatabaseURL string
	CatalogDir  string
	OpenRouter  OpenRouter
	Catalog     Catalog
}

// OpenRouter - доступ к модели. Ключ приходит только из окружения, модель по
// умолчанию - из каталога, переменная окружения её переопределяет.
type OpenRouter struct {
	APIKey              string
	URL                 string
	Model               string
	CriticModel         string
	Timeout             time.Duration
	MaxCompletionTokens int
}

const (
	defaultOpenRouterURL = "https://openrouter.ai/api/v1/chat/completions"
	defaultCatalogDir    = "data/example"
	defaultTimeout       = 45 * time.Second
	defaultMaxTokens     = 1200

	minMeetingMinutes = 15
	maxMeetingMinutes = 24 * 60
)

// Load собирает конфигурацию из окружения и каталога. Ошибка любого шага -
// отказ старта: сервис с битым каталогом назвал бы лиду чужую цену или время.
func Load() (Config, error) {
	cfg := Config{
		DatabaseURL: strings.TrimSpace(os.Getenv("DATABASE_URL")),
		CatalogDir:  envDefault("CATALOG_DIR", defaultCatalogDir),
	}
	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("DATABASE_URL is required")
	}
	catalog, err := LoadCatalog(cfg.CatalogDir)
	if err != nil {
		return Config{}, err
	}
	cfg.Catalog = catalog
	if cfg.OpenRouter, err = LoadOpenRouter(catalog); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// LoadOpenRouter - доступ к модели из окружения; модель по умолчанию берётся
// из каталога. Нужен и сервису, и prompteval, которому база не нужна.
func LoadOpenRouter(catalog Catalog) (OpenRouter, error) {
	cfg := OpenRouter{
		APIKey:              strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY")),
		URL:                 envDefault("OPENROUTER_URL", defaultOpenRouterURL),
		Model:               envDefault("OPENROUTER_MODEL", catalog.Model),
		CriticModel:         envDefault("OPENROUTER_CRITIC_MODEL", catalog.CriticModel),
		Timeout:             defaultTimeout,
		MaxCompletionTokens: defaultMaxTokens,
	}
	if cfg.APIKey == "" {
		return OpenRouter{}, errors.New("OPENROUTER_API_KEY is required")
	}
	if cfg.Model == "" {
		return OpenRouter{}, errors.New("model is required: set it in config.json or OPENROUTER_MODEL")
	}
	if cfg.CriticModel == "" {
		cfg.CriticModel = cfg.Model
	}
	return cfg, nil
}

func envDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

// Catalog - описание клиента: компания, персона, расписание, тарифы, ниши,
// менеджеры и тексты. Location заполняется при загрузке из Timezone.
type Catalog struct {
	Company        string          `json:"company"`
	Persona        Persona         `json:"persona"`
	Timezone       string          `json:"timezone"`
	WorkHours      WorkHours       `json:"work_hours"`
	MeetingMinutes int             `json:"meeting_minutes"`
	Model          string          `json:"model"`
	CriticModel    string          `json:"critic_model"`
	Tariffs        []Tariff        `json:"tariffs"`
	Segments       []Segment       `json:"segments"`
	RefusalReasons []RefusalReason `json:"refusal_reasons"`
	Managers       []Manager       `json:"managers"`
	Links          []string        `json:"links"`

	Texts    Texts          `json:"-"`
	Location *time.Location `json:"-"`
}

// Persona - от чьего имени пишет бот. Род нужен промпту: формы глаголов о себе.
type Persona struct {
	Name   string `json:"name"`
	Gender string `json:"gender"`
}

const (
	GenderMale   = "male"
	GenderFemale = "female"
)

// Tariff - уровень цены по выручке лида. Threshold - нижняя граница выручки в
// рублях в месяц, включительно; Price - цена тарифа в рублях.
type Tariff struct {
	Name      string `json:"name"`
	Threshold int64  `json:"threshold"`
	Price     int64  `json:"price"`
}

// Segment - целевое направление лида. Threshold - минимальная выручка для
// встречи; ноль значит минимальный порог тарифов.
type Segment struct {
	Code      string `json:"code"`
	Name      string `json:"name"`
	Threshold int64  `json:"threshold"`
}

// RefusalReason - повод отказа по нише: код классификатора и имя для менеджера.
type RefusalReason struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

// Manager - кто проводит встречи. Пустой WorkHours - общие часы каталога.
type Manager struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Priority  int       `json:"priority"`
	WorkHours WorkHours `json:"work_hours"`
}

// WorkHours - рабочие окна по дням недели: {"mon": ["10:00", "18:00"]}. День
// без ключа нерабочий.
type WorkHours map[string][2]string

// WeekdayKeys - ключи WorkHours в порядке time.Weekday (Sunday = 0).
var WeekdayKeys = [7]string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

// Window - рабочее окно дня в часах и минутах местного времени каталога.
type Window struct {
	StartHour, StartMinute int
	EndHour, EndMinute     int
}

// ParseWindows разбирает WorkHours в окна по дням недели. Любая ошибка формата
// отбрасывает расписание целиком: частично прочитанное опаснее отсутствующего.
func ParseWindows(hours WorkHours) (map[time.Weekday]Window, error) {
	windows := make(map[time.Weekday]Window, len(hours))
	for key, pair := range hours {
		weekday := -1
		for index, name := range WeekdayKeys {
			if name == key {
				weekday = index
				break
			}
		}
		if weekday < 0 {
			return nil, fmt.Errorf("unknown weekday %q", key)
		}
		window, err := parseWindow(pair[0], pair[1])
		if err != nil {
			return nil, fmt.Errorf("weekday %s: %w", key, err)
		}
		windows[time.Weekday(weekday)] = window
	}
	return windows, nil
}

func parseWindow(start, end string) (Window, error) {
	var window Window
	if _, err := fmt.Sscanf(start, "%d:%d", &window.StartHour, &window.StartMinute); err != nil {
		return Window{}, fmt.Errorf("bad start %q", start)
	}
	if _, err := fmt.Sscanf(end, "%d:%d", &window.EndHour, &window.EndMinute); err != nil {
		return Window{}, fmt.Errorf("bad end %q", end)
	}
	startMinutes := window.StartHour*60 + window.StartMinute
	endMinutes := window.EndHour*60 + window.EndMinute
	if window.StartMinute > 59 || window.EndMinute > 59 || startMinutes >= endMinutes || endMinutes > 24*60 {
		return Window{}, fmt.Errorf("window %s-%s is not a valid day range", start, end)
	}
	return window, nil
}

// LoadCatalog читает config.json и texts.json каталога и проверяет инварианты.
func LoadCatalog(dir string) (Catalog, error) {
	var catalog Catalog
	if err := readJSON(filepath.Join(dir, "config.json"), &catalog); err != nil {
		return Catalog{}, err
	}
	if err := readJSON(filepath.Join(dir, "texts.json"), &catalog.Texts); err != nil {
		return Catalog{}, err
	}
	location, err := time.LoadLocation(catalog.Timezone)
	if err != nil {
		return Catalog{}, fmt.Errorf("catalog timezone %q: %w", catalog.Timezone, err)
	}
	catalog.Location = location
	if err := catalog.Validate(); err != nil {
		return Catalog{}, fmt.Errorf("catalog %s: %w", dir, err)
	}
	return catalog, nil
}

func readJSON(path string, target any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

// MinTariffThreshold - нижний порог тарифов: ниже него цены у лида нет.
func (c Catalog) MinTariffThreshold() int64 {
	lowest := int64(0)
	for index, tariff := range c.Tariffs {
		if index == 0 || tariff.Threshold < lowest {
			lowest = tariff.Threshold
		}
	}
	return lowest
}

// Validate проверяет инварианты каталога, которые код иначе принял бы молча.
func (c Catalog) Validate() error {
	checks := []func() error{c.validateBasics, c.validateTariffs, c.validateSegments,
		c.validateManagers, c.Texts.validate}
	for _, check := range checks {
		if err := check(); err != nil {
			return err
		}
	}
	return nil
}

func (c Catalog) validateBasics() error {
	if strings.TrimSpace(c.Company) == "" {
		return errors.New("company is required")
	}
	if strings.TrimSpace(c.Persona.Name) == "" {
		return errors.New("persona.name is required")
	}
	if c.Persona.Gender != GenderMale && c.Persona.Gender != GenderFemale {
		return fmt.Errorf("persona.gender must be %s or %s", GenderMale, GenderFemale)
	}
	// Длительность встречи - шаг сетки окон от начала рабочего дня. Снизу
	// граница: при минутной сетке в контекст модели ушли бы сотни окон.
	if c.MeetingMinutes < minMeetingMinutes || c.MeetingMinutes > maxMeetingMinutes {
		return fmt.Errorf("meeting_minutes must be between %d and %d, got %d",
			minMeetingMinutes, maxMeetingMinutes, c.MeetingMinutes)
	}
	if _, err := ParseWindows(c.WorkHours); err != nil {
		return fmt.Errorf("work_hours: %w", err)
	}
	for _, link := range c.Links {
		if strings.TrimSpace(link) == "" {
			return errors.New("links must not contain empty values")
		}
	}
	return nil
}

func (c Catalog) validateTariffs() error {
	if len(c.Tariffs) == 0 {
		return errors.New("at least one tariff is required")
	}
	seen := map[int64]bool{}
	for _, tariff := range c.Tariffs {
		if strings.TrimSpace(tariff.Name) == "" || tariff.Threshold <= 0 || tariff.Price <= 0 {
			return fmt.Errorf("tariff %q needs a name, positive threshold and price", tariff.Name)
		}
		if seen[tariff.Threshold] {
			return fmt.Errorf("two tariffs share threshold %d", tariff.Threshold)
		}
		seen[tariff.Threshold] = true
	}
	return nil
}

// validateSegments держит правило старта: порог сегмента не ниже минимального
// порога тарифов, иначе гейт позвал бы на встречу лида, которому нечего продать.
func (c Catalog) validateSegments() error {
	if len(c.Segments) == 0 {
		return errors.New("at least one segment is required")
	}
	minimum := c.MinTariffThreshold()
	codes := map[string]bool{}
	for _, segment := range c.Segments {
		if segment.Code == "" || segment.Name == "" {
			return errors.New("segment needs code and name")
		}
		if codes[segment.Code] {
			return fmt.Errorf("duplicate segment or reason code %q", segment.Code)
		}
		codes[segment.Code] = true
		if segment.Threshold != 0 && segment.Threshold < minimum {
			return fmt.Errorf("segment %q threshold %d is below minimal tariff threshold %d",
				segment.Code, segment.Threshold, minimum)
		}
	}
	for _, reason := range c.RefusalReasons {
		if reason.Code == "" || reason.Name == "" {
			return errors.New("refusal reason needs code and name")
		}
		if codes[reason.Code] {
			return fmt.Errorf("duplicate segment or reason code %q", reason.Code)
		}
		codes[reason.Code] = true
	}
	return nil
}

func (c Catalog) validateManagers() error {
	if len(c.Managers) == 0 {
		return errors.New("at least one manager is required")
	}
	ids := map[string]bool{}
	for _, manager := range c.Managers {
		if manager.ID == "" || manager.Name == "" {
			return errors.New("manager needs id and name")
		}
		if ids[manager.ID] {
			return fmt.Errorf("duplicate manager id %q", manager.ID)
		}
		ids[manager.ID] = true
		if _, err := ParseWindows(manager.WorkHours); err != nil {
			return fmt.Errorf("manager %s work_hours: %w", manager.ID, err)
		}
	}
	return nil
}
