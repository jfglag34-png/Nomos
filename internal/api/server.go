package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wykserdex/wedra/internal/buildinfo"
	"github.com/wykserdex/wedra/internal/core"
	"github.com/wykserdex/wedra/internal/execution"
	"github.com/wykserdex/wedra/internal/gate"
	"github.com/wykserdex/wedra/internal/journal"
	"github.com/wykserdex/wedra/internal/pipeline"
	"github.com/wykserdex/wedra/internal/plugin"
	"github.com/wykserdex/wedra/internal/registry"
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
	// Только в памяти процесса; в cookie — токен с TTL, не секрет.
	PairingCode string

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
	mux.HandleFunc("/api/plugins", s.handlePlugins)
	mux.HandleFunc("/api/plugins/", s.handlePluginDetail)
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

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ver := buildinfo.Resolve()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "version": ver, "protocol": "0.2"})
}

// networkJSON — permissions.network в нижних ключах для UI (v0.5: struct
// NetworkPermission без json-тегов, иначе Host/Port/AnyHost с капсом).
func networkJSON(list []pipeline.NetworkPermission) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(list))
	for _, np := range list {
		out = append(out, map[string]interface{}{
			"host": np.Host, "port": np.Port,
			"any_host": np.AnyHost, "note": np.Note,
		})
	}
	return out
}

func (s *Server) handlePlugins(w http.ResponseWriter, r *http.Request) {
	dirs := []string{}
	// agent-plugins обходится вместе с остальными. До 0.33a каталог не был
	// виден НИ В ОДНОМ списке, хотя плагин из него резолвился по ссылке:
	// агент мог вызвать то, что человек не видел. Хуже всего, что на хостах
	// без изолятора такой плагин и запустить нельзя — поэтому рядом с
	// плагином идёт причина, а не только флаг.
	for _, base := range []string{
		s.PluginsDir,
		filepath.Join(s.PluginsDir, "official"),
		filepath.Join(s.PluginsDir, "community"),
		filepath.Join(s.PluginsDir, plugin.AgentPluginDir),
	} {
		if _, err := os.Stat(base); err != nil {
			continue
		}
		entries, _ := os.ReadDir(base)
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			dir := filepath.Join(base, e.Name())
			if _, err := os.Stat(filepath.Join(dir, "plugin.yaml")); err == nil {
				dirs = append(dirs, dir)
			}
		}
	}
	seen := map[string]bool{}
	list := make([]map[string]interface{}, 0)
	for _, d := range dirs {
		m, err := s.Engine.LoadManifest(d)
		if err != nil {
			continue
		}
		if seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		item := map[string]interface{}{
			"id": m.ID, "version": m.Version, "description": m.Description, "author": m.Author,
			"dir": d, "runtime": m.Runtime.Type, "input": m.Input, "output": m.Output,
			"permissions": map[string]interface{}{
				"network":    networkJSON(m.Permissions.Network),
				"filesystem": m.Permissions.Filesystem,
				"secrets":    m.Permissions.Secrets,
			},
		}
		// Доверие решает ядро по хэшу содержимого, а не поле манифеста, поэтому
		// и список, и ран обязаны считать его ОДИНАКОВО. Показывать «доверен»
		// там, где ран уведёт плагина в песочницу (или упадёт), — значит
		// подсунуть человеку кнопку, которая не сработает.
		decision := plugin.DecideTrust(m, plugin.TrustPolicy{Trusted: s.Trusted})
		item["trusted"] = decision.Trusted
		if decision.Digest != "" {
			item["content_sha256"] = decision.Digest
		}
		if plugin.IsAgentWrittenPlugin(m) {
			item["agent_written"] = true
		}
		// Причина — у ЛЮБОГО недоверенного плагина, а не только у написанного
		// агентом: после инверсии внешним кодом стал любой плагин, которого нет
		// в allow-list. Молчать об этом — значит показать плагин и не сказать,
		// что запустить его нельзя.
		if !decision.Trusted {
			item["blocked_reason"] = blockedReason(decision.Reason)
		}
		list = append(list, item)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list)
}

// blockedReason — человекочитаемое объяснение, почему плагин не запустится на
// этой машине прямо сейчас.
//
// Формулировка зависит от наличия изолятора, но суть одна: нужен изолятор.
// Раньше текст был привязан к «плагин написан агентом», и после инверсии это
// стало лишь одним из частных случаев — поэтому причина теперь начинается с
// вердикта ядра, а изолятор добавляется как условие запуска.
func blockedReason(decisionReason string) string {
	if !plugin.SandboxUsable() {
		return decisionReason + ": для внешнего кода нужен изолятор, а на " + runtime.GOOS +
			" его нет (запуск возможен на Linux через bwrap)"
	}
	return decisionReason + ": запуск только в изоляторе и только с флагом --allow-untrusted-plugins"
}

func (s *Server) handlePluginDetail(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/plugins/")
	if id == "" {
		http.Error(w, "missing id", 400)
		return
	}
	for _, base := range []string{s.PluginsDir, filepath.Join(s.PluginsDir, "official"), filepath.Join(s.PluginsDir, "community"), filepath.Join(s.PluginsDir, plugin.AgentPluginDir)} {
		entries, _ := os.ReadDir(base)
		for _, e := range entries {
			dir := filepath.Join(base, e.Name())
			m, err := s.Engine.LoadManifest(dir)
			if err != nil {
				continue
			}
			if m.ID == id || e.Name() == id {
				// Те же поля доверия, что в списке: детальный вид обязан
				// говорить то же, что список, иначе оператор видит причину
				// блокировки в одном месте и не видит в другом.
				item := map[string]interface{}{
					"id": m.ID, "version": m.Version, "description": m.Description, "author": m.Author,
					"dir": dir, "runtime": m.Runtime, "input": m.Input, "output": m.Output, "permissions": m.Permissions,
				}
				decision := plugin.DecideTrust(m, plugin.TrustPolicy{Trusted: s.Trusted})
				item["trusted"] = decision.Trusted
				if decision.Digest != "" {
					item["content_sha256"] = decision.Digest
				}
				if plugin.IsAgentWrittenPlugin(m) {
					item["agent_written"] = true
				}
				if !decision.Trusted {
					item["blocked_reason"] = blockedReason(decision.Reason)
				}
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(item)
				return
			}
		}
	}
	http.Error(w, "not found", 404)
}

// handlePluginInstallDeps — POST /api/plugins-install-deps?id=<id>.
// Ставит pip-зависимости плагина из его runtime.requires (только точные пины
// package==version) тем же интерпретатором, которым плагины запускаются.
// Тело ответа: {ok, installed[], output} или {ok:false, error, output}.
func (s *Server) handlePluginInstallDeps(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "только POST", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" || strings.ContainsAny(id, `/\.`) {
		http.Error(w, "нужен id плагина", 400)
		return
	}
	var dir string
	for _, base := range []string{s.PluginsDir, filepath.Join(s.PluginsDir, "official"), filepath.Join(s.PluginsDir, "community"), filepath.Join(s.PluginsDir, plugin.AgentPluginDir)} {
		entries, _ := os.ReadDir(base)
		for _, e := range entries {
			d := filepath.Join(base, e.Name())
			m, err := s.Engine.LoadManifest(d)
			if err != nil {
				continue
			}
			if m.ID == id || e.Name() == id {
				dir = d
				break
			}
		}
		if dir != "" {
			break
		}
	}
	if dir == "" {
		http.Error(w, "плагин не найден", 404)
		return
	}
	m, err := s.Engine.LoadManifest(dir)
	if err != nil {
		http.Error(w, "манифест не читается: "+err.Error(), http.StatusUnprocessableEntity)
		return
	}
	if len(m.Runtime.Requires) == 0 {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "installed": []string{}, "output": "зависимостей не объявлено (runtime.requires пуст)"})
		return
	}
	out, err := plugin.PipInstall(m.Runtime.Requires)
	resp := map[string]interface{}{"installed": m.Runtime.Requires, "output": out}
	if err != nil {
		resp["ok"] = false
		resp["error"] = err.Error()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		json.NewEncoder(w).Encode(resp)
		return
	}
	resp["ok"] = true
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) handlePipelines(w http.ResponseWriter, r *http.Request) {
	entries, _ := os.ReadDir(s.PipelinesDir)
	var list []map[string]interface{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		path := filepath.Join(s.PipelinesDir, e.Name())
		pf, err := pipeline.LoadPipelineFile(path)
		if err != nil {
			list = append(list, map[string]interface{}{"file": e.Name(), "error": err.Error()})
			continue
		}
		list = append(list, map[string]interface{}{"file": e.Name(), "name": pf.Pipeline.Name, "steps": len(pf.Pipeline.Steps), "foreach": pf.Pipeline.Foreach})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list)
}

// handlePresets — готовые сценарии из реестра (/api/presets).
//
// Меню консоли начинается с вопроса «с чего начать», и на этот вопрос нужен
// список того, что уже сделано за тебя: пресеты реестра с описанием.
//
// Реестр лежит в корне репозитория, а PluginsDir — его подкаталог, поэтому
// корень восстанавливается обратным шагом. Если реестра нет (каталог без
// исходников, как в mcp-режиме), это НЕ ошибка: отдаём пустой список с
// причиной, чтобы меню показывало «реестр недоступен», а не 500.
func (s *Server) handlePresets(w http.ResponseWriter, r *http.Request) {
	root := filepath.Dir(s.PluginsDir)
	if _, err := os.Stat(filepath.Join(root, registry.RegistryFile)); err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"presets": []interface{}{}, "available": false,
			"reason": "реестр не найден: " + registry.RegistryFile,
		})
		return
	}
	handle, err := registry.Load(root)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"presets": []interface{}{}, "available": false, "reason": err.Error(),
		})
		return
	}
	list := make([]map[string]interface{}, 0, len(handle.Registry.Presets))
	for name, entry := range handle.Registry.Presets {
		file := filepath.Base(entry.Path)
		// installed — лежит ли пресет рядом, в каталоге пайплайнов. Иначе
		// меню предлагает «установить», а не «открыть».
		_, localErr := os.Stat(filepath.Join(s.PipelinesDir, file))
		list = append(list, map[string]interface{}{
			"name": name, "description": entry.Description,
			"file": file, "path": entry.Path,
			"version": entry.Version, "commit": entry.Commit,
			"installed": localErr == nil,
		})
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i]["name"].(string) < list[j]["name"].(string)
	})
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"presets": list, "available": true,
		"plugins": len(handle.Registry.Plugins),
	})
}

func (s *Server) handlePipelineDetail(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/api/pipelines/")
	if name == "" {
		http.Error(w, "missing file", 400)
		return
	}
	if !strings.HasSuffix(name, ".yaml") {
		name += ".yaml"
	}
	path, err := secureContainedPath(s.PipelinesDir, name)
	if err != nil {
		http.Error(w, "file: только имя из PipelinesDir (traversal запрещён)", 400)
		return
	}
	if r.Method == "GET" {
		raw, err := os.ReadFile(path)
		if err != nil {
			http.Error(w, err.Error(), 404)
			return
		}
		w.Header().Set("Content-Type", "application/yaml")
		w.Write(raw)
		return
	}
	if r.Method == "PUT" {
		// H4: сессия человека проверена в Routes() (там же CSRF)
		// v0.12 fix: раньше читал старый файл и писал его же обратно — молчаливая порча данных
		// теперь читаем r.Body и валидируем перед сохранением
		data, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body: "+err.Error(), 400)
			return
		}
		pf, err := pipeline.LoadPipelineFileFromBytes(data)
		if err != nil {
			http.Error(w, "invalid yaml: "+err.Error(), 400)
			return
		}
		issues := pipeline.ValidateIssues(pf, s.Engine)
		errs, _ := pipeline.SplitIssues(issues)
		if len(errs) > 0 {
			writeJSON(w, 400, map[string]interface{}{"ok": false, "issues": issues, "errors": errs})
			return
		}
		// H4: 0600, а не 0644. Пайплайн может содержать и секреты (pipeline.secrets
		// — имена env-ключей), и входы, и пути; читать его должен только
		// пользователь. WriteFile режим применяет только к НОВОМУ файлу, поэтому
		// для уже существующего (например созданного старой версией с 0644)
		// права подтягиваются явно.
		if err := os.WriteFile(path, data, 0600); err != nil {
			http.Error(w, "write: "+err.Error(), 500)
			return
		}
		if err := os.Chmod(path, 0600); err != nil {
			http.Error(w, "chmod: "+err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "saved", "file": name})
		return
	}
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

func summarizeRun(dir string, events []map[string]interface{}) map[string]interface{} {
	acc := newRunSummary()
	for _, e := range events {
		acc.add(eventMetaOf(e))
	}
	return acc.summary(dir, len(events), false)
}

func newRunSummary() *runSummaryAcc {
	return &runSummaryAcc{status: "running"}
}

// eventMetaOf — проекция разобранного события в EventMeta, чтобы summary по
// срезу и по потоку считались одним кодом.
func eventMetaOf(e map[string]interface{}) journal.EventMeta {
	meta := journal.EventMeta{}
	if ts, ok := e["ts"].(string); ok {
		meta.TS = ts
	}
	if typ, ok := e["type"].(string); ok {
		meta.Type = typ
	}
	if pipeline, ok := e["pipeline"].(string); ok {
		meta.Pipeline = pipeline
	}
	if status, ok := e["status"].(string); ok {
		meta.Status = status
	}
	if aborted, ok := e["aborted"].(float64); ok {
		meta.Aborted = &aborted
	}
	return meta
}

// runSummaryAcc — накопитель summary рана по событиям журнала.
type runSummaryAcc struct {
	pipeline string
	status   string
	started  string
	last     string
	steps    int
}

func (a *runSummaryAcc) add(meta journal.EventMeta) {
	if meta.TS != "" {
		if a.started == "" {
			a.started = meta.TS
		}
		a.last = meta.TS
	}
	switch meta.Type {
	case "run_start":
		a.pipeline = meta.Pipeline
	case "run_resumed":
		a.status = "running"
	case "step_end", "step_skipped", "step_failed":
		a.steps++
	case "run_end":
		a.status = "ok"
		if meta.Aborted != nil && *meta.Aborted > 0 {
			a.status = "aborted"
		}
		// v0.34: обрыв процесса, а не отмена. Раньше такие раны навсегда
		// оставались «running», и главное меню предлагало открыть тот, что
		// никогда не закончится.
		if meta.Interrupted != nil && *meta.Interrupted {
			a.status = "interrupted"
		}
	case "run_failed":
		a.status = "failed"
	case "run_cancelled":
		a.status = "cancelled"
	}
}

func (a *runSummaryAcc) summary(dir string, total int, truncated bool) map[string]interface{} {
	return map[string]interface{}{
		"id": filepath.Base(dir), "dir": dir, "pipeline": a.pipeline,
		"status": a.status, "steps": a.steps, "events": total,
		"started": a.started, "last": a.last, "truncated": truncated,
	}
}

func cloneSummary(in map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (s *Server) cachedRunSummary(dir string) map[string]interface{} {
	path := filepath.Join(dir, "journal.jsonl")
	info, err := os.Stat(path)
	if err != nil {
		return runSummary(dir)
	}
	s.summaryMu.Lock()
	entry, ok := s.summaryCache[dir]
	s.summaryMu.Unlock()
	if ok && entry.size == info.Size() && entry.modTime.Equal(info.ModTime()) {
		return cloneSummary(entry.value)
	}
	summary := runSummary(dir)
	s.summaryMu.Lock()
	if s.summaryCache == nil {
		s.summaryCache = map[string]summaryCacheEntry{}
	}
	s.summaryCache[dir] = summaryCacheEntry{modTime: info.ModTime(), size: info.Size(), value: cloneSummary(summary)}
	s.summaryMu.Unlock()
	return summary
}

// runSummary — summary рана потоковым проходом по журналу (P2 F-03): память
// O(1), поэтому список ранов не материализует журнал каждого рана. Статус и
// счётчики считаются по ВСЕМ событиям; truncated=true означает, что проход
// упёрся в потолок ScanMeta (журнал вырожденно большой).
func runSummary(dir string) map[string]interface{} {
	acc := newRunSummary()
	res, err := journal.NewReader(dir).ScanMeta(func(meta journal.EventMeta) error {
		acc.add(meta)
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return acc.summary(dir, res.Total, true)
	}
	return acc.summary(dir, res.Total, res.Truncated)
}

func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	// v0.22: filesystem — источник правды (весь вар/runs), runs.db — только
	// дополняет (artifacts; старые индексы дособираются)
	var ids []string
	seen := map[string]bool{}
	entries, _ := os.ReadDir(s.RunsDir)
	for _, e := range entries {
		if e.IsDir() {
			ids = append(ids, e.Name())
			seen[e.Name()] = true
		}
	}
	var artsMap map[string][]string
	dbPath := filepath.Join(s.RunsDir, "runs.db")
	if _, err := os.Stat(dbPath); err == nil {
		store := journal.NewJsonStore(s.RunsDir, dbPath)
		artsMap = map[string][]string{}
		storeIds, _ := store.ListRuns()
		for _, id := range storeIds {
			if !seen[id] {
				ids = append(ids, id)
				seen[id] = true
			}
			artsMap[id], _ = store.ListArtifacts(id)
		}
	}
	sortStringsDesc(ids)
	// P2 F-03: стабильный потолок ответа — самые новые раны. Форма ответа
	// (массив) не меняется, факт обрезания отдаётся заголовком.
	truncated := len(ids) > maxRunsListed
	if truncated {
		ids = ids[:maxRunsListed]
	}
	var list []map[string]interface{}
	for _, id := range ids {
		m := s.cachedRunSummary(filepath.Join(s.RunsDir, id))
		if artsMap != nil {
			m["artifacts"] = artsMap[id]
		}
		list = append(list, m)
	}
	if list == nil {
		list = []map[string]interface{}{}
	}
	w.Header().Set("Content-Type", "application/json")
	if truncated {
		w.Header().Set("X-Wedra-Runs-Truncated", "true")
	}
	json.NewEncoder(w).Encode(list)
}

func sortStringsDesc(a []string) {
	sort.Slice(a, func(i, j int) bool { return a[i] > a[j] })
}

func (s *Server) handleRunDetail(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/runs/")
	if id == "" {
		http.Error(w, "missing id", 400)
		return
	}
	// v0.24: /api/runs/<id>/gate — статус/решение браузерного гейта
	if rest, ok := strings.CutSuffix(id, "/gate"); ok {
		s.handleRunGate(w, r, rest)
		return
	}
	// v0.9: /api/runs/<id>/cancel — отмена активного рана (нужна сессия)
	if rest, ok := strings.CutSuffix(id, "/cancel"); ok {
		s.handleRunCancel(w, r, rest)
		return
	}
	// v0.22: /api/runs/<id>/journal?since=N — live-хвост для GUI
	if rest, ok := strings.CutSuffix(id, "/journal"); ok {
		dir, err := journal.SafeRunDir(s.RunsDir, rest)
		if err != nil {
			http.Error(w, "invalid run id", 400)
			return
		}
		since := 0
		if v := r.URL.Query().Get("since"); v != "" {
			fmt.Sscanf(v, "%d", &since)
		}
		if since < 0 {
			since = 0
		}
		// P2 F-03: окно ответа ограничено; курсор next продолжает поллинг
		// с того места, где окно оборвалось (total — для клиентов без next).
		res, err := journal.NewReader(dir).EventsBounded(since, runJournalLimits(false))
		if err != nil {
			http.Error(w, err.Error(), 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"total": res.Total, "events": res.Events,
			"first": res.First, "next": res.Next, "truncated": res.Truncated,
		})
		return
	}
	dir, err := journal.SafeRunDir(s.RunsDir, id)
	if err != nil {
		http.Error(w, "invalid run id", 400)
		return
	}
	rd := journal.NewReader(dir)
	// P2 F-03: деталка отдаёт хвост журнала в пределах потолка ответа.
	// total/first/truncated описывают окно, поэтому курсор since для
	// поллинга остаётся точным.
	res, err := rd.EventsBounded(0, runJournalLimits(true))
	if err != nil {
		http.Error(w, err.Error(), 404)
		return
	}
	snap, _ := rd.ContextSnapshot()
	dbPath := filepath.Join(s.RunsDir, "runs.db")
	var arts []string
	if _, err := os.Stat(dbPath); err == nil {
		store := journal.NewJsonStore(s.RunsDir, dbPath)
		arts, _ = store.ListArtifacts(id)
	} else {
		store := journal.NewFilesystemStore(s.RunsDir)
		arts, _ = store.ListArtifacts(id)
	}
	summary := runSummary(dir)
	if !res.Truncated {
		// журнал поместился в окно целиком — summary считаем по уже
		// разобранным событиям, без второго прохода по файлу
		summary = summarizeRun(dir, res.Events)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id": id, "events": res.Events, "context": snap, "artifacts": arts,
		"status": summary["status"], "pipeline": summary["pipeline"],
		"total": res.Total, "first": res.First, "truncated": res.Truncated,
	})
}

// runJournalLimits — потолок ответа с журналом рана (P2 F-03). tail=true —
// окно последних событий (деталка рана и статус гейта), иначе окно от
// позиции since (live-хвост с курсором).
func runJournalLimits(tail bool) journal.ReadLimits {
	return journal.ReadLimits{
		MaxBytes:  maxRunResponseBytes,
		MaxEvents: maxRunResponseEvents,
		Tail:      tail,
	}
}

func (s *Server) resolvePipelineFile(name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("нужно имя или путь к pipeline")
	}
	path, err := secureContainedPath(s.PipelinesDir, name)
	if err != nil {
		return "", err
	}
	if fi, err := os.Stat(path); err == nil && !fi.IsDir() {
		return path, nil
	}
	return "", fmt.Errorf("pipeline %q не найден внутри %s", name, s.PipelinesDir)
}

// handleRunStart — v0.22: POST /api/run {file, yes} — in-process запуск.
// v0.24: yes=false — человеческий гейт в браузере: ран блокируется на
// gate-шаге, решение — POST /api/runs/<runID>/gate. ID рана известен заранее
// (в ответе 202) — карточка гейта не гадает имя.
//
// Сессию здесь проверять не нужно: Routes() требует её для всего под /api/*,
// кроме publicAPI. Проверка в обработчике была бы второй копией одного правила.
func (s *Server) handleRunStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST {file, yes}", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		File string `json:"file"`
		Yes  bool   `json:"yes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "json: "+err.Error(), 400)
		return
	}
	path, err := s.resolvePipelineFile(req.File)
	if err != nil {
		http.Error(w, "file: "+err.Error(), 400)
		return
	}
	pf, err := pipeline.LoadPipelineFile(path)
	if err != nil {
		http.Error(w, "pipeline: "+err.Error(), 400)
		return
	}
	// v0.9: валидация и secrets — синхронно, ДО 202. Раньше 202 выдавался
	// сразу, а отказ валидации печатался только в stdout сервера: клиент
	// (агент) получал run_id несуществующего рана и не видел причину.
	eng := s.runEngine()
	issues := pipeline.ValidateIssues(pf, eng)
	errs, _ := pipeline.SplitIssues(issues)
	if len(errs) > 0 {
		writeJSON(w, 400, map[string]interface{}{"ok": false, "issues": issues})
		return
	}
	var missing []string
	for _, k := range pf.Pipeline.Secrets {
		if os.Getenv(k) == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		writeJSON(w, 400, map[string]interface{}{
			"ok": false, "code": "secrets_missing", "missing": missing,
			"error": "secrets: не заданы переменные окружения: " + strings.Join(missing, ", "),
		})
		return
	}
	if !s.runMu.TryLock() {
		writeJSON(w, 409, map[string]string{"error": "уже идёт ран — дождись завершения", "code": "E_RUN_BUSY"})
		return
	}
	runID, err := execution.NewRunID(pf.Pipeline.Name)
	if err != nil {
		s.runMu.Unlock()
		writeJSON(w, 500, map[string]string{"error": "не удалось создать run_id: " + err.Error()})
		return
	}
	runCtx, cancel := context.WithCancel(context.Background())
	s.setCancel(runID, cancel)
	writeJSON(w, 202, map[string]interface{}{"status": "started", "file": req.File, "run": runID, "issues": issues})

	go func() {
		defer s.runMu.Unlock()
		defer s.clearGate(runID)
		defer s.clearCancel(runID)
		defer cancel()
		opts := core.RunOptions{Yes: req.Yes, Quiet: true, RunsDir: s.RunsDir, RunID: runID, Ctx: runCtx, Trusted: s.Trusted}
		if !req.Yes || pipelineNeedsHuman(pf) {
			opts.GateUI = func(st *pipeline.Step) gate.GateUI {
				ui := gate.NewChannelUI()
				s.setGate(runID, ui)
				// отмена рана закрывает ожидающий гейт (иначе висит вечно)
				go func() { <-runCtx.Done(); ui.Close() }()
				return ui
			}
		}
		stats, err := core.Run(pf, eng, opts)
		if err != nil {
			fmt.Printf("[gui] %s: %v\n", req.File, err)
		} else {
			fmt.Printf("[gui] %s: ok=%d aborted=%d\n", req.File, stats.OK, stats.Aborted)
		}
	}()
}

// pipelineNeedsHuman — есть гейт, который --yes не одобрит (approval: human /
// gates: human_only): такому рану нужен браузерный GateUI даже при yes=true.
func pipelineNeedsHuman(pf *pipeline.PipelineFile) bool {
	for i := range pf.Pipeline.Steps {
		st := &pf.Pipeline.Steps[i]
		if pipeline.IsBuiltin(st.Plugin) && pf.Pipeline.RequiresHuman(st) {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// handleRunCancel — v0.9: POST /api/runs/<id>/cancel. Сессия — в Routes().
func (s *Server) handleRunCancel(w http.ResponseWriter, r *http.Request, id string) {
	if _, err := journal.SafeRunDir(s.RunsDir, id); err != nil {
		http.Error(w, "invalid run id", 400)
		return
	}
	if r.Method != "POST" {
		http.Error(w, "POST", http.StatusMethodNotAllowed)
		return
	}
	cancel := s.cancelFor(id)
	if cancel == nil {
		writeJSON(w, 409, map[string]string{"error": "ран не активен (завершён или не найден)", "code": "E_RUN_DONE"})
		return
	}
	cancel()
	writeJSON(w, 202, map[string]string{"status": "cancelling", "run": id})
}

// handleRunGate — v0.24: GET/POST /api/runs/<id>/gate.
// GET — ожидающий ли гейт (из журнала: gate_wait без gate_decision после).
// POST {action, edits} — решение: POST в ChannelUI активного рана (409, если
// гейта нет, уже решён или ран завершён).
func (s *Server) handleRunGate(w http.ResponseWriter, r *http.Request, id string) {
	dir, err := journal.SafeRunDir(s.RunsDir, id)
	if err != nil {
		http.Error(w, "invalid run id", 400)
		return
	}
	switch r.Method {
	case "GET":
		// P2 F-03: состояние гейта определяется последними gate_wait/gate_
		// decision, поэтому хватает хвоста журнала в пределах потолка.
		res, err := journal.NewReader(dir).EventsBounded(0, runJournalLimits(true))
		if err != nil {
			http.Error(w, err.Error(), 404)
			return
		}
		pending := false
		var lastWait map[string]interface{}
		for _, e := range res.Events {
			switch e["type"] {
			case "gate_wait":
				pending, lastWait = true, e
			case "gate_decision":
				pending, lastWait = false, nil
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if pending {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"pending": true, "step": lastWait["step"],
				"form": lastWait["form"], "actions": lastWait["actions"],
			})
		} else {
			json.NewEncoder(w).Encode(map[string]interface{}{"pending": false})
		}
	case "POST":
		// сессия человека уже проверена в Routes() — и на GET, и на POST
		var req struct {
			Action string                 `json:"action"`
			Edits  map[string]interface{} `json:"edits"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "json: "+err.Error(), 400)
			return
		}
		ui := s.gateFor(id)
		if ui == nil {
			w.WriteHeader(409)
			json.NewEncoder(w).Encode(map[string]string{"error": "нет ожидающего гейта (ран не на гейте или завершён)"})
			return
		}
		// v0.27: ран завершён (run_end в журнале) — решения не принимает, даже
		// если clearGate ещё не успел сработать (окно между записью run_end и
		// dereg под нагрузкой давало 202 на мёртвый ран)
		if sum := s.cachedRunSummary(dir); sum["status"] != "running" {
			st, _ := sum["status"].(string)
			w.WriteHeader(409)
			json.NewEncoder(w).Encode(map[string]string{"error": "ран завершён (" + st + ") — решения не принимает"})
			return
		}
		// source/session проставляет сервер: клиент их не задаёт (json:"-")
		d := gate.Decision{Action: req.Action, Edits: req.Edits, Source: gate.SourceGUI, Session: s.sessionHash(r)}
		if !ui.SendDecision(d) {
			w.WriteHeader(409)
			json.NewEncoder(w).Encode(map[string]string{"error": "гейт уже решён"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(202)
		json.NewEncoder(w).Encode(map[string]string{"status": "queued"})
	default:
		http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleValidatePipeline(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST yaml", http.StatusMethodNotAllowed)
		return
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	pf, err := pipeline.LoadPipelineFileFromBytes(data)
	if err != nil {
		http.Error(w, "parse: "+err.Error(), 400)
		return
	}
	issues := pipeline.ValidateIssues(pf, s.Engine)
	errs, warns := pipeline.SplitIssues(issues)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"errors": errs, "warnings": warns, "ok": len(errs) == 0, "issues": issues})
}

func (s *Server) handlePlanPipeline(w http.ResponseWriter, r *http.Request) {
	// v0.12 fix: раньше был алиас на validate, никакого DAG
	// теперь строит DAG: узлы + рёбра по bind/form зависимостям
	if r.Method != "POST" {
		http.Error(w, "POST yaml", http.StatusMethodNotAllowed)
		return
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	pf, err := pipeline.LoadPipelineFileFromBytes(data)
	if err != nil {
		http.Error(w, "parse: "+err.Error(), 400)
		return
	}
	issues := pipeline.ValidateIssues(pf, s.Engine)
	errs, warns := pipeline.SplitIssues(issues)
	// строим DAG
	nodes := []map[string]interface{}{}
	edges := []map[string]string{}
	// pre-phase для steps.* foreach
	preSteps := map[string]bool{}
	if pf.Pipeline.Foreach != "" && strings.HasPrefix(pf.Pipeline.Foreach, "steps.") {
		parts := strings.Split(pf.Pipeline.Foreach, ".")
		if len(parts) >= 2 {
			srcID := parts[1]
			for _, st := range pf.Pipeline.Steps {
				preSteps[st.ID] = true
				if st.ID == srcID {
					break
				}
			}
		}
	}
	for _, st := range pf.Pipeline.Steps {
		phase := "foreach"
		if preSteps[st.ID] {
			phase = "pre"
		}
		if st.AfterForeach {
			phase = "post"
		}
		whenLabel := ""
		if st.When.IsSet() {
			whenLabel = st.When.Path
			if st.When.Op != "" && st.When.Op != "truthy" {
				whenLabel += " " + st.When.Op
				if st.When.Value != nil {
					whenLabel += " " + fmt.Sprintf("%v", st.When.Value)
				}
			}
		}
		nodes = append(nodes, map[string]interface{}{
			"id": st.ID, "plugin": st.Plugin, "phase": phase, "on_error": st.OnError,
			"bind": st.Bind, "after_foreach": st.AfterForeach,
			"when": whenLabel, "foreach": st.Foreach, "foreach_item": st.ForeachItem,
			"parallel_group": st.ParallelGroup,
		})
		// v0.22: рёбра от путей when/foreach (зависимость на данные)
		for _, p := range []string{st.Foreach, st.When.Path} {
			if strings.HasPrefix(p, "steps.") {
				parts := strings.Split(p, ".")
				if len(parts) >= 2 {
					edges = append(edges, map[string]string{"from": parts[1], "to": st.ID, "via": p})
				}
			}
		}
		// рёбра: из bind и form
		for _, from := range st.Bind {
			if strings.HasPrefix(from, "steps.") {
				parts := strings.Split(from, ".")
				if len(parts) >= 2 {
					edges = append(edges, map[string]string{"from": parts[1], "to": st.ID, "via": from})
				}
			}
		}
		for _, f := range st.Form {
			if strings.HasPrefix(f.Field, "steps.") {
				parts := strings.Split(f.Field, ".")
				if len(parts) >= 2 {
					edges = append(edges, map[string]string{"from": parts[1], "to": st.ID, "via": f.Field})
				}
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"pipeline": pf.Pipeline.Name,
		"foreach":  pf.Pipeline.Foreach,
		"errors":   errs,
		"warnings": warns,
		"issues":   issues,
		"ok":       len(errs) == 0,
		"dag": map[string]interface{}{
			"nodes": nodes,
			"edges": edges,
		},
	})
}
