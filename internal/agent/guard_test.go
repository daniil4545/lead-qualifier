package agent

import (
	"slices"
	"testing"

	"github.com/daniil4545/lead-qualifier/internal/config"
	"github.com/daniil4545/lead-qualifier/internal/knowledge"
	"github.com/daniil4545/lead-qualifier/internal/qualify"
)

func testGuard() Guard {
	catalog := config.Catalog{
		Tariffs: []config.Tariff{
			{Name: "Малый", Threshold: 1_000_000, Price: 40_000},
			{Name: "Средний", Threshold: 5_000_000, Price: 80_000},
		},
		Links: []string{"https://service.example"},
	}
	rules := qualify.NewRules(catalog)
	return NewGuard([]string{"Громов", "Лапина", "Сервис Маяк"}, knowledge.LinkWhitelist(catalog), rules.IsCatalogPrice)
}

func categories(violations []Violation) []string {
	var names []string
	for _, v := range violations {
		names = append(names, v.Category)
	}
	return names
}

func TestGuardCheck_Cuts(t *testing.T) {
	t.Parallel()
	guard := testGuard()
	cases := []struct {
		name        string
		text        string
		tariffPrice int64
		want        string
	}{
		{"sum with unknown tariff", "Стоимость поддержки 40 000 ₽ в месяц.", 0, "price"},
		{"bare sum with unknown tariff", "Цена будет около 55000.", 0, "price"},
		{"other catalog price", "Тариф стоит 80 000 ₽ в месяц.", 40_000, "price"},
		{"non-catalog sum with known tariff", "Оплата от 70 тысяч рублей.", 40_000, "price"},
		{"stop name dative", "Передам ваш вопрос Громову.", 0, "name"},
		{"stop name instrumental", "Созвонитесь с Громовым.", 0, "name"},
		{"female stop name genitive", "Спросите у Лапиной.", 0, "name"},
		{"stop name translit", "Contact Gromov directly.", 0, "name"},
		{"stop phrase", "Раньше клиентов вёл Сервис Маяк.", 0, "name"},
		{"phone", "Звоните +7 913 123-45-67.", 0, "contact"},
		{"email", "Пишите на sales@service.example.", 0, "contact"},
		{"link outside catalog", "Подробнее на https://other.example/page.", 0, "link"},
		{"acceptance threshold", "Мы берём компании с выручкой от 1 млн в месяц.", 0, "internal_rule"},
		{"service mechanics", "Все данные собраны, передаю дальше.", 0, "internal_rule"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := categories(guard.Check(tc.text, tc.tariffPrice, ""))
			if !slices.Contains(got, tc.want) {
				t.Fatalf("Check(%q, tariff=%d) = %v, want %q", tc.text, tc.tariffPrice, got, tc.want)
			}
		})
	}
}

func TestGuardCheck_Allows(t *testing.T) {
	t.Parallel()
	guard := testGuard()
	cases := []struct {
		name        string
		text        string
		tariffPrice int64
		leadName    string
	}{
		{"own tariff price", "Стоимость вашего тарифа 40 000 ₽ в месяц.", 40_000, ""},
		{"catalog link with path", "Примеры работ: https://service.example/cases.", 0, ""},
		{"stop name equal to lead name", "Громов, спасибо за ответ.", 0, "Громов"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := guard.Check(tc.text, tc.tariffPrice, tc.leadName); len(got) != 0 {
				t.Fatalf("Check(%q, tariff=%d, lead=%q) = %v, want none", tc.text, tc.tariffPrice, tc.leadName, categories(got))
			}
		})
	}
}
