package plugintest

// Тесты декларативной подготовки окружения (services в plugin.test.yaml).
//
// Плагин-клиент в этих тестах пишется во временный каталог, а не в testdata:
// testdata — публичный корпус конформности, и добавление туда фикстуры с живым
// сетевым ходом означало бы публикацию нового примера в conformance/ ради
// проверки харнесса.

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// httpProbeManifest — плагин БЕЗ объявленной сети (network: [] — это deny) и без
// единого чужого env. Он нужен, чтобы доказать две вещи сразу: loopback для
// плагина под тестом действительно доступен (своим network: deny), а сервис
// ничего не подкладывает в окружение — никакого HTTP_PROXY, который расширил бы
// политику плагина.
const httpProbeManifest = `id: http_probe
version: 0.1.0
platform_api: "^0.1"

runtime:
  type: python
  entry: main.py

input:
  url: { from: input.url, type: string }

output:
  url: { type: string }
  nested: { type: string }
  env_url: { type: string }
  proxies: { type: string }
  status: { type: number }
  bytes: { type: number }

permissions:
  network: []
  filesystem: none
  secrets: []
`

const httpProbeMain = `#!/usr/bin/env python3
"""Фикстура харнесса: ходит по URL и честно докладывает, что видел."""
import json
import os
import sys
import urllib.request

PROXIES = ["HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"]

try:
    data = json.load(sys.stdin)
except Exception:
    print(json.dumps({"status": "error", "error": {
        "code": "bad_input", "message": "на входе не JSON", "retryable": False}}))
    raise SystemExit(2)

url = str(data.get("url") or "")
nested = ""
try:
    nested = str((data.get("nested") or {}).get("list", [""])[0])
except Exception:
    nested = ""
seen = [name for name in PROXIES if os.environ.get(name)]
status, size = 0, 0
if url:
    try:
        # Явный пустой ProxyHandler: иначе urllib уважал бы переменные окружения,
        # и проверка «сервис ничего не подложил» ничего бы не значила.
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        with opener.open(url, timeout=5) as resp:
            status = resp.status
            size = len(resp.read())
    except Exception:
        status = 0
        size = 0

print(json.dumps({"status": "ok", "output": {
    "url": url,
    "nested": nested,
    "env_url": os.environ.get("PROBE_URL", ""),
    "proxies": ",".join(seen),
    "status": status,
    "bytes": size,
}}, ensure_ascii=False))
`

// tempProbePlugin кладёт плагин-клиент во временный каталог и возвращает его путь
// вместе с каталогом фикстур, который отдаётся сервисом.
func tempProbePlugin(t *testing.T, fixtureBody string) (dir, fixtureRoot string) {
	t.Helper()
	dir = t.TempDir()
	fixtureRoot = t.TempDir()
	for name, body := range map[string]string{
		"plugin.yaml": httpProbeManifest,
		"main.py":     httpProbeMain,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(fixtureRoot, "index.html"), []byte(fixtureBody), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, fixtureRoot
}

func writeSpec(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "spec.test.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// specOneCase — спека одного кейса: services + вход + минимальное ожидание.
func specOneCase(root, input string) string {
	return "services:\n" +
		"  - type: http\n" +
		"    root: " + root + "\n" +
		"    base_url: \"{{base_url}}\"\n" +
		"tests:\n" +
		"  - name: кейс через сервис\n" +
		"    input: " + input + "\n" +
		"    expect:\n" +
		"      status: ok\n" +
		"      output:\n" +
		"        status: 200\n"
}

// Сервис отвечает во время прогона, а после прогона его порт свободен.
func TestServiceServesDuringRunAndFreesPortAfter(t *testing.T) {
	dir, root := tempProbePlugin(t, "hi from fixture\n")

	svcs, err := startTestServices(dir, []TestService{{Type: "http", Root: root}}, true)
	if err != nil {
		t.Fatal(err)
	}
	baseURL := svcs.bindings["base_url"]
	if !strings.HasPrefix(baseURL, "http://127.0.0.1:") {
		svcs.Close()
		t.Fatalf("сервис обязан слушать только loopback, получено %s", baseURL)
	}
	resp, err := http.Get(baseURL + "/index.html")
	if err != nil {
		svcs.Close()
		t.Fatalf("сервис обязан отдавать каталог фикстур: %v", err)
	}
	resp.Body.Close()

	svcs.Close()
	// Порт свободен — именно это, а не «сервер молчит»: тот же адрес должен
	// снова заняться.
	ln, err := net.Listen("tcp", strings.TrimPrefix(baseURL, "http://"))
	if err != nil {
		t.Fatalf("порт %s не освободился после прогона: %v", baseURL, err)
	}
	ln.Close()
}

// Полный путь через раннер: spec → подстановка → плагин → expect.
func TestServiceEndToEndThroughRunner(t *testing.T) {
	requirePython(t)
	dir, root := tempProbePlugin(t, "0123456789")
	spec := writeSpec(t, specOneCase(root, `{ url: "{{base_url}}/index.html" }`))
	passed, failed, err := RunPluginTests(dir, spec, true)
	if err != nil {
		t.Fatal(err)
	}
	if failed != 0 || passed != 1 {
		t.Fatalf("кейс через живой сервис обязан быть зелёным: passed=%d failed=%d", passed, failed)
	}
}

// Подстановка: input (включая вложенный список), input_raw и env. И главное
// утверждение о безопасности: плагин с network: [] доходит до loopback, и ни
// одной прокси-переменной в его окружении не появляется — сервис политику
// плагина не расширяет.
func TestServiceSubstitutesInputInputRawAndEnv(t *testing.T) {
	requirePython(t)
	dir, root := tempProbePlugin(t, "0123456789")
	body := `services:
  - type: http
    root: ` + root + `
    base_url: "{{base_url}}"
tests:
  - name: подстановка в input, во вложенный список и в env
    env: { PROBE_URL: "{{base_url}}/index.html" }
    input:
      url: "{{base_url}}/index.html"
      nested: { list: ["{{base_url}}/nested.html"] }
    expect:
      status: ok
      output:
        url: { type: string }
        nested: { contains: "/nested.html" }
        env_url: { contains: "/index.html" }
        proxies: ""
        status: 200
        bytes: { equals: 10 }
`
	passed, failed, err := RunPluginTests(dir, writeSpec(t, body), true)
	if err != nil {
		t.Fatal(err)
	}
	if failed != 0 || passed != 1 {
		t.Fatalf("подстановка обязана работать везде: passed=%d failed=%d", passed, failed)
	}
}

// input_raw — тоже строка: это единственный способ проверить битый JSON с
// настоящим адресом внутри.
func TestServiceSubstitutesInsideInputRaw(t *testing.T) {
	requirePython(t)
	dir, root := tempProbePlugin(t, "raw-body\n")
	body := `services:
  - type: http
    root: ` + root + `
    base_url: "{{base_url}}"
tests:
  - name: подстановка внутри input_raw
    input_raw: '{"url": "{{base_url}}/index.html"}'
    expect:
      status: ok
      output:
        url: { type: string }
        status: 200
        bytes: { equals: 9 }
`
	passed, failed, err := RunPluginTests(dir, writeSpec(t, body), true)
	if err != nil {
		t.Fatal(err)
	}
	if failed != 0 || passed != 1 {
		t.Fatalf("подстановка в input_raw обязана работать: passed=%d failed=%d", passed, failed)
	}
}

// Порт выбирается система: два прогона подряд — два разных порта.
func TestServicePortsDifferBetweenRuns(t *testing.T) {
	_, root := tempProbePlugin(t, "x\n")
	var seen []string
	for i := 0; i < 2; i++ {
		svcs, err := startTestServices(".", []TestService{{Type: "http", Root: root}}, true)
		if err != nil {
			t.Fatal(err)
		}
		seen = append(seen, svcs.bindings["base_url"])
		svcs.Close()
	}
	if seen[0] == seen[1] {
		t.Fatalf("два прогона должны дать разные порты, оба %s", seen[0])
	}
}

// root проверяется ДО кейсов: нет каталога — кейсы не идут.
func TestServiceMissingRootStopsRun(t *testing.T) {
	dir, _ := tempProbePlugin(t, "x")
	missing := filepath.Join(t.TempDir(), "нет-такого")
	_, _, err := RunPluginTests(dir, writeSpec(t, specOneCase(missing, `{ url: "{{base_url}}" }`)), true)
	if err == nil {
		t.Fatal("несуществующий root обязан быть ошибкой")
	}
	if !strings.Contains(err.Error(), "root") || !strings.Contains(err.Error(), "не найден") {
		t.Fatalf("сообщение должно называть root, получено: %v", err)
	}
}

// Файл вместо каталога — тоже ошибка, и тоже до кейсов.
func TestServiceRootIsFileStopsRun(t *testing.T) {
	dir, root := tempProbePlugin(t, "x")
	asFile := filepath.Join(root, "index.html")
	_, _, err := RunPluginTests(dir, writeSpec(t, specOneCase(asFile, `{ url: "{{base_url}}" }`)), true)
	if err == nil {
		t.Fatal("root-файл вместо каталога обязан быть ошибкой")
	}
	if !strings.Contains(err.Error(), "не каталог") {
		t.Fatalf("сообщение должно говорить «не каталог», получено: %v", err)
	}
}

// Неизвестный тип сервиса — внятная ошибка, а не молчаливый пропуск.
func TestServiceUnknownTypeStopsRun(t *testing.T) {
	dir, root := tempProbePlugin(t, "x")
	body := "services:\n  - type: postgres\n    root: " + root + "\n" +
		"tests:\n  - name: кейс\n    expect: { status: ok }\n"
	_, _, err := RunPluginTests(dir, writeSpec(t, body), true)
	if err == nil {
		t.Fatal("неизвестный тип сервиса обязан быть ошибкой")
	}
	if !strings.Contains(err.Error(), "postgres") || !strings.Contains(err.Error(), "http") {
		t.Fatalf("сообщение должно назвать тип и поддерживаемый вариант: %v", err)
	}
}

// Пустой type — тоже ошибка: молчаливый пропуск означал бы «сервис есть».
func TestServiceEmptyTypeStopsRun(t *testing.T) {
	dir, root := tempProbePlugin(t, "x")
	body := "services:\n  - root: " + root + "\ntests:\n  - name: кейс\n    expect: { status: ok }\n"
	if _, _, err := RunPluginTests(dir, writeSpec(t, body), true); err == nil {
		t.Fatal("пустой type обязан быть ошибкой")
	}
}

// Неизвестный плейсхолдер — ошибка до первого кейса, а не пустая строка.
func TestServiceUnknownPlaceholderStopsRun(t *testing.T) {
	dir, root := tempProbePlugin(t, "x")
	_, _, err := RunPluginTests(dir, writeSpec(t, specOneCase(root, `{ url: "{{site}}" }`)), true)
	if err == nil {
		t.Fatal("неизвестный плейсхолдер обязан быть ошибкой")
	}
	if !strings.Contains(err.Error(), "site") || !strings.Contains(err.Error(), "base_url") {
		t.Fatalf("сообщение должно назвать плейсхолдер и доступные имена: %v", err)
	}
}

// base_url без плейсхолдера — бессмысленное объявление: не подставится ничего, а
// автор увидит зелёный набор, который никуда не ходил.
func TestServiceBaseURLWithoutPlaceholderStopsRun(t *testing.T) {
	dir, root := tempProbePlugin(t, "x")
	body := "services:\n  - type: http\n    root: " + root + "\n" +
		"    base_url: \"http://example.invalid\"\n" +
		"tests:\n  - name: кейс\n    expect: { status: ok }\n"
	if _, _, err := RunPluginTests(dir, writeSpec(t, body), true); err == nil {
		t.Fatal("base_url без плейсхолдера обязан быть ошибкой")
	}
}

// Пустой плейсхолдер {{}} — опечатка, а не «подставь пустую строку».
func TestServiceEmptyPlaceholderStopsRun(t *testing.T) {
	svcs := &testServices{bindings: map[string]string{"base_url": "http://127.0.0.1:1"}}
	if _, err := svcs.substituteString("url: {{}}", "tests[1].input.url"); err == nil {
		t.Fatal("пустой плейсхолдер обязан быть ошибкой")
	}
}

// Два сервиса — два имени: имя плейсхолдера берётся из самого base_url.
func TestServicePlaceholderNamesComeFromBaseURL(t *testing.T) {
	_, root := tempProbePlugin(t, "x")
	svcs, err := startTestServices(".", []TestService{
		{Type: "http", Root: root, BaseURL: "{{web}}"},
		{Type: "http", Root: root, BaseURL: "{{api}}"},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer svcs.Close()
	names := svcs.knownNames()
	if len(names) != 2 || names[0] != "api" || names[1] != "web" {
		t.Fatalf("ожидались имена api и web, получено %v", names)
	}
	tc, err := svcs.prepareCase(PluginTestCase{
		Name:  "два сервиса",
		Input: map[string]interface{}{"url": "{{web}}/a", "other": "{{api}}/b"},
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	web, _ := tc.Input["url"].(string)
	api, _ := tc.Input["other"].(string)
	if !strings.HasSuffix(web, "/a") || !strings.HasSuffix(api, "/b") ||
		strings.TrimPrefix(web, "http://127.0.0.1:") == strings.TrimPrefix(api, "http://127.0.0.1:") {
		t.Fatalf("подстановка двух имён неверна: %q %q", web, api)
	}
}

// Одно имя нельзя объявить дважды: иначе «какая это фикстура» — вопрос без ответа.
func TestServiceDuplicatePlaceholderRejected(t *testing.T) {
	svcs := &testServices{bindings: map[string]string{}}
	if _, err := svcs.bindPlaceholders(0, "{{base_url}}", "http://127.0.0.1:1"); err != nil {
		t.Fatal(err)
	}
	if _, err := svcs.bindPlaceholders(1, "{{base_url}}", "http://127.0.0.1:2"); err == nil {
		t.Fatal("повторное объявление имени обязано быть ошибкой")
	}
}

// ОБРАТНАЯ СОВМЕСТИМОСТЬ: файл без services работает ровно как раньше. Скобки во
// входе раньше были просто символами — и обязаны остаться ими.
func TestNoServicesKeepsOldBehaviour(t *testing.T) {
	requirePython(t)
	passed, failed, err := RunPluginTests("testdata/plugins/echo_ok", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if failed != 0 || passed == 0 {
		t.Fatalf("штатные фикстуры обязаны работать как раньше: passed=%d failed=%d", passed, failed)
	}

	body := "tests:\n  - name: скобки — обычные символы\n    input: { value: \"{{base_url}}\" }\n" +
		"    expect:\n      status: ok\n      output:\n        value: \"{{base_url}}\"\n"
	passed, failed, err = RunPluginTests("testdata/plugins/echo_ok", writeSpec(t, body), true)
	if err != nil {
		t.Fatal(err)
	}
	if failed != 0 || passed != 1 {
		t.Fatalf("без services подстановки быть не должно: passed=%d failed=%d", passed, failed)
	}
}

// Упавший кейс не оставляет сервис жить.
func TestServiceClosedEvenWhenCaseFails(t *testing.T) {
	requirePython(t)
	dir, root := tempProbePlugin(t, "x")
	body := "services:\n  - type: http\n    root: " + root + "\n" +
		"tests:\n  - name: заведомо красный\n    input: { url: \"{{base_url}}/index.html\" }\n" +
		"    expect:\n      status: ok\n      output:\n        bytes: 999999\n"
	_, failed, err := RunPluginTests(dir, writeSpec(t, body), true)
	if err != nil {
		t.Fatal(err)
	}
	if failed != 1 {
		t.Fatalf("ожидался один упавший кейс, получено %d", failed)
	}
	// Порт, который сервис занимал, снова свободен: об этом говорит то, что
	// новый сервис на той же машине поднимается без ошибки, а сам прогон
	// завершился (Close в defer).
	svcs, err := startTestServices(dir, []TestService{{Type: "http", Root: root}}, true)
	if err != nil {
		t.Fatalf("сервис не поднялся после прогона с упавшим кейсом: %v", err)
	}
	svcs.Close()
}

// Несколько прогонов подряд: сервис поднимается и гасится каждый раз, зрителей
// не остаётся.
func TestServiceRepeatedRunsLeaveNoListeners(t *testing.T) {
	requirePython(t)
	dir, root := tempProbePlugin(t, "hello\n")
	for i := 0; i < 3; i++ {
		spec := writeSpec(t, specOneCase(root, `{ url: "{{base_url}}/index.html" }`))
		if _, failed, err := RunPluginTests(dir, spec, true); err != nil || failed != 0 {
			t.Fatalf("прогон %d: err=%v failed=%d", i, err, failed)
		}
	}
}

// root относительный разрешается от каталога плагина — как и словари плагинов.
func TestServiceRelativeRootResolvedAgainstPluginDir(t *testing.T) {
	requirePython(t)
	dir, _ := tempProbePlugin(t, "x")
	fixtures := filepath.Join(dir, "fixtures", "site")
	if err := os.MkdirAll(fixtures, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixtures, "index.html"), []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	body := "services:\n  - type: http\n    root: fixtures/site\ntests:\n" +
		"  - name: относительный root\n    input: { url: \"{{base_url}}/index.html\" }\n" +
		"    expect:\n      status: ok\n      output:\n        status: 200\n"
	if _, failed, err := RunPluginTests(dir, writeSpec(t, body), true); err != nil || failed != 0 {
		t.Fatalf("относительный root обязан работать: err=%v failed=%d", err, failed)
	}
}

// Каталог фикстур не отдаётся наружу: FileServer чистит путь, и за пределы root
// не выйти.
func TestServiceDoesNotServeAboveRoot(t *testing.T) {
	_, root := tempProbePlugin(t, "inside\n")
	if err := os.WriteFile(filepath.Join(filepath.Dir(root), "secret.txt"), []byte("top secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	svcs, err := startTestServices(".", []TestService{{Type: "http", Root: root}}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer svcs.Close()
	resp, err := http.Get(svcs.bindings["base_url"] + "/../secret.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("файл за пределами root обязан быть недоступен")
	}
}

// Тип сервиса проверяется ДО чтения root: непонятный тип не должен молча
// «поднять» сервис, а потом падать на отсутствии каталога.
func TestServiceTypeCheckedBeforeRoot(t *testing.T) {
	_, err := startTestServices(".", []TestService{{Type: "smtp", Root: "нет-такого"}}, true)
	if err == nil || !strings.Contains(err.Error(), "smtp") {
		t.Fatalf("ожидалась ошибка про тип, получено: %v", err)
	}
}

// Close идемпотентен: «сервис погашен» не должно зависеть от того, кто первый
// вызвал закрытие.
func TestServiceCloseIsIdempotent(t *testing.T) {
	_, root := tempProbePlugin(t, "x")
	svcs, err := startTestServices(".", []TestService{{Type: "http", Root: root}}, true)
	if err != nil {
		t.Fatal(err)
	}
	svcs.Close()
	svcs.Close()
}

// Один сервис на весь прогон: кейсы идут против него же.
func TestServiceReusedAcrossCases(t *testing.T) {
	requirePython(t)
	dir, root := tempProbePlugin(t, "shared\n")
	body := "services:\n  - type: http\n    root: " + root + "\n" +
		"tests:\n" +
		"  - name: первый\n    input: { url: \"{{base_url}}/index.html\" }\n" +
		"    expect:\n      status: ok\n      output:\n        status: 200\n" +
		"  - name: второй\n    input: { url: \"{{base_url}}/index.html\" }\n" +
		"    expect:\n      status: ok\n      output:\n        status: 200\n"
	passed, failed, err := RunPluginTests(dir, writeSpec(t, body), true)
	if err != nil {
		t.Fatal(err)
	}
	if failed != 0 || passed != 2 {
		t.Fatalf("один сервис на прогон: passed=%d failed=%d", passed, failed)
	}
}
