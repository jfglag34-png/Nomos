package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestConformance — батарея ядра (см. conformance/README.md).
// CI запускает её как go test ./internal/core -run TestConformance.
func TestConformance(t *testing.T) {
	report := RunConformance("testdata/plugins")
	for _, c := range report.Checks {
		t.Logf("%s pass=%v %s", c.Name, c.Pass, c.Details)
		if !c.Pass {
			t.Errorf("conformance %s: %s", c.Name, c.Details)
		}
	}
	if !report.OK {
		t.Fatal("conformance failed")
	}
}

// Регресс: `wedra plugin test <плагин> --conformance` передавал каталог плагина
// как каталог фикстур. Фикстур там нет, load() возвращал nil, а fixtureRun
// отдавал провал с ПУСТЫМ именем — вызывающие считали это «проверки не было» и
// разыменовывали nil-результат. Команда падала с panic вместо объяснения.
//
// Контракт, который здесь закрепляется: нечитаемый каталог фикстур — это
// отчёт с причиной (fixtures_dir + fixture_manifests_loaded) и OK=false, а не
// паника и не тихий успех.
func TestConformanceUnreadableFixturesDirDoesNotPanic(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-fixtures")

	report := RunConformance(missing)

	if report.OK {
		t.Fatal("отчёт по несуществующему каталогу фикстур не может быть OK")
	}
	if c, ok := findCheck(report, "fixtures_dir"); !ok {
		t.Fatalf("нет чека fixtures_dir с объяснением; есть: %s", checkNames(report))
	} else if !strings.Contains(c.Details, "не читается") {
		t.Errorf("fixtures_dir без причины: %q", c.Details)
	}
	assertAllChecksNamed(t, report)
}

// Пустой каталог отличается от нечитаемого: его можно открыть, но фикстур в нём
// нет. Раньше такой прогон тоже заканчивался паникой на первом поведенческом
// чеке — теперь каждый отсутствующий файл назван по имени.
func TestConformanceEmptyFixturesDirFailsWithoutPanic(t *testing.T) {
	report := RunConformance(t.TempDir())

	if report.OK {
		t.Fatal("отчёт по пустому каталогу фикстур не может быть OK")
	}
	c, ok := findCheck(report, "fixture_manifests_loaded")
	if !ok {
		t.Fatalf("нет чека fixture_manifests_loaded; есть: %s", checkNames(report))
	}
	if !strings.Contains(c.Details, "нет ни одной фикстуры") {
		t.Errorf("причина провала не объяснена: %q", c.Details)
	}
	if c, ok := findCheck(report, "fixtures_dir"); !ok {
		t.Fatalf("нет чека fixtures_dir с подсказкой про --fixtures; есть: %s", checkNames(report))
	} else if !strings.Contains(c.Details, "каталог фикстур") {
		t.Errorf("fixtures_dir без объяснения: %q", c.Details)
	}
	assertAllChecksNamed(t, report)
}

// Частичный каталог: одна фикстура на месте, остальных нет. Каждая
// поведенческая проверка обязана провалиться ПОД СВОИМ именем — иначе
// отсутствующая фикстура снова станет невидимой.
func TestConformanceMissingFixtureFailsByName(t *testing.T) {
	dir := t.TempDir()
	copyFixture(t, filepath.Join("testdata", "plugins", "echo_ok"), filepath.Join(dir, "echo_ok"))

	report := RunConformance(dir)

	if report.OK {
		t.Fatal("отчёт с одной фикстурой из набора не может быть OK")
	}
	c, ok := findCheck(report, "crasher_fixture")
	if !ok {
		t.Fatalf("нет чека crasher_fixture; есть: %s", checkNames(report))
	}
	if c.Pass {
		t.Errorf("crasher_fixture не может пройти при отсутствующей фикстуре: %q", c.Details)
	}
	assertAllChecksNamed(t, report)
}

// Пустое имя чека — это ровно тот дефект, из-за которого вызовы принимали
// провал за «проверки не было» и разыменовывали nil. Отчёт не должен отдавать
// безымянные проверки ни при каком состоянии каталога фикстур.
func assertAllChecksNamed(t *testing.T, report ConformanceReport) {
	t.Helper()
	for i, c := range report.Checks {
		if strings.TrimSpace(c.Name) == "" {
			t.Errorf("чек #%d без имени (pass=%v): %q", i, c.Pass, c.Details)
		}
	}
}

func findCheck(report ConformanceReport, name string) (ConformanceCheck, bool) {
	for _, c := range report.Checks {
		if c.Name == name {
			return c, true
		}
	}
	return ConformanceCheck{}, false
}

func checkNames(report ConformanceReport) string {
	names := make([]string, 0, len(report.Checks))
	for _, c := range report.Checks {
		names = append(names, c.Name)
	}
	return strings.Join(names, ", ")
}

// copyFixture копирует каталог фикстуры в temp: прогон не должен писать в
// testdata репозитория.
func copyFixture(t *testing.T, src, dst string) {
	t.Helper()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("чтение фикстуры %s: %v", src, err)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatalf("создание %s: %v", dst, err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(src, entry.Name()))
		if err != nil {
			t.Fatalf("чтение %s: %v", entry.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(dst, entry.Name()), raw, 0o644); err != nil {
			t.Fatalf("запись %s: %v", entry.Name(), err)
		}
	}
}
