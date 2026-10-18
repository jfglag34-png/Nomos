package api

import (
	"encoding/json"
	"github.com/wykserdex/wedra/internal/journal"
	"github.com/wykserdex/wedra/internal/pipeline"
	"github.com/wykserdex/wedra/internal/registry"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

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
