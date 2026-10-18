package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"github.com/wykserdex/wedra/internal/buildinfo"
	"github.com/wykserdex/wedra/internal/pipeline"
	"github.com/wykserdex/wedra/internal/plugin"
)

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

