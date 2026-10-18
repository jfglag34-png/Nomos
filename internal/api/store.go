package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wykserdex/wedra/internal/doctor"
	"github.com/wykserdex/wedra/internal/install"
	"github.com/wykserdex/wedra/internal/plugin"
	"github.com/wykserdex/wedra/internal/registry"
)

// storeLogLimit — сколько журнала установки отдаём в ответе.
//
// Установка тянет git и печатает прогресс; без ограничения ответ мог бы вырасти
// настолько, что его перестаёт отдавать разумный клиент. Обрезанный журвал
// честнее ошибки: человек видит, что установка шла, и может повторить её в CLI,
// где видно всё.
const storeLogLimit = 64 << 10

// handleStore — витрина плагинов: что есть в реестре, что уже стоит и что
// из стоящего готово к запуску.
//
// Три разных вопроса, и они не сводятся в один флаг «готов»:
//
//	available — есть запись в реестре (факт репозитория);
//	installed — лежит каталог рядом (факт файловой системы);
//	status    — меряет doctor тем же Python, которым плагин запустится.
//
// Последнее известно ТОЛЬКО для установленных. Для записи реестра, которой
// ещё нет на диске, статус неизвестен по существу: донор может отсутствовать, а
// может и стоять. Поэтому для неустановленных плагинов status остаётся пустым,
// а не «ready» и не «needs» — выдумывать готовность того, чего нет на диске,
// было бы той же ложью, из-за которой doctor когда-то советовал несуществующий
// pip install.
func (s *Server) handleStore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{
			"error": "витрина только читается; установка — POST /api/store/install",
			"code":  "E_METHOD_NOT_ALLOWED",
		})
		return
	}

	// Готовность меряется только по явному ?status=1.
	//
	// doctor.Run опрашивает КАЖДЫЙ плагин интерпретатором и, для ненайденных
	// доноров, ходит в индекс PyPI. Это десятки секунд на полном каталоге, и
	// тянуть их в открытие вкладки нельзя: витрина должна открываться сразу, а
	// «что сломано» — это вопрос, на который человек отвечает кнопкой.
	//
	// По умолчанию отдаём только каталог: что есть в реестре, что стоит, какие
	// права заявлены. Готовность — отдельным запросом, осознанно.
	rep := doctor.Report{}
	statusByID := map[string]doctor.Plugin{}
	// status_measured — был ли замер вообще. Без него нули в ready/needs_*
	// читаются как «всё готово», то есть ровно та ложь, против которой вся
	// эта витрина и написана: молчание о неизвестном неотличимо от успеха.
	statusMeasured := r.URL.Query().Get("status") == "1"
	if statusMeasured {
		rep = doctor.Run(s.PluginsDir)
		for _, p := range rep.Plugins {
			statusByID[p.ID] = p
		}
	}
	root := filepath.Dir(s.PluginsDir)
	if _, err := os.Stat(filepath.Join(root, registry.RegistryFile)); err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"available": false, "reason": "реестр не найден: " + registry.RegistryFile,
			"plugins": []interface{}{}, "python": rep.Python, "python_error": rep.PythonError,
			"summary": rep.Summary, "unverified_donors": rep.UnverifiedDonors,
			"status_measured": statusMeasured,
		})
		return
	}
	handle, err := registry.Load(root)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"available": false, "reason": err.Error(),
			"plugins": []interface{}{}, "python": rep.Python, "python_error": rep.PythonError,
			"summary": rep.Summary, "unverified_donors": rep.UnverifiedDonors,
			"status_measured": statusMeasured,
		})
		return
	}

	list := make([]map[string]interface{}, 0, len(handle.Registry.Plugins))
	for name, entry := range handle.Registry.Plugins {
		item := map[string]interface{}{
			"name":        name,
			"description": entry.Description,
			"version":     entry.Version,
			"commit":      entry.Commit,
			"path":        entry.Path,
			"source":      entry.Source,
		}

		// Установлен ли: лежит ли каталог из записи реестра на диске.
		//
		// Путь берётся ИЗ ЗАПИСИ (entry.Path), а не собирается как
		// «plugins/<имя>»: в реестре лежит полный путь (plugins/community/x),
		// и склейка по имени давала «не установлен» для всех 99 стоящих
		// плагинов. Проверено живым запуском витрины, а не чтением кода.
		//
		// Проверяем каталог, а не запись реестра: расхождение возможно
		// (плагин поставили руками, или реестр обновили, а каталог нет).
		dir := ""
		if entry.Path != "" {
			dir = filepath.Join(root, entry.Path)
		}
		if dir == "" {
			dir = filepath.Join(s.PluginsDir, name)
		}
		installed := false
		if _, statErr := os.Stat(filepath.Join(dir, "plugin.yaml")); statErr == nil {
			installed = true
		}
		item["installed"] = installed

		// Готовность и доверие — только для того, что реально лежит на диске.
		if installed {
			if st, ok := statusByID[name]; ok {
				item["status"] = st.Status
				item["donor"] = st.Donor
				item["donor_kind"] = st.DonorKind
				item["donor_missing"] = st.DonorMissing
				item["donor_absent_from_index"] = st.DonorAbsentFromIndex
				item["donor_index_unknown"] = st.DonorIndexUnknown
				item["requires"] = st.Requires
				item["missing_requires"] = st.MissingRequires
				item["install_hint"] = st.Install
			}
			// Доверие считается по фактическому содержимому каталога, тем же
			// решением, что и список плагинов и запуск ранов (DecideTrust).
			// Неустановленному плагину доверие не выдаётся: нечего хэшировать.
			if m, loadErr := s.Engine.LoadManifest(dir); loadErr == nil {
				dec := plugin.DecideTrust(m, plugin.TrustPolicy{Trusted: s.Trusted})
				item["trusted"] = dec.Trusted
				if dec.Digest != "" {
					item["content_sha256"] = dec.Digest
				}
				// Причина и ключ — те же, что у /api/plugins: витрина и список
				// обязаны говорить об одном плагине одинаково, иначе человек
				// увидит в одном экране «доверен», а в другом «не запустится».
				if !dec.Trusted {
					item["blocked_reason"] = blockedReason(dec.Reason)
				}
			}
		} else {
			item["status"] = ""
			item["status_unknown"] = true
		}
		list = append(list, item)
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i]["name"].(string) < list[j]["name"].(string)
	})

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"available":           true,
		"plugins":             list,
		"total":               len(list),
		"python":              rep.Python,
		"python_error":        rep.PythonError,
		"summary":             rep.Summary,
		"ready":               rep.Ready,
		"needs_dependency":    rep.NeedsDep,
		"needs_external_tool": rep.NeedsDonor,
		"status_measured":     statusMeasured,
		"unverified_donors":   rep.UnverifiedDonors,
	})
}

// handleStoreInstall — установка плагина из витрины.
//
// Здесь идёт РЕАЛЬНАЯ установка: git-клон, запись в plugins/, блокировка
// .wedra. Поэтому обработчик сознательно делает две вещи, которых нет в
// read-only витрине: требует запись в PluginsDir и отдаёт наружу журнал.
//
// Безопасность целиком наследуется от обёртки Routes(): сессия, CSRF,
// allow-list Host и лимит тела применяются автоматически. В publicAPI этот
// путь не добавляется — установка кода никогда не бывает без входа.
func (s *Server) handleStoreInstall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{
			"error": "установка только POST", "code": "E_METHOD_NOT_ALLOWED",
		})
		return
	}
	var body struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	if err := dec.Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error": "ожидался JSON {\"name\": ...}", "code": "E_BAD_REQUEST",
		})
		return
	}
	// Имя приходит из тела, а не из пути: имя плагина попадает в имя каталога
	// и в запись реестра, поэтому проверяется тем же ParseRef, что и в CLI,
	// а не «просто нарежем и обернём в os.Stat».
	ref := strings.TrimSpace(body.Name)
	if ref == "" {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error": "не указан плагин", "code": "E_BAD_REQUEST",
		})
		return
	}
	name, version := install.ParseRef(ref)
	if body.Version != "" && version == "" {
		version = strings.TrimSpace(body.Version)
	}
	if err := validatePluginName(name); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error": err.Error(), "code": "E_BAD_REQUEST",
		})
		return
	}

	root := filepath.Dir(s.PluginsDir)
	var buf bytes.Buffer
	res, err := install.Run(&buf, install.Request{
		Name:        name,
		Version:     version,
		RegistrySrc: filepath.Join(root, registry.RegistryFile),
		Dest:        s.PluginsDir,
		LocalSource: root,
	})
	log := buf.String()
	if len(log) > storeLogLimit {
		log = log[:storeLogLimit] + "\n… журнал обрезан\n"
	}
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]interface{}{
			"ok": false, "error": err.Error(), "log": log,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok": true, "name": res.Name, "version": res.Version, "dir": res.Dir,
		"source": res.Source, "commit": res.Commit, "notes": res.Notes, "log": log,
	})
}

// validatePluginName — границы имени плагина для HTTP-пути.
//
// Тот же список символов, что у реестра (internal/registry: ValidateComponent),
// потому что имя становится и каталогом, и ключом записи. Здесь он намеренно
// строже: «достаточно, чтобы не сломать fs» здесь недостаточно — значение
// приходит из тела запроса, то есть от человека, который уже вошёл, но всё
// равно не должен иметь возможности указать путь.
func validatePluginName(name string) error {
	if name == "" {
		return errBadPluginName("имя пустое")
	}
	if len(name) > 128 {
		return errBadPluginName("имя длиннее 128 символов")
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_' || r == '-' || r == '.':
			if i == 0 {
				return errBadPluginName("имя не может начинаться с разделителя")
			}
		default:
			return errBadPluginName("в имени есть недопустимый символ")
		}
	}
	if strings.Contains(name, "..") {
		return errBadPluginName("в имени не может быть двух точек подряд")
	}
	return nil
}

type errBadPluginName string

func (e errBadPluginName) Error() string {
	return "недопустимое имя плагина: " + string(e)
}
