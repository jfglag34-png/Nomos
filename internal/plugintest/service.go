package plugintest

// Декларативная подготовка окружения для контракт-тестов: локальный HTTP-сервер
// над каталогом фикстур, к которому плагин под тестом ходит настоящим сетевым
// запросом.
//
// Зачем. Из плагинов, которым `wedra doctor` пишет needs_external_tool, большая
// часть HTTP-зависима (arjun, dirsearch, wapiti, wfuzz, xsstrike). Правила
// `wedra plugin test` давали только name/env/input/input_raw/timeout/expect —
// поднять сервер было нечем, поэтому тесты держались на моках. Проверено
// практически: донор против закрытого порта печатает Skipped и не создаёт
// выходной файл, то есть «зелёный» набор проверял бы только мок.
//
// Границы, которые здесь держатся намеренно:
//   - сервер слушает ТОЛЬКО 127.0.0.1 и всегда на свободном порту (0);
//   - сервис НЕ трогает сетевую политику плагина: не добавляет HTTP(S)_PROXY, не
//     правит permissions, не меняет решение о доверии. Плагин под тестом идёт с
//     той же политикой, что и файл без services; loopback для allow-listed
//     каталога и так разрешён, и расширять его нечем;
//   - произвольной команды здесь нет и не будет (см. TestService);
//   - root проверяется ДО старта кейсов, неизвестный тип сервиса и неизвестный
//     плейсхолдер — ошибки, а не молчаливый пропуск.
//
// Что осталось за границей: симлинк внутри root обслуживается (http.Dir его
// разыменует), то есть фикстура может отдать файл за пределами каталога.
// Считать это дырой нельзя: каталог плагина с симлинком вообще не проходит
// проверку доверия (trustlist.go, ErrDigestUnavailable), а `wedra plugin test`
// запускает только тот каталог, который оператор назвал руками. Отдельного
// запрета здесь нет намеренно: он был бы запретом на полезное (общая папка
// фикстур), а не на опасное.

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// defaultBaseURL — имя плейсхолдера, если поле base_url не задано.
const defaultBaseURL = "{{base_url}}"

// placeholderRe — любой {{...}}. Намеренно снисходительный: неизвестный
// плейсхолдер должен дать внятную ошибку, а не тихо остаться в строке.
var placeholderRe = regexp.MustCompile(`\{\{([^{}]*)\}\}`)

// testServices — поднятые сервисы и то, что они разрешили подставлять.
type testServices struct {
	bindings map[string]string // имя плейсхолдера → реальный URL
	stoppers []func()          // в обратном порядке при Close
}

// startTestServices поднимает объявленные сервисы. Любая ошибка — до кейсов:
// файл с несуществующим root или незнакомым типом не должен «прогнаться
// частично».
func startTestServices(dir string, list []TestService, quiet bool) (*testServices, error) {
	svcs := &testServices{bindings: map[string]string{}}
	if len(list) == 0 {
		return svcs, nil // файл без services — поведение ровно прежнее
	}
	for i, svc := range list {
		if err := svcs.start(dir, i, svc, quiet); err != nil {
			svcs.Close()
			return nil, err
		}
	}
	return svcs, nil
}

func (s *testServices) start(dir string, idx int, svc TestService, quiet bool) error {
	switch strings.TrimSpace(svc.Type) {
	case "http":
	case "":
		return fmt.Errorf("services[%d]: type обязателен (сейчас поддерживается только http)", idx)
	default:
		return fmt.Errorf("services[%d]: type %q не поддерживается — сейчас только http", idx, svc.Type)
	}

	root, err := serviceRoot(dir, idx, svc.Root)
	if err != nil {
		return err
	}

	// Слушаем только loopback и только свободный порт: 0.0.0.0 здесь был бы
	// публикацией фикстур наружу, а фиксированный порт — гонкой с чужим.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("services[%d]: не поднялся локальный сервер: %w", idx, err)
	}
	if host, _, splitErr := net.SplitHostPort(ln.Addr().String()); splitErr != nil {
		_ = ln.Close()
		return fmt.Errorf("services[%d]: сервер слушает %s, адрес не разобран", idx, ln.Addr())
	} else if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		_ = ln.Close()
		return fmt.Errorf("services[%d]: сервер слушает %s, а не loopback — это дыра, а не сервис", idx, host)
	}

	srv := &http.Server{
		Handler: http.FileServer(http.Dir(root)),
		// Ошибки протокола уходят в log по умолчанию и портили бы вывод тестов.
		ErrorLog:          log.New(io.Discard, "", 0),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	// Close, а не Shutdown: после кейсов активных запросов быть не должно, а
	// Close закрывает слушатель и все соединения — порт освобождается сразу.
	s.stoppers = append(s.stoppers, func() { _ = srv.Close() })

	baseURL := "http://" + ln.Addr().String()
	names, err := s.bindPlaceholders(idx, svc.BaseURL, baseURL)
	if err != nil {
		return err
	}
	if !quiet {
		fmt.Printf("  ℹ сервис http: %s → %s (%s)\n", strings.Join(names, ", "), baseURL, root)
	}
	return nil
}

// serviceRoot проверяет каталог фикстур ДО кейсов: несуществующий root или файл
// вместо каталога — внятная ошибка, а не пустой результат, который выглядит как
// «сервис ничего не отдал».
func serviceRoot(dir string, idx int, root string) (string, error) {
	raw := strings.TrimSpace(root)
	if raw == "" {
		return "", fmt.Errorf("services[%d]: root обязателен (каталог, который отдаётся)", idx)
	}
	abs := raw
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(dir, abs)
	}
	abs = filepath.Clean(abs)
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("services[%d]: root %q не найден (%s) — проверяется до кейсов", idx, raw, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("services[%d]: root %q — не каталог", idx, raw)
	}
	return abs, nil
}

// bindPlaceholders — имена, под которые отдаётся URL сервиса. Имя берётся из
// самого шаблона base_url: `base_url: "{{base_url}}"` объявляет плейсхолдер
// base_url. Без плейсхолдера объявление бессмысленно (тихо ничего не подставит
// никто), поэтому это ошибка.
func (s *testServices) bindPlaceholders(idx int, tmpl, baseURL string) ([]string, error) {
	if strings.TrimSpace(tmpl) == "" {
		tmpl = defaultBaseURL
	}
	matches := placeholderRe.FindAllStringSubmatch(tmpl, -1)
	if len(matches) == 0 {
		return nil, fmt.Errorf("services[%d]: base_url %q не содержит плейсхолдера вида {{имя}} — "+
			"объявление ничего не подставит", idx, tmpl)
	}
	var names []string
	for _, m := range matches {
		name := strings.TrimSpace(m[1])
		if name == "" {
			return nil, fmt.Errorf("services[%d]: в base_url пустой плейсхолдер {{}}", idx)
		}
		if _, busy := s.bindings[name]; busy {
			return nil, fmt.Errorf("services[%d]: плейсхолдер {{%s}} объявлен дважды", idx, name)
		}
		s.bindings[name] = baseURL
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// Close гасит сервисы. Вызывается в defer, поэтому сервер не переживает прогон
// даже тогда, когда кейс упал.
func (s *testServices) Close() {
	for i := len(s.stoppers) - 1; i >= 0; i-- {
		s.stoppers[i]()
	}
	s.stoppers = nil
}

// prepareCase — копия кейса с подставленными значениями. Ошибка здесь означает
// плохой файл, а не упавший кейс, поэтому prepare зовётся для ВСЕХ кейсов до
// запуска первого.
func (s *testServices) prepareCase(tc PluginTestCase, num int) (PluginTestCase, error) {
	where := fmt.Sprintf("tests[%d]", num)
	if name := strings.TrimSpace(tc.Name); name != "" {
		where += " (" + name + ")"
	}
	in, err := s.substituteValue(tc.Input, where+".input")
	if err != nil {
		return PluginTestCase{}, err
	}
	raw, err := s.substituteString(tc.InputRaw, where+".input_raw")
	if err != nil {
		return PluginTestCase{}, err
	}
	env := make(map[string]string, len(tc.Env))
	for k, v := range tc.Env {
		sub, err := s.substituteString(v, where+".env."+k)
		if err != nil {
			return PluginTestCase{}, err
		}
		env[k] = sub
	}
	out := tc
	if in != nil {
		out.Input, _ = in.(map[string]interface{})
	}
	out.InputRaw = raw
	out.Env = env
	return out, nil
}

// substituteValue идёт по значению целиком: вложенные map и списки —
// подстановка должна доставать до строк на любой глубине, потому что вход
// плагина может быть объектом в объекте.
func (s *testServices) substituteValue(v interface{}, where string) (interface{}, error) {
	switch x := v.(type) {
	case string:
		return s.substituteString(x, where)
	case map[string]interface{}:
		for k, item := range x {
			sub, err := s.substituteValue(item, where+"."+k)
			if err != nil {
				return nil, err
			}
			x[k] = sub
		}
		return x, nil
	case []interface{}:
		for i, item := range x {
			sub, err := s.substituteValue(item, fmt.Sprintf("%s[%d]", where, i))
			if err != nil {
				return nil, err
			}
			x[i] = sub
		}
		return x, nil
	}
	return v, nil
}

// substituteString — замена {{имя}} на реальный URL. Неизвестное имя — ошибка:
// пустая строка была бы «зелёным» тестом, который проверяет ничего.
//
// Если сервисов нет, строки не трогаются вообще: скобки в файле без services —
// это обычные символы, и в этом файле плагина они могут быть его содержимым
// (шаблоны, mustache, {{base_url}} в тексте примера). Проверка идёт по числу
// объявленных сервисов, а не по наличию скобок.
func (s *testServices) substituteString(text, where string) (string, error) {
	if len(s.bindings) == 0 || !strings.Contains(text, "{{") {
		return text, nil
	}
	var b strings.Builder
	last := 0
	for _, loc := range placeholderRe.FindAllStringSubmatchIndex(text, -1) {
		name := strings.TrimSpace(text[loc[2]:loc[3]])
		value, known := s.bindings[name]
		if !known {
			return "", fmt.Errorf("%s: неизвестный плейсхолдер {{%s}} (доступны: %s)",
				where, name, strings.Join(s.knownNames(), ", "))
		}
		b.WriteString(text[last:loc[0]])
		b.WriteString(value)
		last = loc[1]
	}
	b.WriteString(text[last:])
	return b.String(), nil
}

func (s *testServices) knownNames() []string {
	names := make([]string, 0, len(s.bindings))
	for name := range s.bindings {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
