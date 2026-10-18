package api

import (
	"time"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/wykserdex/wedra/internal/core"
	"github.com/wykserdex/wedra/internal/gate"
	"github.com/wykserdex/wedra/internal/plugin"
	"github.com/wykserdex/wedra/web"
)

// Version — версия бинарника. var (не const): release-воркфлоу переопределяет
// через ldflags -X из тега сборки; фолбэк — текущая версия для локальных сборок.

const maxRequestBodySize = 8 << 20

// P2 F-03: стабильные потолки ответов /api/runs и /api/runs/<id>. Журнал
// рана ничем не ограничен, а эндпоинты читали его целиком и кодировали в
// ответ без предела: RAM и тело ответа росли вместе с журналом. Теперь тело
// ограничено по событиям и байтам, а обрезание честно помечается
// truncated=true (старые поля ответа сохранены).
const (
	maxRunResponseEvents = 20000    // событий журнала в ответе
	maxRunResponseBytes  = 16 << 20 // байт событий журнала в ответе
	maxRunsListed        = 200      // ранов в /api/runs
)

type Server struct {
	PluginsDir   string
	PipelinesDir string
	// AssetsDir — куда ложатся загруженные файлы (фото, CSV). Сосед
	// каталога пайплайнов: всё рабочее рядом, а не в %TEMP%.
	AssetsDir string
	RunsDir   string
	Engine    *plugin.Engine
	// Trusted — allow-list доверенных плагинов. Используется и списком
	// (/api/plugins), и запуском ранов: одно и то же решение о доверии, иначе
	// список показывал бы «доверен», а ран — падал бы (или наоборот).
	// nil = никто не доверен.
	Trusted *plugin.AllowList

	// v0.22: in-process запуск из GUI — один ран за раз
	runMu sync.Mutex

	// v0.24: ожидающие браузерные гейты активных ранов: runID → ChannelUI.
	// Заполняется лениво (когда ран доходит до gate-шага), чистится при выходе.
	gatesMu sync.Mutex
	gates   map[string]*gate.ChannelUI

	// v0.9: отмена активных ранов: runID → cancel (POST /api/runs/<id>/cancel).
	cancelsMu sync.Mutex
	cancels   map[string]context.CancelFunc

	// v0.9: одноразовый код обмена (EnableSession) и живые сессии. Пустой
	// PairingCode — сессия не требуется (--no-session, встраивание, тесты).
	PairingCode string

	// TrustConfigPath — КАК ИМЕННО оператор задал доверие: путь к
	// wedra-trust.yaml, который реально выбран (флаг, переменная, каталог
	// бинаря) или пустая строка, если выбран встроенный allow-list.
	//
	// Это не деталь реализации: без него редактор плагинов может только
	// сказать «плагин не доверен», а человеку на Windows неизвестно, куда
	// положить файл, чтобы он заработал. Пустой путь — само по себе полезный
	// ответ: значит доверять можно только флагом или переменной окружения.
	TrustConfigPath string

	// StaticDir — каталог с фронтендом (index.html, app.js, editor/).
	// Пусто = встроенный (go:embed) фронтенд. Раньше каталог web/static
	// подхватывался автоматически, если был виден ИЗ ТЕКУЩЕГО КАТАЛОГА, и это
	// делало отдаваемый фронтенд свойством места запуска: подложенный
	// ./web/static/index.html подменял GUI, а после входа этот JS жил в origin
	// с полной сессией. Теперь dev-режим включается только явным флагом.
	StaticDir string

	// H4: чем сервер объявляет себя наружу (allow-list Host, внешняя схема,
	// доверие к прокси, имя cookie и TTL сессии). Наполняется ConfigureListen;
	// пустая политика = loopback на любом порту, http, без прокси.
	policy ListenPolicy

	sessionMu           sync.Mutex
	sessions            map[string]sessionEntry
	pairingUsed         bool
	pairingExpires      time.Time
	pairingFailures     int
	pairingBlockedUntil time.Time

	summaryMu    sync.Mutex
	summaryCache map[string]summaryCacheEntry
}

type summaryCacheEntry struct {
	modTime time.Time
	size    int64
	value   map[string]interface{}
}

// defaultAssetsDir — каталог загруженных файлов рядом с пайплайнами:
// pipelines/ и assets/ окажутся соседями, если pipelinesDir — <корень>/pipes.
func defaultAssetsDir(pipelinesDir string) string {
	abs, err := filepath.Abs(pipelinesDir)
	if err != nil {
		return filepath.Join("assets")
	}
	return filepath.Join(filepath.Dir(abs), "assets")
}

func NewServer(pluginsDir, pipelinesDir, runsDir string) *Server {
	eng := plugin.NewEngine()
	// v0.9: один резолв плагинов для validate/plan/list и run (раньше
	// validate жил с дефолтным "plugins", а run — с PluginsDir)
	eng.PluginsDir = pluginsDir
	srv := &Server{
		PluginsDir:   pluginsDir,
		PipelinesDir: pipelinesDir,
		AssetsDir:    defaultAssetsDir(pipelinesDir),
		RunsDir:      runsDir,
		Engine:       eng,
		// Дефолт — встроенный allow-list (пины реестра), а не пустой: иначе
		// GUI-консоль перестала бы запускать официальные плагины на хостах без
		// изолятора. Конфиг оператора поверх него подкладывает вызывающий
		// (cmd/gui.go, cmd/mcp.go).
		Trusted:      plugin.BuiltinAllowList(),
		gates:        map[string]*gate.ChannelUI{},
		cancels:      map[string]context.CancelFunc{},
		summaryCache: map[string]summaryCacheEntry{},
		sessions:     map[string]sessionEntry{},
	}
	// Политика по умолчанию: loopback на любом порту, http, без прокси. Порт
	// неизвестен до Listen, поэтому cookie без суффикса и cookieName() допишет
	// порт, как только вызовут ConfigureListen (все три точки входа — вызывают).
	policy, err := NewListenPolicy(ListenOptions{Addr: "127.0.0.1:0"})
	if err != nil {
		panic("api: политика прослушивания по умолчанию не собралась: " + err.Error())
	}
	srv.policy = policy
	// v0.34: закрыть раны, оборванные вместе с процессом. Статус рана выводится
	// из журнала, и без run_end он навсегда остаётся «running» — а меню первым
	// пунктом предлагает «открыть таймлайн» того, что уже не существует.
	if closed := reconcileInterruptedRuns(srv.RunsDir); closed > 0 {
		log.Printf("сверка ранов: %d прервано процессом, помечены как interrupted", closed)
	}
	return srv
}

// reconcileInterruptedRuns дописывает итоговый run_end с interrupted: true в
// те раны, у которых итогового события нет. Отдельная отметка, а не aborted:
// обрыв это не отмена человеком. Возвращает, сколько ранов закрыто.
func reconcileInterruptedRuns(runsDir string) int {
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		return 0
	}
	closed := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(runsDir, e.Name())
		path := filepath.Join(dir, "journal.jsonl")
		raw, err := os.ReadFile(path)
		if err != nil || len(raw) == 0 {
			continue
		}
		hasEnd, lastTS, hasStart := scanForRunEnd(raw)
		if hasEnd || !hasStart {
			continue
		}
		ev := map[string]interface{}{
			"type": "run_end", "ts": lastTS, "status": "interrupted", "interrupted": true,
		}
		line, err := json.Marshal(ev)
		if err != nil {
			continue
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			continue
		}
		_, werr := f.Write(append(line, '\n'))
		_ = f.Close()
		if werr == nil {
			closed++
		}
	}
	return closed
}

func scanForRunEnd(raw []byte) (hasEnd bool, lastTS string, hasStart bool) {
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev struct {
			Type string `json:"type"`
			TS   string `json:"ts"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		if ev.TS != "" {
			lastTS = ev.TS
		}
		switch ev.Type {
		case "run_end":
			hasEnd = true
		case "run_start", "run_resumed":
			hasStart = true
		}
	}
	return hasEnd, lastTS, hasStart
}

// ConfigureListen — объявить, как сервер виден снаружи. Обязателен для любого
// входа, который поднимает HTTP: без него имя cookie не знает порт, а
// allow-list остаётся «loopback на любом порту».
//
// Вызывать ПОСЛЕ net.Listen (адрес с портом 0 становится конкретным только там)
// и ДО первого обслуженного запроса: политика читается обработчиками без
// синхронизации. ListenOptions проходит ту же проверку, что и CLI
// (ValidateListen), поэтому не-loopback адрес без --allow-remote отвергается и
// здесь.
func (s *Server) ConfigureListen(opts ListenOptions) error {
	policy, err := NewListenPolicy(opts)
	if err != nil {
		return err
	}
	s.policy = policy
	return nil
}

func (s *Server) HTTPServer(addr string) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           s.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
}

// runEngine — свежий движок для рана (без кэша манифестов прошлых ранов).
func (s *Server) runEngine() *core.Engine {
	eng := core.NewEngine()
	eng.PluginsDir = s.PluginsDir
	return eng
}

func secureContainedPath(root, name string) (string, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	candidate := name
	if !filepath.IsAbs(candidate) && !strings.HasPrefix(candidate, "/") && !(len(candidate) >= 2 && candidate[1] == ':') {
		candidate = filepath.Join(rootAbs, candidate)
	}
	rel, err := filepath.Rel(rootAbs, candidate)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path вне корня")
	}
	rootReal, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return "", err
	}
	if info, lstatErr := os.Lstat(candidate); lstatErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("path является symlink")
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err == nil {
		resolvedRel, relErr := filepath.Rel(rootReal, resolved)
		if relErr != nil || resolvedRel == ".." || strings.HasPrefix(resolvedRel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("path уходит через symlink")
		}
		return resolved, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	if info, lstatErr := os.Lstat(candidate); lstatErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("path является symlink")
	} else if lstatErr != nil && !os.IsNotExist(lstatErr) {
		return "", lstatErr
	}
	parentReal, err := filepath.EvalSymlinks(filepath.Dir(candidate))
	if err != nil {
		return "", err
	}
	parentRel, err := filepath.Rel(rootReal, parentReal)
	if err != nil || parentRel == ".." || strings.HasPrefix(parentRel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("родительский путь уходит через symlink")
	}
	return candidate, nil
}

func (s *Server) setCancel(id string, c context.CancelFunc) {
	s.cancelsMu.Lock()
	defer s.cancelsMu.Unlock()
	s.cancels[id] = c
}

func (s *Server) clearCancel(id string) {
	s.cancelsMu.Lock()
	defer s.cancelsMu.Unlock()
	delete(s.cancels, id)
}

func (s *Server) cancelFor(id string) context.CancelFunc {
	s.cancelsMu.Lock()
	defer s.cancelsMu.Unlock()
	return s.cancels[id]
}

func (s *Server) setGate(id string, ui *gate.ChannelUI) {
	s.gatesMu.Lock()
	defer s.gatesMu.Unlock()
	s.gates[id] = ui
}

func (s *Server) clearGate(id string) {
	s.gatesMu.Lock()
	defer s.gatesMu.Unlock()
	if ui, ok := s.gates[id]; ok {
		ui.Close() // если ран умер с ожидающим гейтом — не висит в памяти
		delete(s.gates, id)
	}
}

func (s *Server) gateFor(id string) *gate.ChannelUI {
	s.gatesMu.Lock()
	defer s.gatesMu.Unlock()
	return s.gates[id]
}

// csrfGuard — v0.28a, переработано в H4: защита POST/PUT/PATCH/DELETE от
// cross-site форм (<form> с чужого сайта запускает пайплайны с --yes).
// Same-site не равен same-origin: Origin проверяется и для loopback, а запрос
// без браузерных заголовков остаётся совместимым с curl CI.
//
// Что изменилось: ожидаемая сторона больше НЕ берётся из того же запроса.
// Раньше csrfRequestHost() отдавал r.Host (или X-Forwarded-Host от кого
// угодно), и Origin сравнивался с ним — то есть сравнивался сам с собой: Name
// «evil.com» в Origin и Host проходили проверку, стоило только объявить
// правильный Host. Теперь Origin сверяется с политикой прослушивания:
// allow-list Host (сервер объявляет их сам) + внешняя схема.
func (s *Server) csrfGuard(w http.ResponseWriter, r *http.Request) bool {
	site := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")))
	switch site {
	case "", "same-origin", "none":
	case "cross-site", "same-site":
		http.Error(w, "cross-site request запрещён (CSRF)", http.StatusForbidden)
		return false
	default:
		http.Error(w, "неизвестный Sec-Fetch-Site (CSRF)", http.StatusForbidden)
		return false
	}

	if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" {
		if !s.policy.originAllowed(origin) {
			http.Error(w, "Origin не совпадает с объявленным адресом (CSRF)", http.StatusForbidden)
			return false
		}
		return true
	}
	if referer := strings.TrimSpace(r.Header.Get("Referer")); referer != "" && !s.policy.originAllowed(referer) {
		http.Error(w, "Referer не совпадает с объявленным адресом (CSRF)", http.StatusForbidden)
		return false
	}
	return true
}

// publicAPI — пути под /api/*, открытые БЕЗ сессии человека. Список закрытый и
// задан здесь одним местом: всё остальное под /api/* требует cookie (журналы,
// входы и выходы шагов, плагины, пайплайны, статусы гейта). Новый эндпоинт
// поэтому по умолчанию защищён, а не наоборот.
var publicAPI = map[string]string{
	"/api/health": "живость и версия: ни журналов, ни входов/выходов шагов; нужен мониторингу " +
		"и `curl -sf /api/health` без входа в GUI",
	"/api/session": "обмен одноразового кода из терминала на cookie и проверка «есть ли уже " +
		"сессия»; без самого кода отдаёт только {\"authenticated\":false}",
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc("/api/session", s.handleSession)
	mux.HandleFunc("/api/trust", s.handleTrust)
	mux.HandleFunc("/api/plugins", s.handlePluginsRoot)
	mux.HandleFunc("/api/plugins/", s.handlePluginSubtree)
	mux.HandleFunc("/api/plugins-install-deps", s.handlePluginInstallDeps)
	mux.HandleFunc("/api/pipelines", s.handlePipelines)
	mux.HandleFunc("/api/pipelines/", s.handlePipelineDetail)
	mux.HandleFunc("/api/presets", s.handlePresets)
	// v0.39: магазин плагинов. Витрина читает реестр и меряет готовность тем
	// же doctor'ом, установка — реальная запись в plugins/, поэтому только POST
	// и всегда за сессией (в publicAPI не добавляется).
	mux.HandleFunc("/api/store", s.handleStore)
	mux.HandleFunc("/api/store/install", s.handleStoreInstall)
	// v0.38: загруженные артефакты для редактора. Сессия и CSRF —
	// из обёртки Routes(), как и у остального /api/*.
	mux.HandleFunc("/api/assets", s.handleAssets)
	mux.HandleFunc("/api/runs", s.handleRuns)
	mux.HandleFunc("/api/runs/", s.handleRunDetail)
	mux.HandleFunc("/api/run", s.handleRunStart)
	mux.HandleFunc("/api/validate/pipeline", s.handleValidatePipeline)
	mux.HandleFunc("/api/plan/pipeline", s.handlePlanPipeline)
	// v0.25: редактор — парсинг/сериализация через ядро (JS не держит YAML)
	mux.HandleFunc("/api/parse/pipeline", s.handleParsePipeline)
	mux.HandleFunc("/api/serialize/pipeline", s.handleSerializePipeline)
	// static frontend — v0.7: GUI вшит в бинарник (go:embed, package web).
	// Frontend с диска отдаётся ТОЛЬКО когда каталог назван явно (StaticDir,
	// флаг `wedra gui --static=<dir>`): иначе отдаваемое содержимое зависело бы
	// от места запуска, а не от того, что собрано в бинарник.
	if s.StaticDir != "" {
		mux.Handle("/", http.FileServer(http.Dir(s.StaticDir)))
	} else {
		static, err := fs.Sub(web.FS, "static")
		if err != nil {
			// недостижимо: fs.Sub по паттерну embedded-константы не падает;
			// фолбэк — честная ошибка вместо «GUI postponed»
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "GUI assets unavailable: "+err.Error(), 500)
			})
		} else {
			mux.Handle("/", http.FileServer(http.FS(static)))
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 1. allow-list Host — до всего, включая статику и обмен кода на
		//    cookie. Иначе DNS-rebinding (чужой домен → 127.0.0.1) проходит
		//    дальше и читает журналы, входы и выходы шагов.
		if !s.policy.hostAllowed(r.Host) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(403)
			w.Write([]byte(`{"error":"Host не в списке разрешённых для этого сервера (DNS-rebinding?)","code":"E_HOST_NOT_ALLOWED"}` + "\n"))
			return
		}
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)
		}
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		// 2. ?c=<одноразовый код> — вход в GUI одним кликом (cookie + redirect).
		if s.sessionHandshake(w, r) {
			return
		}
		// 3. сессия человека — на ВСЁ под /api/*, кроме перечисленного в
		//    publicAPI. Раньше cookie проверялся только на мутациях, и все GET
		//    (журналы, входы и выходы шагов, плагины, пайплайны, статус гейта)
		//    отдавались любому, кто дотянулся до порта.
		if strings.HasPrefix(r.URL.Path, "/api/") {
			if _, open := publicAPI[r.URL.Path]; !open && !s.requireSession(w, r) {
				return
			}
		}
		// 4. CSRF на мутациях — здесь, а не в каждом обработчике.
		if (r.Method == "POST" || r.Method == "PUT" || r.Method == "PATCH" || r.Method == "DELETE") && !s.csrfGuard(w, r) {
			return
		}
		mux.ServeHTTP(w, r)
	})
}

