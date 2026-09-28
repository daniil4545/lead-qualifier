package config

import (
	"go/scanner"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// moneyUnits - именованные множители единиц денег («тыс», «млн»): их
// объявления - не значения каталога, хотя число может совпасть с порогом.
var moneyUnits = map[string]bool{"unitThousand": true, "unitMillion": true}

// digitGroups - число внутри строки, в том числе с пробелами между разрядами.
var digitGroups = regexp.MustCompile(`\d(?:[\d_]|[ \x{00A0}]\d{3})*`)

// Критерий общности: значений каталога (пояс, цены, пороги, имена) в Go-коде
// вне тестов нет. Сравниваются литералы кода и числа внутри строк, а не комментарии.
func TestNoCatalogLiteralsInCode(t *testing.T) {
	t.Parallel()
	numbers := map[int64]string{}
	// Пояс из спеки собирается из частей, чтобы стоп-список по дереву был пуст.
	texts := map[string]string{"Europe" + "/" + "Moscow": "timezone"}
	for _, dir := range []string{"../../data/example", "../../testdata/catalog-alt"} {
		catalog, err := LoadCatalog(dir)
		if err != nil {
			t.Fatalf("load catalog %s: %v", dir, err)
		}
		for _, tariff := range catalog.Tariffs {
			numbers[tariff.Threshold] = "tariff threshold"
			numbers[tariff.Price] = "tariff price"
			texts[tariff.Name] = "tariff name"
		}
		for _, segment := range catalog.Segments {
			if segment.Threshold != 0 {
				numbers[segment.Threshold] = "segment threshold"
			}
		}
		for _, manager := range catalog.Managers {
			texts[manager.ID] = "manager id"
			texts[manager.Name] = "manager name"
		}
		for _, link := range catalog.Links {
			texts[link] = "link"
		}
		texts[catalog.Timezone] = "timezone"
		texts[catalog.Company] = "company"
		texts[catalog.Persona.Name] = "persona"
	}

	err := filepath.WalkDir("../..", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && (entry.Name() == ".git" || entry.Name() == "testdata") {
			return filepath.SkipDir
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanFile(t, path, source, numbers, texts)
		return nil
	})
	if err != nil {
		t.Fatalf("walk code: %v", err)
	}
}

func scanFile(t *testing.T, path string, source []byte, numbers map[int64]string, texts map[string]string) {
	t.Helper()
	fset := token.NewFileSet()
	var scan scanner.Scanner
	scan.Init(fset.AddFile(path, -1, len(source)), source, nil, 0)
	var name, previous string // два предыдущих токена: имя и «=» объявления
	for {
		pos, tok, lit := scan.Scan()
		if tok == token.EOF {
			return
		}
		var found []string
		switch tok {
		case token.INT:
			if previous != "=" || !moneyUnits[name] {
				found = append(found, lit)
			}
		case token.STRING:
			found = digitGroups.FindAllString(lit, -1)
			for text, kind := range texts {
				if strings.Contains(lit, text) {
					t.Errorf("%s: %s %q in code", fset.Position(pos), kind, text)
				}
			}
		}
		for _, number := range found {
			clean := strings.NewReplacer("_", "", " ", "", " ", "").Replace(number)
			value, err := strconv.ParseInt(clean, 0, 64)
			if kind, ok := numbers[value]; err == nil && ok {
				t.Errorf("%s: %s %s in code", fset.Position(pos), kind, number)
			}
		}
		current := lit
		if tok != token.IDENT && tok != token.INT && tok != token.STRING {
			current = tok.String()
		}
		name, previous = previous, current
	}
}
