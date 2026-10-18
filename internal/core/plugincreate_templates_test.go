package core

// Тесты генератора по шаблонам.
//
// Критерий тот же, что у скелета и жёстче: плагин из шаблона обязан быть
// зелёным из коробки. Для шаблона это нетривиально — он содержит работающий
// код, значит ошибка в нём видна сразу как красный тест.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// generateAndRun — общий путь проверки: создать, провалидировать, прогнать
// контракт-тесты. Возвращает число зелёных тестов.
func generateAndRun(t *testing.T, name string, opts CreateOptions) int {
	t.Helper()
	requirePython(t)
	dir := filepath.Join(t.TempDir(), name)
	if _, err := CreatePluginWith(dir, opts); err != nil {
		t.Fatalf("create: %v", err)
	}
	if errs := ValidatePluginDir(dir); len(errs) != 0 {
		t.Fatalf("манифест невалиден: %v", errs)
	}
	passed, failed, err := RunPluginTests(dir, "", true)
	if err != nil {
		t.Fatalf("тесты не запустились: %v", err)
	}
	if failed != 0 {
		out, _ := os.ReadFile(filepath.Join(dir, "plugin.test.yaml"))
		t.Fatalf("плагин из шаблона должен быть зелёным из коробки: passed=%d failed=%d\n%s",
			passed, failed, out)
	}
	return passed
}

func TestTemplateTextMetricsIsGreen(t *testing.T) {
	if n := generateAndRun(t, "t_metrics", CreateOptions{Template: "text-metrics"}); n == 0 {
		t.Fatal("шаблон не дал ни одного теста")
	}
}

func TestTemplateRegexReplaceDefaultsAreGreen(t *testing.T) {
	// Умолчание (схлопывание пробелов) — единственный случай, где точный
	// результат подмены вычислим генератором, поэтому тест тут точный.
	generateAndRun(t, "t_collapse", CreateOptions{Template: "regex-replace"})
}

func TestTemplateLineFilterDefaultsAreGreen(t *testing.T) {
	generateAndRun(t, "t_filter", CreateOptions{Template: "line-filter"})
}

// Своя регулярка: точный результат генератор вычислить не может, поэтому
// ожидания структурные. Плагин всё равно обязан быть зелёным.
func TestTemplateRegexReplaceCustomPatternIsGreen(t *testing.T) {
	generateAndRun(t, "t_custom", CreateOptions{
		Template:    "regex-replace",
		Pattern:     `(\d{4})-(\d{2})-(\d{2})`,
		Replacement: `\3.\2.\1`,
	})
}

func TestTemplateLineFilterCustomPatternIsGreen(t *testing.T) {
	generateAndRun(t, "t_filter_custom", CreateOptions{
		Template: "line-filter",
		Pattern:  `(?i)warn`,
	})
}

// Параметр попадает в генерированный код. Значит генератор обязан экранировать
// его, иначе параметр превращается в произвольный код — а это ровно то, чего
// шаблоны должны избегать by design.
func TestTemplateParameterCannotInjectCode(t *testing.T) {
	requirePython(t)
	// Попытка закрыть литерал и дописать вызов. При корректном экранировании
	// это просто текст, который ищется по шаблону и не находится.
	evil := `x'; import os; os.system("id") #`
	dir := filepath.Join(t.TempDir(), "t_evil")
	if _, err := CreatePluginWith(dir, CreateOptions{
		Template:    "regex-replace",
		Pattern:     evil,
		Replacement: `x`,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "main.py"))
	if err != nil {
		t.Fatal(err)
	}
	// В исходнике не должно быть НИКАКОГО вызова os.system в исполняемом виде.
	if strings.Contains(string(raw), "os.system(\"id\")\n") ||
		strings.Contains(string(raw), "\nos.system(") {
		t.Fatalf("параметр попал в код как исполняемый:\n%s", raw)
	}
	if errs := ValidatePluginDir(dir); len(errs) != 0 {
		t.Fatalf("манифест невалиден: %v", errs)
	}
	passed, failed, err := RunPluginTests(dir, "", true)
	if err != nil {
		t.Fatalf("тесты не запустились: %v", err)
	}
	if failed != 0 {
		t.Fatalf("злой параметр должен остаться данными: passed=%d failed=%d", passed, failed)
	}
}

// Битая регулярка обязана ловиться при генерации. Иначе сгенерированный плагин
// окажется красным из коробки, то есть сработает ровно тот случай, ради
// которого шаблоны и делались.
func TestTemplateRejectsBrokenPattern(t *testing.T) {
	for _, bad := range []string{`(unclosed`, `[z-a]`, `a{2,1}`, `(`} {
		dir := filepath.Join(t.TempDir(), "t_bad")
		if _, err := CreatePluginWith(dir, CreateOptions{
			Template: "regex-replace", Pattern: bad,
		}); err == nil {
			t.Errorf("регулярка %q принята, а должна быть отвергнута", bad)
		} else if !strings.Contains(err.Error(), "pattern") {
			t.Errorf("ошибка должна называть --pattern: %v", err)
		}
	}
}

// Ошибка шаблона не должна оставлять папку с половиной файлов: пользователь
// увидит «папка не пуста» при следующей попытке и запутается.
func TestTemplateFailureLeavesNoPartialDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "t_partial")
	if _, err := CreatePluginWith(dir, CreateOptions{
		Template: "regex-replace", Pattern: `(unclosed`,
	}); err == nil {
		t.Fatal("ожидалась ошибка")
	}
	if _, err := os.Stat(dir); err == nil {
		entries, _ := os.ReadDir(dir)
		if len(entries) > 0 {
			t.Fatalf("после ошибки осталось файлов: %d", len(entries))
		}
	}
}

func TestTemplateUnknownRejected(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "t_whatever")
	_, err := CreatePluginWith(dir, CreateOptions{Template: "нет-такого"})
	if err == nil {
		t.Fatal("несуществующий шаблон должен отвергаться")
	}
	// В сообщении должны быть перечислены доступные имена, иначе
	// пользователь угадывает.
	for _, want := range []string{"skeleton", "text-metrics", "regex-replace", "line-filter"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("в ошибке нет имени шаблона %q: %v", want, err)
		}
	}
}

func TestTemplateListMentionsEveryTemplate(t *testing.T) {
	list := ListTemplates()
	for _, tpl := range TemplateNames() {
		if !strings.Contains(list, tpl) {
			t.Errorf("шаблон %q не попал в перечень", tpl)
		}
	}
	if !strings.Contains(list, "untrusted") {
		t.Error("перечень должен предупреждать про untrusted: иначе шаблон читается как " +
			"«безопасная кнопка запуска кода»")
	}
}

func TestParseCreateArgsTemplateFlags(t *testing.T) {
	_, o, list, err := ParseCreateArgs([]string{
		"plugins/x", "--template=regex-replace", "--pattern=a+", "--replacement=b",
	})
	if err != nil || list {
		t.Fatalf("err=%v list=%v", err, list)
	}
	if o.Template != "regex-replace" || o.Pattern != "a+" || o.Replacement != "b" {
		t.Fatalf("флаги шаблона не разобрались: %+v", o)
	}
}

func TestParseCreateArgsListTemplates(t *testing.T) {
	_, _, list, err := ParseCreateArgs([]string{"--list-templates"})
	if err != nil || !list {
		t.Fatalf("--list-templates: err=%v list=%v", err, list)
	}
}

// pyLiteral — точка, где параметр превращается в код. Здесь важно не только
// экранирование, но и отказ вместо искажения управляющих символов.
func TestPyLiteralEscaping(t *testing.T) {
	cases := []struct{ in, want string }{
		{`abc`, `'abc'`},
		{`a'b`, `'a\'b'`},
		{`a\b`, `'a\\b'`},
		{"a\nb", `'a\nb'`},
		{"a\tb", `'a\tb'`},
		{`\\1`, `'\\\\1'`}, // обратная ссылка re.sub должна дойти как есть
		{`привет`, `'привет'`},
	}
	for _, c := range cases {
		got, err := PyLiteralForTest(c.in)
		if err != nil {
			t.Errorf("pyLiteral(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("pyLiteral(%q) = %s, ждали %s", c.in, got, c.want)
		}
	}
}

func TestPyLiteralRejectsControlChars(t *testing.T) {
	for _, bad := range []string{"a\x00b", "a\x01b", "a\x7fb"} {
		if got, err := PyLiteralForTest(bad); err == nil {
			t.Errorf("pyLiteral(%q) = %s, а должен был отказать", bad, got)
		}
	}
}

// Сгенерированный плагин не должен заявлять сеть, файлы или секреты: шаблон
// про это не просил, а «не заявлено» здесь значит «запрещено».
func TestTemplateManifestGrantsNothing(t *testing.T) {
	requirePython(t)
	for _, name := range []string{"text-metrics", "regex-replace", "line-filter"} {
		dir := filepath.Join(t.TempDir(), "t_perm")
		if _, err := CreatePluginWith(dir, CreateOptions{Template: name}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		raw, err := os.ReadFile(filepath.Join(dir, "plugin.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		m := string(raw)
		if !strings.Contains(m, "network: []") ||
			!strings.Contains(m, "filesystem: none") ||
			!strings.Contains(m, "secrets: []") {
			t.Errorf("шаблон %s заявил лишние разрешения:\n%s", name, m)
		}
	}
}
