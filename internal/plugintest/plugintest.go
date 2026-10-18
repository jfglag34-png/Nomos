package plugintest

// tool plugin test <dir> — контракт-тесты для авторов плагинов (ТЗ v2 §6.1).
// Плагин гоняется на фикстурах из plugin.test.yaml через ТОТ ЖЕ механизм
// subprocess+stdin/stdout, что использует ядро. Плюс enforce контракта:
// теоретический тест отличается от практического тем, что практический
// проходит через настоящий рантайм.

import (
	"bytes"
	stdctx "context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wykserdex/wedra/internal/common"
	"github.com/wykserdex/wedra/internal/pipeline"
	"github.com/wykserdex/wedra/internal/plugin"
	"gopkg.in/yaml.v3"
)

// локальные алиасы — ранее из internal/core/types.go
type Manifest = pipeline.Manifest
type Duration = pipeline.Duration

var NewEngine = plugin.NewEngine

func truncate(s string, n int) string { return common.Truncate(s, n) }
func kindOf(v interface{}) string {
	switch v.(type) {
	case bool:
		return "boolean"
	case float64:
		return "number"
	case string:
		return "string"
	case []interface{}:
		return "array"
	case map[string]interface{}:
		return "object"
	case nil:
		return "null"
	}
	return "unknown"
}

// ── Схема plugin.test.yaml ──────────────────────────────────────────────

type PluginTestCase struct {
	Name     string                 `yaml:"name"`
	Env      map[string]string      `yaml:"env"`       // env на время теста (секреты, mock-режимы)
	Input    map[string]interface{} `yaml:"input"`     // JSON на stdin
	InputRaw string                 `yaml:"input_raw"` // сырой stdin — для тестов битого JSON
	Timeout  Duration               `yaml:"timeout"`   // дефолт 10s
	Expect   Expectation            `yaml:"expect"`
}

type ErrorExpectation struct {
	Code      string `yaml:"code"`
	Retryable *bool  `yaml:"retryable"`
}

type Expectation struct {
	Status   string                 `yaml:"status"` // ok | error | (пусто — не проверять)
	ExitCode *int                   `yaml:"exit_code"`
	Output   map[string]interface{} `yaml:"output"` // значение: литерал ИЛИ матчер
	Error    *ErrorExpectation      `yaml:"error"`
}

// TestService — декларативная подготовка окружения ДО кейсов (см.
// plugintest_service.go). Сегодня единственный тип — локальный HTTP-сервер:
//
//	services:
//	  - type: http
//	    root: fixtures/site
//	    base_url: "{{base_url}}"
//
// Поля нет намеренно: произвольная команда в файле тестов — это запуск кода по
// описанию, а не по доверию оператора. Подготовить что-то ещё — отдельная задача.
type TestService struct {
	Type    string `yaml:"type"`     // http — единственный поддерживаемый тип
	Root    string `yaml:"root"`     // каталог, который отдаётся (от каталога плагина)
	BaseURL string `yaml:"base_url"` // шаблон: имя плейсхолдера, подставляемого в кейсы
}

type PluginTestFile struct {
	Services []TestService    `yaml:"services"` // опционально; без него поведение прежнее
	Tests    []PluginTestCase `yaml:"tests"`
}

// Матчеры для полей output:
//
//	value: 42                    — точное (глубокое) равенство
//	value: { present: true }     — поле существует
//	value: { absent: true }      — поле НЕ уходит наружу (см. absentWanted/leaksOut)
//	value: { contains: "foo" }   — строка содержит подстроку; МАССИВ содержит элемент (любой тип)
//	value: { type: "array" }     — тип (boolean|number|string|array|object)
var matcherKeys = map[string]bool{"present": true, "absent": true, "contains": true, "type": true, "equals": true}

type CaseResult struct {
	Name     string
	Pass     bool
	Ms       int64
	Details  []string
	Warnings []string
}

func deepEqual(a, b interface{}) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return bytes.Equal(ab, bb)
}

func isMatcherMap(m map[string]interface{}) bool {
	if len(m) == 0 {
		return false
	}
	for k := range m {
		if !matcherKeys[k] {
			return false
		}
	}
	return true
}

// absentWanted — просят ли матчером «этого поля быть не должно».
//
// Только `absent: true`. `absent: false` — то же, что отсутствие просьбы
// (как `present: false` сегодня): поле проверяется как обычно, то есть на
// наличие. Отдельной проверки значения тут быть не может — матчер про поле,
// которого может не быть.
func absentWanted(want interface{}) bool {
	m, ok := want.(map[string]interface{})
	if !ok || !isMatcherMap(m) {
		return false
	}
	b, isBool := m["absent"].(bool)
	return isBool && b
}

// leaksOut — уходит ли поле наружу, то есть попадает ли оно в то, что ран
// положит в контекст шага.
//
// «Наружу» — это выход EnforceOutput, а не сырой stdout плагина: незадекларированное
// поле ядро отбрасывает (runner.go, MCP и conformance зовут его одинаково), и
// проверять надо именно эту границу. Плагин, отдающий лишние поля, — это норма
// (фикстура leaker), и ловить его должен контракт, а не тест автора.
//
// На не-ok ответе EnforceOutput не зовётся вовсе: наружу не уходит ничего, и
// поле утечь не может. Если же контракт нарушен, clean пуст, а кейс и так
// красный — тогда судим строго, по сырому ответу плагина.
func leaksOut(enforced map[string]interface{}, raw map[string]interface{}, key string) bool {
	if enforced != nil {
		_, ok := enforced[key]
		return ok
	}
	_, ok := raw[key]
	return ok
}

// matchContains: строка — подстрока; массив — вхождение элемента (глубокое сравнение).
// Сообщения различают случаи: «ожидали подстроку, получили [host_is_ip]» обманывал,
// когда элемент визуально был в массиве (фидбек тестера №1).
func matchContains(path string, want, got interface{}, fail func(string, ...interface{})) {
	switch g := got.(type) {
	case string:
		needle, ok := want.(string)
		if !ok {
			fail("%s: contains по строке требует строку, получили %v", path, want)
			return
		}
		if !strings.Contains(g, needle) {
			fail("%s: строка не содержит подстроки %q: %s", path, needle, truncate(g, 80))
		}
	case []interface{}:
		for _, el := range g {
			if deepEqual(el, want) {
				return
			}
		}
		fail("%s: массив не содержит элемент %v: %s", path, want, truncate(fmt.Sprint(g), 80))
	default:
		fail("%s: contains применим к строке или массиву, получили %s (%s)",
			path, kindOf(got), truncate(fmt.Sprint(got), 80))
	}
}

func matchValue(path string, want, got interface{}, fail func(string, ...interface{})) {
	if m, ok := want.(map[string]interface{}); ok && isMatcherMap(m) {
		for k, v := range m {
			switch k {
			case "present": // наличие ключа уже проверено вызывающим
			case "absent": // проверяет runCase: нужно знать, УШЁЛ ли ключ наружу
			case "contains":
				matchContains(path, v, got, fail)
			case "type":
				wantType, _ := v.(string)
				if kindOf(got) != wantType {
					fail("%s: тип %q не совпал с ожидаемым %q", path, kindOf(got), wantType)
				}
			case "equals":
				if !deepEqual(v, got) {
					fail("%s: ожидали %v, получили %v", path, v, got)
				}
			}
		}
		return
	}
	if !deepEqual(want, got) {
		fail("%s: ожидали %v, получили %v", path, want, truncate(fmt.Sprint(got), 80))
	}
}

func mapToEnv(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

func runCase(ctx stdctx.Context, m *Manifest, tc PluginTestCase) CaseResult {
	cr := CaseResult{Name: tc.Name, Pass: true}
	fail := func(format string, a ...interface{}) {
		cr.Pass = false
		cr.Details = append(cr.Details, fmt.Sprintf(format, a...))
	}
	warn := func(format string, a ...interface{}) {
		cr.Warnings = append(cr.Warnings, fmt.Sprintf(format, a...))
	}

	var stdin []byte
	if tc.InputRaw != "" {
		stdin = []byte(tc.InputRaw)
	} else {
		b, err := json.Marshal(tc.Input)
		if err != nil {
			fail("input не сериализуется в JSON: %v", err)
			return cr
		}
		stdin = b
	}

	timeout := tc.Timeout.Duration
	if timeout == 0 {
		timeout = 10 * time.Second
	}

	start := time.Now()
	res := plugin.ExecWithEnvCtx(ctx, m, stdin, timeout, mapToEnv(tc.Env))
	cr.Ms = time.Since(start).Milliseconds()

	exp := tc.Expect
	if exp.ExitCode != nil && res.ExitCode != *exp.ExitCode {
		fail("exit_code: ожидали %d, получили %d", *exp.ExitCode, res.ExitCode)
	}
	switch exp.Status {
	case "ok":
		if !res.OK() {
			fail("ожидали ok, получили exit=%d code=%s msg=%s", res.ExitCode, res.ErrCode, res.ErrMsg)
		}
	case "error":
		if res.OK() {
			fail("ожидали error, получили ok")
		}
	}

	// enforce контракта на успешных ответах — то, что сделает ядро в ране.
	// clean сохраняем: по нему судим, что реально уходит наружу (матчер absent).
	var enforced map[string]interface{}
	if res.OK() {
		if clean, dropped, cvErr := plugin.EnforceOutput(m, res.Output); cvErr != nil {
			fail("контракт: %v", cvErr)
		} else {
			enforced = clean
			for _, d := range dropped {
				warn("незадекларированное поле отброшено: %s", d)
			}
		}
		// типы по манифесту: дрейф типа не должен проходить молча
		for name, port := range m.Output {
			v, present := res.Output[name]
			if !present {
				continue // отсутствие проверено EnforceOutput
			}
			if v == nil && port.Optional {
				continue
			}
			if port.Type != "" && kindOf(v) != port.Type {
				fail("контракт: поле %q объявлено как %s, вернулось %s",
					name, port.Type, kindOf(v))
			}
		}
	}

	for key, wantVal := range exp.Output {
		// `absent` — единственный матчер, которому поле может быть не нужно:
		// без него «плагин не отдал наружу лишнего» проверялось только глазами
		// по распечатке отчёта, а не зелёным тестом.
		if absentWanted(wantVal) {
			if leaksOut(enforced, res.Output, key) {
				fail("output.%s уходит наружу, а ожидалось, что не уйдёт", key)
			}
			continue
		}
		got, ok := res.Output[key]
		if !ok {
			fail("output.%s отсутствует", key)
			continue
		}
		matchValue("output."+key, wantVal, got, fail)
	}

	if exp.Error != nil {
		if exp.Error.Code != "" && res.ErrCode != exp.Error.Code {
			// v9: crash → platform:<code> — подсказка для авторов, у кого тест ожидает crash
			hint := ""
			if exp.Error.Code == "crash" && strings.HasPrefix(res.ErrCode, "platform:") {
				hint = " (в v9 код переименован в platform:<code> — поправьте expect на " + res.ErrCode + ")"
			} else if strings.HasPrefix(res.ErrCode, "platform:") && res.ErrCode == "platform:"+exp.Error.Code {
				hint = " (в v9 exit>=2 сохраняет код как platform:<code> — ожидайте " + res.ErrCode + ")"
			}
			fail("error.code: ожидали %q, получили %q (%s)%s", exp.Error.Code, res.ErrCode, res.ErrMsg, hint)
		}
		if exp.Error.Retryable != nil && res.Retryable != *exp.Error.Retryable {
			fail("error.retryable: ожидали %v, получили %v", *exp.Error.Retryable, res.Retryable)
		}
	}
	if !cr.Pass && strings.TrimSpace(res.Stderr) != "" {
		cr.Details = append(cr.Details, "stderr: "+truncate(res.Stderr, 300))
	}
	return cr
}

func printCase(cr CaseResult, quiet bool) {
	if quiet {
		return
	}
	if cr.Pass {
		fmt.Printf("  ✓ %s (%dms)\n", cr.Name, cr.Ms)
	} else {
		fmt.Printf("  ✗ %s (%dms)\n", cr.Name, cr.Ms)
		for _, d := range cr.Details {
			fmt.Printf("      %s\n", d)
		}
	}
	for _, w := range cr.Warnings {
		fmt.Printf("      · %s\n", w)
	}
}

func hasExpectation(expect Expectation) bool {
	return expect.Status != "" || expect.ExitCode != nil || len(expect.Output) > 0 || expect.Error != nil
}

// RunPluginTests прогоняет plugin.test.yaml плагина. specPath == "" → <dir>/plugin.test.yaml.
//
// Контекст здесь не принимается намеренно: CLI зовёт эту функцию из
// синхронного кода, и «а если сети нет» его не касается. Точка входа с отменой —
// RunPluginTestsCtx.
func RunPluginTests(dir, specPath string, quiet bool) (passed, failed int, err error) {
	return RunPluginTestsCtx(stdctx.Background(), dir, specPath, quiet)
}

// RunPluginTestsCtx — тот же прогон, но с контекстом вызывающего.
//
// Нужен HTTP-обработчику редактора плагинов: там прогон инициирован запросом
// браузера, и без отмены закрытое соединение оставило бы тесты и поднятые ими
// сервисы работать до собственного wall_timeout — то есть запрос отменён, а код
// исполняется. Отмена по ctx проходит вниз до запуска процесса плагина.
func RunPluginTestsCtx(ctx stdctx.Context, dir, specPath string, quiet bool) (passed, failed int, err error) {
	m, err := NewEngine().LoadManifest(dir)
	if err != nil {
		return 0, 0, err
	}
	// Доверие каталогу, который назвал оператор: `wedra plugin test <dir>` —
	// это просьба человека прогнать ИМЕННО ЭТОТ код. Основание то же, что у
	// `AllowListFromDirs`: решение принял человек, а не плагин. Без этого
	// разработка плагина была бы невозможна (а на Windows/macOS ещё и не
	// работала бы: изолятора там нет, и внешний код не запускается вовсе).
	//
	// Это НЕ обход доверия в раннере: ран пайплайна сюда не ходит, у него
	// свой путь — ядро и allow-list оператора.
	allow, allowErr := plugin.AllowListFromDirs(dir)
	if allowErr != nil {
		return 0, 0, allowErr
	}
	ctx = plugin.WithTrustPolicy(ctx, plugin.TrustPolicy{Trusted: allow})
	if specPath == "" {
		specPath = filepath.Join(dir, "plugin.test.yaml")
	}
	raw, err := os.ReadFile(specPath)
	if err != nil {
		return 0, 0, fmt.Errorf("нет файла тестов %s: %w", specPath, err)
	}
	var spec PluginTestFile
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&spec); err != nil {
		return 0, 0, fmt.Errorf("%s: %w", specPath, err)
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return 0, 0, fmt.Errorf("%s: несколько YAML-документов", specPath)
		}
		return 0, 0, fmt.Errorf("%s: %w", specPath, err)
	}
	if len(spec.Tests) == 0 {
		return 0, 0, fmt.Errorf("%s: ни одного теста", specPath)
	}
	for i, tc := range spec.Tests {
		if !hasExpectation(tc.Expect) {
			return 0, 0, fmt.Errorf("%s: тест %d не содержит expect", specPath, i+1)
		}
	}

	if len(m.Permissions.Secrets) > 0 && !quiet {
		fmt.Printf("  ℹ манифест заявляет секреты %v — сетевые тесты требуют env (или mock-режима)\n",
			m.Permissions.Secrets)
	}

	// Окружение готовится ОДИН раз на прогон и до первого кейса: сервис не
	// должен подниматься и гаситься на каждый кейс, а его ошибки (нет root,
	// неизвестный тип, неизвестный плейсхолдер) должны останавливать прогон
	// целиком, а не на середине. Close в defer — сервис не переживёт прогон,
	// даже если кейс упал.
	svcs, err := startTestServices(dir, spec.Services, quiet)
	if err != nil {
		return 0, 0, fmt.Errorf("%s: %w", specPath, err)
	}
	defer svcs.Close()

	cases := make([]PluginTestCase, 0, len(spec.Tests))
	for i, tc := range spec.Tests {
		prepared, err := svcs.prepareCase(tc, i+1)
		if err != nil {
			return 0, 0, fmt.Errorf("%s: %w", specPath, err)
		}
		if prepared.Name == "" {
			prepared.Name = "(без имени)"
		}
		cases = append(cases, prepared)
	}

	for _, tc := range cases {
		res := runCase(ctx, m, tc)
		printCase(res, quiet)
		if res.Pass {
			passed++
		} else {
			failed++
		}
	}
	return passed, failed, nil
}
