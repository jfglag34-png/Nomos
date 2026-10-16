package schemacheck

// Тесты проверки схем. Ключевое свойство здесь — ОТКАЗ, а не успех: валидатор
// подмножества обязан говорить, чего он не понимает. Молчаливый пропуск
// неизвестной конструкции сделал бы проверку хуже, чем её отсутствие: она
// выглядела бы работающей.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func schemaOf(t *testing.T, body string) interface{} {
	t.Helper()
	var v interface{}
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestValidateTypeAndRequired(t *testing.T) {
	sch := schemaOf(t, `{"type":"object",
		"required":["a"],
		"properties":{"a":{"type":"string"},"n":{"type":"integer"}}}`)
	if errs := validate(sch, map[string]interface{}{"a": "x", "n": float64(3)}, "$", map[string]bool{}); len(errs) != 0 {
		t.Fatalf("должно было пройти: %v", errs)
	}
	if errs := validate(sch, map[string]interface{}{}, "$", map[string]bool{}); len(errs) == 0 {
		t.Error("без обязательного поля должно падать")
	}
	if errs := validate(sch, map[string]interface{}{"a": float64(1)}, "$", map[string]bool{}); len(errs) == 0 {
		t.Error("строка вместо числа должна падать")
	}
}

func TestValidateAdditionalPropertiesFalse(t *testing.T) {
	sch := schemaOf(t, `{"type":"object","additionalProperties":false,
		"properties":{"a":{"type":"string"}}}`)
	if errs := validate(sch, map[string]interface{}{"a": "x"}, "$", map[string]bool{}); len(errs) != 0 {
		t.Fatalf("должно было пройти: %v", errs)
	}
	errs := validate(sch, map[string]interface{}{"a": "x", "b": 1}, "$", map[string]bool{})
	if len(errs) == 0 {
		t.Fatal("лишнее поле должно падать при additionalProperties:false")
	}
	if !strings.Contains(errs[0], `"b"`) {
		t.Errorf("в ошибке должно называться поле: %v", errs)
	}
}

func TestValidateOneOfRequiresExactlyOne(t *testing.T) {
	sch := schemaOf(t, `{"oneOf":[{"type":"string"},{"type":"object"}]}`)
	if errs := validate(sch, "s", "$", map[string]bool{}); len(errs) != 0 {
		t.Fatalf("строка должна подходить ровно одной ветви: %v", errs)
	}
	if errs := validate(sch, map[string]interface{}{}, "$", map[string]bool{}); len(errs) != 0 {
		t.Fatalf("объект должен подходить ровно одной ветви: %v", errs)
	}
	// Ни одной ветви: нарушителя ровно ноль — это тоже не «ровно одна».
	if errs := validate(sch, float64(1), "$", map[string]bool{}); len(errs) == 0 {
		t.Error("число не подходит ни одной ветви и должно падать")
	}
}

func TestValidateIfThen(t *testing.T) {
	// Связка status=ok требует output. Именно так выражена схема ответа.
	sch := schemaOf(t, `{"type":"object",
		"properties":{"status":{"type":"string"},"output":{"type":"object"}},
		"if":{"properties":{"status":{"const":"ok"}}},
		"then":{"required":["output"]}}`)
	if errs := validate(sch, map[string]interface{}{"status": "ok", "output": map[string]interface{}{}}, "$", map[string]bool{}); len(errs) != 0 {
		t.Fatalf("status=ok с output должен проходить: %v", errs)
	}
	if errs := validate(sch, map[string]interface{}{"status": "ok"}, "$", map[string]bool{}); len(errs) == 0 {
		t.Error("status=ok без output должен падать по then")
	}
	if errs := validate(sch, map[string]interface{}{"status": "error"}, "$", map[string]bool{}); len(errs) != 0 {
		t.Fatalf("then не применяется, если if не сошёлся: %v", errs)
	}
}

func TestValidatePatternAndBounds(t *testing.T) {
	sch := schemaOf(t, `{"type":"string","pattern":"^[a-z]+$","minLength":2,"maxLength":5}`)
	if errs := validate(sch, "abc", "$", map[string]bool{}); len(errs) != 0 {
		t.Fatalf("должно было пройти: %v", errs)
	}
	if errs := validate(sch, "a", "$", map[string]bool{}); len(errs) == 0 {
		t.Error("короче minLength должно падать")
	}
	if errs := validate(sch, "abcdefg", "$", map[string]bool{}); len(errs) == 0 {
		t.Error("длиннее maxLength должно падать")
	}
	if errs := validate(sch, "ABC", "$", map[string]bool{}); len(errs) == 0 {
		t.Error("не под pattern должно падать")
	}
}

// Правило, которое движок Go применить не может, должно ПОПАДАТЬ В ОТЧЕТ, а не
// молча пропускаться. Иначе в отчёте написано «всё проверено».
func TestUncompilablePatternIsReportedNotSkipped(t *testing.T) {
	// Отрицательный просмотр вперёд — RE2 так не умеет, а draft-07 умеет.
	sch := schemaOf(t, `{"type":"string","pattern":"^(?!x).+"}`)
	seen := map[string]bool{}
	if errs := validate(sch, "abc", "$", seen); len(errs) != 0 {
		t.Fatalf("это не нарушение, а граница инструмента, попадать в ошибки не должно: %v", errs)
	}
	if len(seen) == 0 {
		t.Fatal("непроверяемое правило обязано быть зафиксировано, иначе отчёт врёт")
	}
}

// Ключ, которого валидатор не знает, обязан быть отвергнут. Иначе схема может
// набирать правила, которые никто не проверяет.
func TestUnknownConstructIsRejected(t *testing.T) {
	sch := schemaOf(t, `{"type":"object","propertyNames":{"pattern":"^x"}}`)
	out := map[string]bool{}
	unsupportedConstructs(sch, "", out)
	if len(out) != 0 {
		t.Errorf("здесь всё поддержано, а найдено: %v", out)
	}

	sch2 := schemaOf(t, `{"type":"object","dependencies":{"a":["b"]}}`)
	out2 := map[string]bool{}
	unsupportedConstructs(sch2, "", out2)
	if len(out2) == 0 {
		t.Error("конструкция dependencies не поддержана и обязана быть названа")
	}
}

// ИМЕНА СВОЙСТВ — НЕ КЛЮЧЕВЫЕ СЛОВА СХЕМЫ. Обход, который этого не различает,
// объявляет каждое свойство неподдержанной конструкцией: так было в первой
// версии, и шаг падал на 78 таких.
func TestPropertyNamesAreNotTreatedAsKeywords(t *testing.T) {
	sch := schemaOf(t, `{"type":"object","properties":{
		"title":{"type":"string"},
		"default":{"type":"string"},
		"items":{"type":"array"},
		"type":{"type":"string"}}}`)
	out := map[string]bool{}
	unsupportedConstructs(sch, "", out)
	if len(out) != 0 {
		t.Errorf("имена свойств не должны считаться конструкциями схемы: %v", out)
	}
}

// Настоящий репозиторий должен сходиться.
func TestCheckRepoOnRealRepository(t *testing.T) {
	repo, err := findRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	out, err := CheckRepo(repo)
	if err != nil {
		t.Fatalf("репозиторий разошёлся со своими схемами:\n%s", out)
	}
	if !strings.Contains(out, "пайплайны:") || !strings.Contains(out, "манифесты:") {
		t.Errorf("отчёт неполон — проверка, скорее всего, ничего не нашла:\n%s", out)
	}
}

// Правило, которое не применилось, обязано быть видно в отчёте: иначе «37/37
// соответствуют» звучит как «всё проверено», хотя одно правило не проверено.
func TestCheckRepoMentionsUnverifiableRule(t *testing.T) {
	repo, err := findRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	out, err := CheckRepo(repo)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "не компилируется движком Go") {
		t.Errorf("правило, которое Go не берёт, должно быть названо в отчёте:\n%s", out)
	}
}

func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}

// Детали обеих пачек манифестов обязаны попадать в отчёт. Раньше problems был
// глобалом, который каждый вызов checkDocs обнулял: счётчик считал обе пачки,
// а в отчёте оставалась только вторая. Тест ломает по одному манифесту в
// каждом из двух паттернов и требует оба пути в отчёте.
func TestCheckRepoKeepsDetailsFromBothManifestBatches(t *testing.T) {
	src, err := findRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	for _, dir := range []string{"schemas", "examples"} {
		if err := copyDir(t, filepath.Join(src, dir), filepath.Join(repo, dir)); err != nil {
			t.Fatal(err)
		}
	}
	// Заведомо битые манифесты: не YAML вовсе, так что виноват путь чтения, а
	// не тонкости схемы, — для проверки накопления этого достаточно: обе ветви
	// checkDocs складывают детали в один отчёт.
	// Первый паттерн — plugins/*/plugin.yaml (глубина 3), второй —
	// plugins/*/*/plugin.yaml (глубина 4). Ломаем по одному файлу в каждом,
	// чтобы вторая пачка не затирала первую.
	broken := []string{
		filepath.Join(repo, "plugins", "solo", "plugin.yaml"),
		filepath.Join(repo, "plugins", "official", "a", "plugin.yaml"),
	}
	for _, p := range broken {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("{{{ не yaml\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out, err := CheckRepo(repo)
	if err == nil {
		t.Fatalf("два битых манифеста обязаны ронять проверку:\n%s", out)
	}
	for _, want := range []string{"solo/plugin.yaml", "official/a/plugin.yaml"} {
		if !strings.Contains(out, want) {
			t.Errorf("в отчёте нет детали пачки: нет %q:\n%s", want, out)
		}
	}
}

// Схема plugin.test.yaml обязана быть опубликована, а не «де-факто в коде».
// Проверяем обе стороны: файл с объявленным сервисом проходит, а файл с тем же
// сервисом, но с опечаткой в типе или в имени поля — падает. Иначе новый
// раздел контракта жил бы без схемы, и опечатка `servcies` прошла бы гейт.
func TestCheckRepoValidatesPluginTestFiles(t *testing.T) {
	src, err := findRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	build := func(t *testing.T, spec string) string {
		t.Helper()
		repo := t.TempDir()
		for _, dir := range []string{"schemas", "examples"} {
			if err := copyDir(t, filepath.Join(src, dir), filepath.Join(repo, dir)); err != nil {
				t.Fatal(err)
			}
		}
		dir := filepath.Join(repo, "plugins", "official", "x")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		manifest := "id: x\nversion: 0.1.0\nplatform_api: \"^0.1\"\n" +
			"runtime:\n  type: python\n  entry: main.py\noutput:\n  value: { type: string }\n"
		if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "plugin.test.yaml"), []byte(spec), 0o644); err != nil {
			t.Fatal(err)
		}
		return repo
	}

	good := "services:\n  - type: http\n    root: fixtures/site\n    base_url: \"{{base_url}}\"\n" +
		"tests:\n  - name: кейс\n    input: { url: \"{{base_url}}/\" }\n" +
		"    expect: { status: ok }\n"
	out, err := CheckRepo(build(t, good))
	if err != nil {
		t.Fatalf("файл с объявленным сервисом обязан соответствовать схеме:\n%s", out)
	}
	if !strings.Contains(out, "файлы тестов: 1/1") {
		t.Errorf("отчёт должен называть проверенные файлы тестов:\n%s", out)
	}

	for name, bad := range map[string]string{
		"чужой тип сервиса": "services:\n  - type: postgres\n    root: fixtures/site\ntests:\n  - name: к\n    expect: { status: ok }\n",
		"нет root":          "services:\n  - type: http\ntests:\n  - name: к\n    expect: { status: ok }\n",
		"опечатка в поле":   "services:\n  - type: http\n    rootdir: fixtures/site\ntests:\n  - name: к\n    expect: { status: ok }\n",
		"опечатка в кейсе":  "tests:\n  - name: к\n    inpt: { value: 1 }\n    expect: { status: ok }\n",
		"кейс без expect":   "tests:\n  - name: к\n    input: { value: 1 }\n",
		"мусорный статус":   "tests:\n  - name: к\n    expect: { status: maybe }\n",
	} {
		out, err = CheckRepo(build(t, bad))
		if err == nil {
			t.Errorf("%s: такой файл обязан падать, а прошёл:\n%s", name, out)
		}
	}
}

func copyDir(t *testing.T, src, dst string) error {
	t.Helper()
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, raw, 0o644)
	})
}
