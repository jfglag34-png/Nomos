package plugintest

// Тесты на `tool plugin test`: матчеры — юнитно, прогон — на фикстурах
// и (интеграционно) на всех шипленных плагинах репозитория.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── матчеры (чистые функции) ────────────────────────────────────────────

func TestMatchValueDeepEqual(t *testing.T) {
	var failures []string
	fail := func(f string, a ...interface{}) { failures = append(failures, f) }

	matchValue("x", 42, 42.0, fail) // YAML int ≡ JSON float64
	if len(failures) != 0 {
		t.Fatalf("int/float должны сойтись: %v", failures)
	}
	matchValue("x", "abc", "abd", fail)
	if len(failures) != 1 {
		t.Fatal("разные строки должны давать провал")
	}
	matchValue("x",
		map[string]interface{}{"a": []interface{}{1.0, 2.0}},
		map[string]interface{}{"a": []interface{}{1.0, 2.0}}, fail)
	if len(failures) != 1 {
		t.Fatalf("глубокое равенство вложенных структур: %v", failures)
	}
}

func TestMatchValueMatchers(t *testing.T) {
	var failures []string
	fail := func(f string, a ...interface{}) { failures = append(failures, f) }

	matchValue("x", map[string]interface{}{"contains": "mock"}, "[mock:x] t", fail)
	matchValue("x", map[string]interface{}{"type": "array"}, []interface{}{}, fail)
	matchValue("x", map[string]interface{}{"type": "string"}, 42.0, fail) // провал
	matchValue("x", map[string]interface{}{"contains": "q"}, 42.0, fail)  // ни строка, ни массив = провал
	matchValue("x", map[string]interface{}{"equals": true}, true, fail)
	if len(failures) != 2 {
		t.Fatalf("ожидалось 2 провала (type, contains-not-str), got %d: %v", len(failures), failures)
	}
}

// contains по массивам — фикс по фидбеку тестера №1
// (раньше «массив не содержит» резал глаз, когда элемент там был).
func TestMatchContainsArrays(t *testing.T) {
	var failures []string
	fail := func(f string, a ...interface{}) { failures = append(failures, f) }

	arr := []interface{}{"syntax_bad", "host_is_ip", "disposable"}
	matchValue("reasons", map[string]interface{}{"contains": "host_is_ip"}, arr, fail)
	if len(failures) != 0 {
		t.Fatalf("существующий элемент должен находиться: %v", failures)
	}

	matchValue("reasons", map[string]interface{}{"contains": "no_such"}, arr, fail)
	if len(failures) != 1 {
		t.Fatal("отсутствующий элемент должен давать провал")
	}
	if !strings.Contains(failures[0], "массив не содержит элемент") {
		t.Fatalf("сообщение должно говорить про элемент массива: %q", failures[0])
	}

	// числовые элементы — через глубокое сравнение
	failures = nil
	matchValue("nums", map[string]interface{}{"contains": 2}, []interface{}{1.0, 2.0, 3.0}, fail)
	if len(failures) != 0 {
		t.Fatalf("YAML int 2 должен совпасть с JSON 1.0,2.0,3.0: %v", failures)
	}
}

// Runtime-проверка типов по манифесту: плагин вернул number там,
// где output объявлен string → contract-тест обязан упасть.
func TestPluginTestCatchesTypeDrift(t *testing.T) {
	requirePython(t)
	passed, failed, err := RunPluginTests("testdata/plugins/type_drifter", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if failed != 1 || passed != 0 {
		t.Fatalf("дрейф типа должен фейлить кейс: passed=%d failed=%d", passed, failed)
	}
}

// Позитивный путь type-check: строка от плагина, объявленная string — проходит.
func TestPluginTestTypeCheckPassesHonestPlugin(t *testing.T) {
	requirePython(t)
	passed, failed, err := RunPluginTests("testdata/plugins/echo_ok", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if failed != 0 || passed == 0 {
		t.Fatalf("честный плагин не должен фейлить type-check: passed=%d failed=%d", passed, failed)
	}
}

// ── прогон на фикстурах ─────────────────────────────────────────────────

func TestPluginTestFixturesPass(t *testing.T) {
	requirePython(t)
	for _, name := range []string{"echo_ok", "failer"} {
		passed, failed, err := RunPluginTests("testdata/plugins/"+name, "", true)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if failed != 0 || passed == 0 {
			t.Fatalf("%s: passed=%d failed=%d", name, passed, failed)
		}
	}
}

func TestPluginTestDetectsFailure(t *testing.T) {
	requirePython(t)
	// спека с заведомо неверным ожиданием
	spec := "tests:\n  - name: ловушка\n    input: { value: \"hi\" }\n    expect: { status: ok, output: { value: \"WRONG\" } }\n"
	specPath := filepath.Join(t.TempDir(), "bad.test.yaml")
	if err := os.WriteFile(specPath, []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}
	passed, failed, err := RunPluginTests("testdata/plugins/echo_ok", specPath, true)
	if err != nil {
		t.Fatal(err)
	}
	if passed != 0 || failed != 1 {
		t.Fatalf("заведомо битое ожидание должно фейлиться: passed=%d failed=%d", passed, failed)
	}
}

// absent — матчер «поля не должно быть». Разбирается не в matchValue (у него
// уже есть значение), а в runCase, где видно, что вообще ушло наружу.
func TestAbsentWanted(t *testing.T) {
	cases := []struct {
		want interface{}
		ok   bool
	}{
		{map[string]interface{}{"absent": true}, true},
		{map[string]interface{}{"absent": false}, false},
		{map[string]interface{}{"present": true}, false},
		{map[string]interface{}{"contains": "x"}, false},
		{nil, false},
		{42.0, false},
		{"absent", false},
		// карта с неизвестным ключом — не матчер, а обычное значение
		{map[string]interface{}{"absent": true, "что-то": 1}, false},
	}
	for _, c := range cases {
		if got := absentWanted(c.want); got != c.ok {
			t.Errorf("absentWanted(%#v) = %v, ждали %v", c.want, got, c.ok)
		}
	}
}

// Кейс из жизни: плагин отдаёт лишние поля (фикстура leaker отдаёт незадекларированный
// junk), ядро их отбрасывает — и «наружу не уходит» проверяется зелёным тестом,
// а не глазами по распечатке. Именно этим раньше проверялось «донор не отдал
// pdb_path», и это было без машинной проверки.
func TestPluginTestAbsentPassesWhenFieldDroppedByContract(t *testing.T) {
	requirePython(t)
	spec := "tests:\n  - name: лишние поля не уходят наружу\n    expect:\n" +
		"      status: ok\n      output:\n        value: \"x\"\n        junk: { absent: true }\n"
	specPath := filepath.Join(t.TempDir(), "absent.test.yaml")
	if err := os.WriteFile(specPath, []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}
	passed, failed, err := RunPluginTests("testdata/plugins/leaker", specPath, true)
	if err != nil {
		t.Fatal(err)
	}
	if failed != 0 || passed != 1 {
		t.Fatalf("отброшенное контрактом поле не должно считаться утечкой: passed=%d failed=%d", passed, failed)
	}
}

// Обратная сторона: absent — не «проверять нечего». Объявленное в манифесте поле
// уходит наружу, и `absent: true` на нём обязан давать провал, иначе матчер
// был бы зелёной лампочкой на любую формулировку.
func TestPluginTestAbsentFailsWhenFieldActuallyLeaves(t *testing.T) {
	requirePython(t)
	spec := "tests:\n  - name: объявленное поле уходит наружу\n    input: { value: hi }\n" +
		"    expect:\n      status: ok\n      output:\n        value: { absent: true }\n"
	specPath := filepath.Join(t.TempDir(), "leak.test.yaml")
	if err := os.WriteFile(specPath, []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}
	passed, failed, err := RunPluginTests("testdata/plugins/echo_ok", specPath, true)
	if err != nil {
		t.Fatal(err)
	}
	if passed != 0 || failed != 1 {
		t.Fatalf("поле, уходящее наружу, обязано ломать absent: passed=%d failed=%d", passed, failed)
	}
}

// Плагин, который вообще не отдаёт поля, — тоже случай «наружу не уходит»:
// absent не должен требовать, чтобы поле хоть где-то мелькнуло.
func TestPluginTestAbsentOnNeverReturnedField(t *testing.T) {
	requirePython(t)
	spec := "tests:\n  - name: плагин не отдавал pdb_path\n    input: { value: hi }\n" +
		"    expect:\n      status: ok\n      output:\n        value: \"hi\"\n        pdb_path: { absent: true }\n"
	specPath := filepath.Join(t.TempDir(), "pdb.test.yaml")
	if err := os.WriteFile(specPath, []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}
	passed, failed, err := RunPluginTests("testdata/plugins/echo_ok", specPath, true)
	if err != nil {
		t.Fatal(err)
	}
	if failed != 0 || passed != 1 {
		t.Fatalf("отсутствующее поле должно проходить absent: passed=%d failed=%d", passed, failed)
	}
}

func TestPluginTestRejectsUnknownFields(t *testing.T) {
	spec := "tests:\n  - name: typo\n    input: { value: hi }\n    expect: { status: ok }\n    unexpected: true\n"
	specPath := filepath.Join(t.TempDir(), "unknown.test.yaml")
	if err := os.WriteFile(specPath, []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := RunPluginTests("testdata/plugins/echo_ok", specPath, true); err == nil {
		t.Fatal("unknown plugin.test.yaml field must be rejected")
	}
}

func TestPluginTestRequiresExpectation(t *testing.T) {
	spec := "tests:\n  - name: no assertion\n    input: { value: hi }\n"
	specPath := filepath.Join(t.TempDir(), "empty-expect.test.yaml")
	if err := os.WriteFile(specPath, []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := RunPluginTests("testdata/plugins/echo_ok", specPath, true); err == nil {
		t.Fatal("test without expect must be rejected")
	}
}

func TestPluginTestNoSpecFile(t *testing.T) {
	if _, _, err := RunPluginTests("testdata/plugins/crasher", "", true); err == nil {
		t.Fatal("отсутствующий plugin.test.yaml обязан быть ошибкой с подсказкой")
	}
}

// Интеграционный прогон всех шипленных плагинов вынесен в
// plugintest_shipped_integration_test.go (тег integration): он зовёт Python и
// порождает процесс на каждый плагин, а в юнит-пакете это делало internal/core
// самым долгим пакетом CI и главным подозреваемым в зависании windows-джоба.
