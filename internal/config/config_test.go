package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Старт проверяет: порог сегмента не ниже минимального порога тарифов,
// иначе гейт позвал бы на встречу лида, которому нечего продать.
func TestValidate_SegmentThresholdBelowTariffs(t *testing.T) {
	t.Parallel()
	catalog, err := LoadCatalog("../../data/example")
	if err != nil {
		t.Fatalf("load example catalog: %v", err)
	}
	if err := catalog.Validate(); err != nil {
		t.Fatalf("example catalog: Validate = %v, want nil", err)
	}
	catalog.Segments = append(catalog.Segments[:0:0], catalog.Segments...)
	catalog.Segments[0].Threshold = catalog.MinTariffThreshold() - 1
	if err := catalog.Validate(); err == nil {
		t.Fatalf("segment threshold %d below min tariff %d: Validate = nil, want error",
			catalog.Segments[0].Threshold, catalog.MinTariffThreshold())
	}
}

// Встреча в каталоге от 15 минут: каталог с 14 не загружается, с 15 - да.
func TestLoadCatalog_MeetingMinutesFloor(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		minutes int
		wantErr bool
	}{{14, true}, {15, false}} {
		dir := t.TempDir()
		for _, name := range []string{"config.json", "texts.json"} {
			data, err := os.ReadFile(filepath.Join("../../data/example", name))
			if err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
			if name == "config.json" {
				var raw map[string]any
				if err := json.Unmarshal(data, &raw); err != nil {
					t.Fatalf("parse config.json: %v", err)
				}
				raw["meeting_minutes"] = tc.minutes
				if data, err = json.Marshal(raw); err != nil {
					t.Fatalf("encode config.json: %v", err)
				}
			}
			if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
				t.Fatalf("write %s: %v", name, err)
			}
		}
		if _, err := LoadCatalog(dir); (err != nil) != tc.wantErr {
			t.Errorf("LoadCatalog(meeting_minutes=%d) error = %v, want error %v", tc.minutes, err, tc.wantErr)
		}
	}
}

// Модель по умолчанию из каталога, окружение её переопределяет; ключ и адрес
// модели - только из окружения. Не параллельный: меняет окружение процесса.
func TestLoadOpenRouter(t *testing.T) {
	catalog, err := LoadCatalog("../../data/example")
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	t.Setenv("OPENROUTER_API_KEY", "test-key")
	t.Setenv("OPENROUTER_URL", "http://127.0.0.1:1/chat")
	t.Setenv("OPENROUTER_MODEL", "")
	cfg, err := LoadOpenRouter(catalog)
	if err != nil {
		t.Fatalf("LoadOpenRouter: %v", err)
	}
	if cfg.APIKey != "test-key" || cfg.URL != "http://127.0.0.1:1/chat" || cfg.Model != catalog.Model {
		t.Errorf("cfg key %q, url %q, model %q; want key and url from env, model %q", cfg.APIKey, cfg.URL, cfg.Model, catalog.Model)
	}
	t.Setenv("OPENROUTER_MODEL", "example/override")
	if cfg, err = LoadOpenRouter(catalog); err != nil || cfg.Model != "example/override" {
		t.Errorf("model with env override = %q (err %v), want example/override", cfg.Model, err)
	}
}
