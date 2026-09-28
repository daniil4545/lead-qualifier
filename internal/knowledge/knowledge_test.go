package knowledge

import (
	"testing"

	"github.com/daniil4545/lead-qualifier/internal/config"
)

func TestLint(t *testing.T) {
	t.Parallel()
	catalog := config.Catalog{
		Tariffs: []config.Tariff{{Name: "Малый", Threshold: 1_000_000, Price: 40_000}},
		Links:   []string{"https://service.example"},
	}
	stopNames := []string{"Громов"}
	const index = "- faq.md - частые вопросы\n"
	const clean = "Тариф «Малый» стоит 40 000 ₽ в месяц. Кейсы: https://service.example/cases."
	cases := []struct {
		name      string
		index     string
		faq       string
		wantClean bool
	}{
		{"catalog price and link", index, clean, true},
		{"non-catalog sum", index, "Настройка стоит 55 000 ₽.", false},
		{"foreign link", index, "Подробности на https://other.example.", false},
		{"phone", index, "Звоните +7 913 123-45-67.", false},
		{"email", index, "Пишите sales@service.example.", false},
		{"stop name", index, "Раньше у нас работал Громов.", false},
		{"index names missing section", index + "- tarify.md - тарифы\n", clean, false},
		{"section missing from index", "", clean, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			set := Set{Index: tc.index, Files: map[string]string{"faq.md": tc.faq}}
			problems := Lint(set, catalog, stopNames)
			if (len(problems) == 0) != tc.wantClean {
				t.Fatalf("Lint(index=%q, faq=%q) = %v, want clean %v", tc.index, tc.faq, problems, tc.wantClean)
			}
		})
	}
}
