package api

// H4: HTTP-поверхность GUI. Здесь собраны проверки, которых раньше не было
// вообще: сессия на ВСЕМ /api/*, allow-list Host (DNS-rebinding), недоверие к
// X-Forwarded-*, имя cookie с портом и отказ слушать не-loopback без явного
// флага. Плюс главная регрессия: одностраничный сценарий «открыл GUI — редактор
// работает» обязан продолжать работать.

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// testAddr — адрес, который тестовые серверы объявляютallow-list'ом.
const testAddr = "127.0.0.1:8765"

// h4Server — сервер в tempdir с включённой сессией и объявленным адресом.
// Возвращает хендлер, код входа и каталоги.
func h4Server(t *testing.T, opts ListenOptions, code string) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	plugins := filepath.Join(dir, "plugins")
	pipelines := filepath.Join(dir, "pipelines")
	runs := filepath.Join(dir, "runs")
	for _, d := range []string{plugins, pipelines, runs} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(pipelines, "gate_demo.yaml"), []byte(gatePipeYAML), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pipelines, "saved.yaml"), []byte(gatePipeYAML), 0644); err != nil {
		t.Fatal(err)
	}
	if opts.Addr == "" {
		opts.Addr = testAddr
	}
	srv := NewServer(plugins, pipelines, runs)
	if err := srv.ConfigureListen(opts); err != nil {
		t.Fatal(err)
	}
	if code != "" {
		srv.EnableSession(code)
	}
	return srv, dir
}

func h4Request(t *testing.T, srv *Server, method, target string, body io.Reader, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req, err := http.NewRequest(method, "http://"+testAddr+target, body)
	if err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	return rec
}

// login — обмен одноразового кода на cookie (как это делает браузер).
func login(t *testing.T, srv *Server, code string) *http.Cookie {
	t.Helper()
	rec := h4Request(t, srv, "POST", "/api/session", strings.NewReader(`{"code":"`+code+`"}`), nil)
	if rec.Code != 200 {
		t.Fatalf("обмен кода на cookie: code=%d body=%s", rec.Code, rec.Body.String())
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("обмен кода: cookies=%#v (ждём ровно одну)", cookies)
	}
	return cookies[0]
}

// TestSessionRequiredOnEveryAPIRead — ГЛАВНОЕ ПОВЕДЕНИЕ H4. Все GET под
// /api/* без cookie → 401. Раньше cookie проверялся только на мутациях, и
// журналы, входы и выходы шагов, плагины, пайплайны и статус гейта отдавались
// любому, кто дотянулся до порта. Открыты остаются ровно два пути, и они
// перечислены в publicAPI.
func TestSessionRequiredOnEveryAPIRead(t *testing.T) {
	srv, _ := h4Server(t, ListenOptions{Addr: testAddr}, "AAAA-BBBB-CCCC")

	// прогон, чтобы было что читать: входы/выходы в журнале
	runRec := h4Request(t, srv, "POST", "/api/run",
		strings.NewReader(`{"file":"gate_demo.yaml","yes":true}`), nil)
	if runRec.Code != 401 {
		t.Fatalf("POST /api/run без cookie: code=%d (want 401)", runRec.Code)
	}
	c := login(t, srv, "AAAA-BBBB-CCCC")
	withCookie := func(r *http.Request) { r.AddCookie(c) }
	rec := h4Request(t, srv, "POST", "/api/run", strings.NewReader(`{"file":"gate_demo.yaml","yes":true}`), withCookie)
	if rec.Code != 202 {
		t.Fatalf("POST /api/run с cookie: code=%d body=%s (want 202)", rec.Code, rec.Body.String())
	}
	var started map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	runID, _ := started["run"].(string)
	if runID == "" {
		t.Fatalf("202 без run: %s", rec.Body.String())
	}

	// все эндпоинты чтения: без cookie 401, с cookie — не 401
	reads := []string{
		"/api/health",
		"/api/session",
		"/api/plugins",
		"/api/plugins/core/human_gate",
		"/api/pipelines",
		"/api/pipelines/gate_demo.yaml",
		"/api/runs",
		"/api/runs/" + runID,
		"/api/runs/" + runID + "/journal?since=0",
		"/api/runs/" + runID + "/gate",
		"/api/store",
		"/api/unknown-endpoint",
	}
	for _, path := range reads {
		rec := h4Request(t, srv, "GET", path, nil, nil)
		want := 401
		if path == "/api/health" || path == "/api/session" {
			want = 200 // перечислены в publicAPI: без данных о ранах
		}
		if rec.Code != want {
			t.Errorf("GET %s без cookie: code=%d (want %d), body=%s", path, rec.Code, want, rec.Body.String())
		}
		if want == 401 && !strings.Contains(rec.Body.String(), "E_SESSION_REQUIRED") {
			t.Errorf("GET %s: нет кода E_SESSION_REQUIRED: %s", path, rec.Body.String())
		}
	}

	// то же самое с cookie — данные отдаются
	for _, path := range []string{"/api/runs", "/api/runs/" + runID, "/api/runs/" + runID + "/gate", "/api/pipelines"} {
		rec := h4Request(t, srv, "GET", path, nil, withCookie)
		if rec.Code == 401 || rec.Code == 403 {
			t.Fatalf("GET %s с cookie: code=%d body=%s", path, rec.Code, rec.Body.String())
		}
	}
}

// TestHostAllowListRejectsForeignHost — DNS-rebinding: браузер со страницы
// чужого домена шлёт `Host: чужой.домен`, а запрос идёт на 127.0.0.1. Раньше
// такой Host принимался и отдавал журналы.
func TestHostAllowListRejectsForeignHost(t *testing.T) {
	srv, _ := h4Server(t, ListenOptions{Addr: testAddr}, "AAAA-BBBB-CCCC")
	c := login(t, srv, "AAAA-BBBB-CCCC")

	// даже с cookie — чужой Host отвергается
	rec := h4Request(t, srv, "GET", "/api/runs", nil, func(r *http.Request) {
		r.Host = "evil.example"
		r.AddCookie(c)
	})
	if rec.Code != 403 || !strings.Contains(rec.Body.String(), "E_HOST_NOT_ALLOWED") {
		t.Fatalf("Host: evil.example: code=%d body=%s (want 403 E_HOST_NOT_ALLOWED)", rec.Code, rec.Body.String())
	}
	// без cookie — тоже 403, а не 401: до сессии дело не дошло
	rec = h4Request(t, srv, "GET", "/api/runs", nil, func(r *http.Request) { r.Host = "evil.example" })
	if rec.Code != 403 {
		t.Fatalf("Host: evil.example без cookie: code=%d (want 403)", rec.Code)
	}
	// статика тоже (иначе GUI грузится с чужого домена, хоть и пустой)
	rec = h4Request(t, srv, "GET", "/", nil, func(r *http.Request) { r.Host = "evil.example" })
	if rec.Code != 403 {
		t.Fatalf("чужой Host на /: code=%d (want 403)", rec.Code)
	}
	// свои Host — принимаются
	for _, host := range []string{"127.0.0.1:8765", "localhost:8765", "[::1]:8765", "127.0.0.1"} {
		rec := h4Request(t, srv, "GET", "/api/health", nil, func(r *http.Request) { r.Host = host })
		if rec.Code != 200 {
			t.Errorf("Host %s: code=%d (want 200)", host, rec.Code)
		}
	}
}

// TestForwardedProtoIgnoredWithoutTrustedProxy — X-Forwarded-* от клиента не
// даёт доверия: без --trusted-proxy cookie не становится Secure, а Origin со
// схемой https не проходит проверку.
func TestForwardedProtoIgnoredWithoutTrustedProxy(t *testing.T) {
	srv, _ := h4Server(t, ListenOptions{Addr: testAddr}, "AAAA-BBBB-CCCC")
	rec := h4Request(t, srv, "POST", "/api/session", strings.NewReader(`{"code":"AAAA-BBBB-CCCC"}`),
		func(r *http.Request) {
			r.Header.Set("X-Forwarded-Proto", "https")
			r.Header.Set("X-Forwarded-Host", "evil.example")
		})
	if rec.Code != 200 {
		t.Fatalf("обмен кода: code=%d body=%s", rec.Code, rec.Body.String())
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies=%#v", cookies)
	}
	if cookies[0].Secure {
		t.Fatal("cookie стал Secure по X-Forwarded-Proto клиента без --trusted-proxy")
	}
	if !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie: %#v (ждём HttpOnly + SameSite=Strict)", cookies[0])
	}
	if cookies[0].MaxAge <= 0 {
		t.Fatalf("у cookie нет TTL: %#v", cookies[0])
	}
	// имя cookie содержит порт (иначе два инстанса на localhost затирают друг
	// друга: cookie в браузере портом не различаются)
	if !strings.HasSuffix(cookies[0].Name, "_8765") {
		t.Fatalf("имя cookie без порта: %q", cookies[0].Name)
	}
	// Origin https://localhost — не наш, пока оператор не сказал, что схема https
	srv.RotatePairingCode("BBBB-BBBB-BBBB")
	cookie := login(t, srv, "BBBB-BBBB-BBBB")
	rec = h4Request(t, srv, "POST", "/api/serialize/pipeline", bytes.NewReader([]byte(`{"name":"t","input":[],"steps":[],"unsupported":[]}`)),
		func(r *http.Request) {
			r.Header.Set("Origin", "https://localhost:8765")
			r.Header.Set("X-Forwarded-Proto", "https")
			r.AddCookie(cookie)
		})
	if rec.Code != 403 {
		t.Fatalf("Origin https с XFP без --trusted-proxy: code=%d (want 403), body=%s", rec.Code, rec.Body.String())
	}
	// а с --trusted-proxy — Secure и Origin проходит
	srv2, _ := h4Server(t, ListenOptions{Addr: testAddr, AllowRemote: true, TrustedProxy: true, PublicHosts: []string{"localhost"}}, "AAAA-BBBB-CCCC")
	rec = h4Request(t, srv2, "POST", "/api/session", strings.NewReader(`{"code":"AAAA-BBBB-CCCC"}`), nil)
	if rec.Code != 200 {
		t.Fatalf("обмен кода (--trusted-proxy): code=%d body=%s", rec.Code, rec.Body.String())
	}
	cookies = rec.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].Secure {
		t.Fatalf("с --trusted-proxy cookie должен быть Secure: %#v", cookies)
	}
}

// TestSessionCodeIsSingleUse — код одноразовый: второй браузер по старому коду
// не входит. В этом и смысл: то, что попало в терминал/скроллбек, перестаёт
// работать сразу после первого входа.
func TestSessionCodeIsSingleUse(t *testing.T) {
	srv, _ := h4Server(t, ListenOptions{Addr: testAddr}, "AAAA-BBBB-CCCC")
	if login(t, srv, "AAAA-BBBB-CCCC") == nil {
		t.Fatal("нет cookie")
	}
	rec := h4Request(t, srv, "POST", "/api/session", strings.NewReader(`{"code":"AAAA-BBBB-CCCC"}`), nil)
	if rec.Code != 401 {
		t.Fatalf("повторный обмен того же кода: code=%d (want 401), body=%s", rec.Code, rec.Body.String())
	}
	// новый код (Enter в терминале / новый гейт в mcp) — работает
	srv.RotatePairingCode("ZZZZ-ZZZZ-ZZZZ")
	if login(t, srv, "ZZZZ-ZZZZ-ZZZZ") == nil {
		t.Fatal("новый код не сработал")
	}
	// перепечатка кода человеком (заглавные/дефисы/пробелы) совпадает
	srv3, _ := h4Server(t, ListenOptions{Addr: testAddr}, "WWWW-WWWW-WWWW")
	rec = h4Request(t, srv3, "POST", "/api/session", strings.NewReader(`{"code":"wwww wwww wwww"}`), nil)
	if rec.Code != 200 {
		t.Fatalf("код с пробелами/нижним регистром: code=%d body=%s", rec.Code, rec.Body.String())
	}
	// неверный код — 401 с кодом контракта
	srv4, _ := h4Server(t, ListenOptions{Addr: testAddr}, "QQQQ-QQQQ-QQQQ")
	rec = h4Request(t, srv4, "POST", "/api/session", strings.NewReader(`{"code":"XXXX-XXXX-XXXX"}`), nil)
	if rec.Code != 401 || !strings.Contains(rec.Body.String(), "E_SESSION_CODE_INVALID") {
		t.Fatalf("неверный код: code=%d body=%s", rec.Code, rec.Body.String())
	}
	// перебор закрывается (иначе локальный процесс подбирает код)
	for i := 0; i < maxPairingFailures; i++ {
		h4Request(t, srv4, "POST", "/api/session", strings.NewReader(`{"code":"XXXX-XXXX-XXXX"}`), nil)
	}
	rec = h4Request(t, srv4, "POST", "/api/session", strings.NewReader(`{"code":"QQQQ-QQQQ-QQQQ"}`), nil)
	if rec.Code != 429 {
		t.Fatalf("после перебора: code=%d (want 429), body=%s", rec.Code, rec.Body.String())
	}
}

// TestSessionRotateKeepsLiveSessions — ротация кода не выкидывает живые сессии.
//
// Раньше RotatePairingCode вызывал EnableSession, который очищал карту сессий:
// вход второго браузера убивал первый, причём ровно в момент, когда человеку
// нужно было одобрить гейт (mcp выдаёт свежий код на каждое ожидание).
func TestSessionRotateKeepsLiveSessions(t *testing.T) {
	srv, _ := h4Server(t, ListenOptions{Addr: testAddr}, "AAAA-BBBB-CCCC")
	first := login(t, srv, "AAAA-BBBB-CCCC")

	srv.RotatePairingCode("ZZZZ-ZZZZ-ZZZZ")

	rec := h4Request(t, srv, "GET", "/api/runs", nil, func(r *http.Request) { r.AddCookie(first) })
	if rec.Code != 200 {
		t.Fatalf("старая сессия после ротации кода: code=%d (want 200), body=%s", rec.Code, rec.Body.String())
	}
	if login(t, srv, "ZZZZ-ZZZZ-ZZZZ") == nil {
		t.Fatal("новый код не сработал")
	}
	rec = h4Request(t, srv, "POST", "/api/session", strings.NewReader(`{"code":"AAAA-BBBB-CCCC"}`), nil)
	if rec.Code != 401 {
		t.Fatalf("старый код после ротации: code=%d (want 401), body=%s", rec.Code, rec.Body.String())
	}
}

// TestTwoPortsDoNotClobberSession — имя cookie включает порт, и токены двух
// инстансов независимы. Живой вариант (с настоящими слушателями и одним
// cookie jar) — TestTwoLiveInstancesBelow.
func TestTwoPortsDoNotClobberSession(t *testing.T) {
	code := "PPPP-PPPP-PPPP"
	first, _ := h4Server(t, ListenOptions{Addr: "127.0.0.1:8765"}, code)
	second, _ := h4Server(t, ListenOptions{Addr: "127.0.0.1:8766"}, code)

	c1 := login(t, first, code)
	c2 := login(t, second, code)
	if c1.Name == c2.Name {
		t.Fatalf("имена cookie совпали: %q — сессии затирают друг друга", c1.Name)
	}
	if c1.Value == c2.Value {
		t.Fatal("токены сессии совпали: это один и тот же секрет, а не независимые сессии")
	}
	// cookie первого не подходит второму и наоборот (серверы разных людей)
	rec := h4Request(t, second, "GET", "/api/runs", nil, func(r *http.Request) { r.AddCookie(c1) })
	if rec.Code != 401 {
		t.Fatalf("cookie инстанса 8765 на инстансе 8766: code=%d (want 401)", rec.Code)
	}
}

// TestTwoLiveInstances — два ЖИВЫХ инстанса на соседних портах и один
// cookie jar: вход в первый не выкидывает второй из сессии.
func TestTwoLiveInstancesBelow(t *testing.T) {
	first, _ := h4Server(t, ListenOptions{Addr: "127.0.0.1:0"}, "1111-1111-1111")
	second, _ := h4Server(t, ListenOptions{Addr: "127.0.0.1:0"}, "2222-2222-2222")

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar}

	start := func(srv *Server) *httptest.Server {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		_, port, err := net.SplitHostPort(ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		if err := srv.ConfigureListen(ListenOptions{Addr: "127.0.0.1:" + port, ResolvedPort: port}); err != nil {
			t.Fatal(err)
		}
		ts := &httptest.Server{Listener: ln, Config: &http.Server{Handler: srv.Routes()}}
		ts.Start()
		t.Cleanup(ts.Close)
		return ts
	}
	ts1, ts2 := start(first), start(second)

	// вход в первый
	exchange := func(ts *httptest.Server, code string) {
		resp, err := client.Post(ts.URL+"/api/session", "application/json", strings.NewReader(`{"code":"`+code+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("обмен кода на %s: code=%d body=%s", ts.URL, resp.StatusCode, body)
		}
	}
	exchange(ts1, "1111-1111-1111")
	// вход во второй — jar теперь хранит ДВА cookie с разными именами
	exchange(ts2, "2222-2222-2222")

	// оба остаются залогинены
	for _, ts := range []*httptest.Server{ts1, ts2} {
		resp, err := client.Get(ts.URL + "/api/runs")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("GET /api/runs на %s после входа в оба: code=%d body=%s", ts.URL, resp.StatusCode, body)
		}
	}
}

// TestListenNonLoopbackRequiresExplicitFlag — `--listen 0.0.0.0` без
// --allow-remote отклоняется, и это проверяется БЕЗ поднятия сервера.
func TestListenNonLoopbackRequiresExplicitFlag(t *testing.T) {
	cases := []struct {
		name    string
		opts    ListenOptions
		wantErr string
	}{
		{"wildcard без флага", ListenOptions{Addr: "0.0.0.0:8765"}, "--allow-remote"},
		{"ipv6 wildcard без флага", ListenOptions{Addr: "[::]:8765"}, "--allow-remote"},
		{"lan-ip без флага", ListenOptions{Addr: "192.168.1.10:8765"}, "--allow-remote"},
		{"пустой host без флага", ListenOptions{Addr: ":8765"}, "--allow-remote"},
		{"имя без флага", ListenOptions{Addr: "example.internal:8765"}, "--allow-remote"},
		{"wildcard без public-host", ListenOptions{Addr: "0.0.0.0:8765", AllowRemote: true}, "--public-host"},
		{"lan-ip без public-host нельзя", ListenOptions{Addr: "192.168.1.10:8765", AllowRemote: true}, ""},
		{"no-session + внешний", ListenOptions{Addr: "0.0.0.0:8765", AllowRemote: true, NoSession: true, PublicHosts: []string{"1.2.3.4"}}, "--no-session"},
		{"public-host без allow-remote", ListenOptions{Addr: "127.0.0.1:8765", PublicHosts: []string{"1.2.3.4"}}, "--public-host"},
		{"без порта", ListenOptions{Addr: "127.0.0.1"}, "host:port"},
		{"мусорный порт", ListenOptions{Addr: "127.0.0.1:abc"}, "порт"},
		{"плохая схема", ListenOptions{Addr: "127.0.0.1:8765", ExternalScheme: "ftp"}, "--origin-scheme"},
		{"loopback без флагов", ListenOptions{Addr: "127.0.0.1:8765"}, ""},
		{"localhost без флагов", ListenOptions{Addr: "localhost:8765"}, ""},
		{"порт 0 без флагов", ListenOptions{Addr: "127.0.0.1:0"}, ""},
		{"внешний с public-host", ListenOptions{Addr: "0.0.0.0:8765", AllowRemote: true, PublicHosts: []string{"192.168.1.10"}}, ""},
		{"внешний с именем", ListenOptions{Addr: "0.0.0.0:8765", AllowRemote: true, PublicHosts: []string{"wedra.lan:8765"}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateListen(tc.opts)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ожидался запуск, получено: %v", err)
				}
				if _, perr := NewListenPolicy(tc.opts); perr != nil {
					t.Fatalf("NewListenPolicy: %v", perr)
				}
				return
			}
			if err == nil {
				t.Fatalf("ожидался отказ (%s), сервер бы запустился", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("отказ без упоминания %q: %v", tc.wantErr, err)
			}
		})
	}
	// тот же отказ на самом сервере (не только в проверке флагов)
	srv, _ := h4Server(t, ListenOptions{Addr: testAddr}, "")
	if err := srv.ConfigureListen(ListenOptions{Addr: "0.0.0.0:8765"}); err == nil {
		t.Fatal("ConfigureListen принял 0.0.0.0 без --allow-remote")
	}
}

// TestPipelineSaved0600 — пайплайны сохраняются 0600, а не 0644.
//
// На Windows POSIX-биты не хранятся (WriteFile даёт 0666, Chmod — только
// read-only), поэтому проверка прав имеет смысл только на POSIX; CI гоняет её
// на Linux и macOS. На Windows отдельного контроля нет: файл лежит в каталоге
// пользователя, и это известное ограничение, а не повод писать 0644.
func TestPipelineSaved0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows не хранит POSIX-биты прав: проверка прав бессмысленна")
	}
	srv, _ := h4Server(t, ListenOptions{Addr: testAddr}, "AAAA-BBBB-CCCC")
	c := login(t, srv, "AAAA-BBBB-CCCC")
	rec := h4Request(t, srv, "PUT", "/api/pipelines/saved.yaml", strings.NewReader(gatePipeYAML), func(r *http.Request) {
		r.AddCookie(c)
	})
	if rec.Code != 200 {
		t.Fatalf("PUT пайплайна: code=%d body=%s", rec.Code, rec.Body.String())
	}
	info, err := os.Stat(filepath.Join(srv.PipelinesDir, "saved.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0600 {
		t.Fatalf("права пайплайна = %o, ждём 600", mode)
	}
}

// TestOnePageScenarioStillWorks — НЕ ЛОМАЕМ РАБОТУ: человек открыл GUI в
// браузере, обменял код на cookie, и редактор/консоль живут. Ровно тот путь, по
// которому ходит web/static (fetch с cookie same-origin).
func TestOnePageScenarioStillWorks(t *testing.T) {
	// TTL сессии короче обычного — проверяем в отдельном тесте, здесь дефолтный.
	srv, _ := h4Server(t, ListenOptions{Addr: testAddr}, "ABCD-EFGH-JKMN")
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)
	// порт httptest неизвестен заранее — объявляем его (имя cookie с портом)
	host, port, err := net.SplitHostPort(strings.TrimPrefix(ts.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.ConfigureListen(ListenOptions{Addr: net.JoinHostPort(host, port), ResolvedPort: port}); err != nil {
		t.Fatal(err)
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar}

	// 1) открыли / — статика отдаётся без cookie (иначе GUI не откроется вовсе)
	resp, err := client.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "WEDRA") {
		t.Fatalf("GET / без сессии: code=%d, тело=%q", resp.StatusCode, truncate(string(body)))
	}
	resp, err = client.Get(ts.URL + "/editor/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /editor/ без сессии: code=%d, тело=%q", resp.StatusCode, truncate(string(body)))
	}

	// 2) страница спрашивает «есть ли сессия» — открытый путь, отвечает честно
	resp, err = client.Get(ts.URL + "/api/session")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"authenticated":false`) {
		t.Fatalf("GET /api/session: code=%d body=%s", resp.StatusCode, body)
	}

	// 3) человек вводит код — обмен на cookie
	resp, err = client.Post(ts.URL+"/api/session", "application/json", strings.NewReader(`{"code":"ABCD-EFGH-JKMN"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("ввод кода: code=%d", resp.StatusCode)
	}
	if len(jar.Cookies(resp.Request.URL)) == 0 {
		t.Fatal("cookie не сохранился в jar браузера")
	}

	// 4) дальше всё как раньше: списки, деталка, редактор
	for _, path := range []string{"/api/health", "/api/runs", "/api/pipelines", "/api/plugins"} {
		resp, err := client.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("GET %s после входа: code=%d body=%s", path, resp.StatusCode, truncate(string(body)))
		}
	}
	// 5) редактор: parse → serialize → save
	yamlBody := "format_version: \"0.2\"\npipeline:\n  name: one_page\n  steps:\n    - id: g\n      plugin: core/human_gate\n      actions: [accept]\n      on_reject: stop\n"
	resp, err = client.Post(ts.URL+"/api/parse/pipeline", "text/yaml", strings.NewReader(yamlBody))
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"name":"one_page"`) {
		t.Fatalf("parse: code=%d body=%s", resp.StatusCode, truncate(string(body)))
	}
	req, _ := http.NewRequest("PUT", ts.URL+"/api/pipelines/one_page.yaml", strings.NewReader(yamlBody))
	req.Header.Set("Origin", ts.URL)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("PUT пайплайна из браузера: code=%d body=%s", resp.StatusCode, truncate(string(body)))
	}
	// 6) запуск рана из браузера
	req, _ = http.NewRequest("POST", ts.URL+"/api/run", strings.NewReader(`{"file":"gate_demo.yaml","yes":true}`))
	req.Header.Set("Origin", ts.URL)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 202 {
		t.Fatalf("POST /api/run из браузера: code=%d body=%s", resp.StatusCode, truncate(string(body)))
	}
	var started map[string]interface{}
	if err := json.Unmarshal(body, &started); err != nil {
		t.Fatal(err)
	}
	runID, _ := started["run"].(string)
	// ждём, пока ран допишет журнал: иначе TempDir снимут из-под работающего
	// рана (тест падает на мусоре в каталоге, а не на своей логике)
	deadline := time.Now().Add(30 * time.Second)
	for {
		r, err := client.Get(ts.URL + "/api/runs/" + runID)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(r.Body)
		r.Body.Close()
		var d map[string]interface{}
		_ = json.Unmarshal(raw, &d)
		if st, _ := d["status"].(string); st != "" && st != "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ран %s не завершился за 30 c", runID)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestSessionHasTTL — у cookie есть срок, и сервер его проверяет. Раньше cookie
// была вечным секретом: ни TTL, ни возможности отозвать.
//
// TTL=1ns выбран намеренно вместо «живой» проверки со sleep: такой cookie
// истекает уже на следующем запросе, и тест не зависит от нагрузки на машину.
// Что TTL вообще передаётся клиенту, проверяет TestForwardedProtoIgnored…:
// там у cookie MaxAge>0 (12 ч) и заполненный Expires.
func TestSessionHasTTL(t *testing.T) {
	srv, _ := h4Server(t, ListenOptions{Addr: testAddr, SessionTTL: time.Nanosecond}, "ABCD-EFGH-JKMN")
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)
	host, port, err := net.SplitHostPort(strings.TrimPrefix(ts.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.ConfigureListen(ListenOptions{
		Addr: net.JoinHostPort(host, port), ResolvedPort: port, SessionTTL: time.Nanosecond,
	}); err != nil {
		t.Fatal(err)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar}
	resp, err := client.Post(ts.URL+"/api/session", "application/json", strings.NewReader(`{"code":"ABCD-EFGH-JKMN"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("вход по коду: code=%d", resp.StatusCode)
	}
	// даже если клиент cookie прислал, сервер её не признаёт
	req, _ := http.NewRequest("GET", ts.URL+"/api/runs", nil)
	for _, ck := range jar.Cookies(resp.Request.URL) {
		req.AddCookie(ck)
	}
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != 401 {
		t.Fatalf("сессия с истёкшим TTL принята: code=%d (want 401)", r2.StatusCode)
	}
}

// TestNoSessionModeOpensEverything — --no-session остаётся (честный отказ от
// защиты), это видно по /api/session, и на внешнем адресе он запрещён.
func TestNoSessionModeOpensEverything(t *testing.T) {
	srv, _ := h4Server(t, ListenOptions{Addr: testAddr}, "")
	rec := h4Request(t, srv, "GET", "/api/runs", nil, nil)
	if rec.Code != 200 {
		t.Fatalf("--no-session: /api/runs code=%d (want 200)", rec.Code)
	}
	// и GUI честно говорит, что вход не треб��ется
	rec = h4Request(t, srv, "GET", "/api/session", nil, nil)
	var st map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st["required"] != false || st["authenticated"] != true {
		t.Fatalf("--no-session: /api/session = %v (ждём required:false, authenticated:true)", st)
	}
}

func truncate(s string) string {
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}
