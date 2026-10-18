package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wykserdex/wedra/internal/core"
	"github.com/wykserdex/wedra/internal/plugin"
)

// pluginSourceFiles — единственный белый список файлов, которые редактор может
// читать и писать.
//
// Не «разрешено всё, что не запрещено», а именно список: без него через
// `files` можно было бы дотянуться до любого файла внутри каталога плагина
// (а через симлинк — и дальше), и редактор из «правит манифест» превратился бы
// в «пишет что угодно рядом».
var pluginSourceFiles = []string{
	"plugin.yaml",
	"main.py",
	"plugin.test.yaml",
	"requirements.lock",
	"README.md",
}

func isPluginSourceFile(name string) bool {
	for _, f := range pluginSourceFiles {
		if f == name {
			return true
		}
	}
	return false
}

// pluginTestTimeout — потолок на прогон тестов плагина из редактора.
//
// Тесты запускают интерпретатор и код плагина, то есть это исполняемый код по
// запросу из браузера. Без предела один зависший плагин держит обработчик и
// соединение, а запросов может быть много. Потолок НЕ усекает молча: по
// истечении возвращается ошибка с пометкой timeout, чтобы человек увидел «не
// дошло», а не «зелёное».
const pluginTestTimeout = 5 * time.Minute

// resolvePluginDir — тот же поиск по четырём базам, что у handlePlugins и
// handlePluginDetail.
//
// Дублирование метода не «принципиально»: база знаний о том, где лежат
// плагины, принадлежит ядру, и пока нет функции-единого источника, копия
// метода честнее выдуманного реестра каталогов, который завтра разъедется с
// реальностью.
func (s *Server) resolvePluginDir(id string) (string, error) {
	if id == "" || strings.ContainsAny(id, "/\\") {
		return "", errors.New("недопустимое имя плагина")
	}
	for _, base := range []string{
		s.PluginsDir,
		filepath.Join(s.PluginsDir, "official"),
		filepath.Join(s.PluginsDir, "community"),
		filepath.Join(s.PluginsDir, plugin.AgentPluginDir),
	} {
		if _, err := os.Stat(base); err != nil {
			continue
		}
		dir := filepath.Join(base, id)
		if _, err := os.Stat(filepath.Join(dir, "plugin.yaml")); err == nil {
			return dir, nil
		}
	}
	return "", errors.New("плагин не найден")
}

// readPluginFiles — читает белый список файлов каталога плагина.
//
// Возвращает files и список ОТСУТСТВУЮЩИХ: плагин без requirements.lock — это
// нормальное состояние (runtime.requires пуст), и редактор обязан это показать,
// а не прятать в пустой textarea.
func readPluginFiles(dir string) (files map[string]string, missing []string, err error) {
	files = map[string]string{}
	// missing — массив, а не nil: пустой nil уехал бы в JSON как null, и
	// страница получила бы «нет файла» вместо «все файлы на месте».
	missing = []string{}
	for _, name := range pluginSourceFiles {
		raw, readErr := os.ReadFile(filepath.Join(dir, name))
		if readErr != nil {
			missing = append(missing, name)
			continue
		}
		files[name] = string(raw)
	}
	if len(missing) == len(pluginSourceFiles) {
		return nil, missing, errors.New("каталог плагина пуст")
	}
	return files, missing, nil
}

// stagingCopyLimit — потолок на объём копии плагина во временный каталог.
//
// Копируется каталог ЦЕЛИКОМ (см. stagingDir), а каталоги плагинов не должны
// весить мегабайты: у 8 из 99 рядом с манифестом лежат fixtures/ общим весом
// десятки килобайт. Потолок нужен не «на всякий случай», а чтобы запрос из
// браузера не заставил сервер скопировать в temp всё подряд, если плагин
// внезапно окажется тяжёлым.
const stagingCopyLimit = 32 << 20

// copyPluginTree — копирует каталог плагина целиком для проверки черновика.
//
// Почему целиком, а не только редактируемые файлы: у плагина бывают
// СОБСТВЕННЫЕ данные рядом с манифестом — fixtures/, моки донора, справочные
// файлы, на которые ссылается plugin.test.yaml. Копия только белого списка
// роняла бы тесты с «файл не найден», и это выглядело бы как ошибка плагина,
// хотя сломан был бы стенд. Белый список ограничивает ЗАПИСЬ, а не состав
// каталога.
//
// Симлинки не переносятся: копирование по ссылке вынесло бы наружу то, что
// человек не собирался отдавать в temp.
func copyPluginTree(src, dst string) error {
	var total int64
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(src, path)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		case d.Type()&os.ModeSymlink != 0:
			return nil // пропускаем молча: содержимое копии это не изменит,
			// а читать по ссылке наружу нельзя
		case d.Type().IsRegular():
			info, statErr := d.Info()
			if statErr != nil {
				return statErr
			}
			total += info.Size()
			if total > stagingCopyLimit {
				return errors.New("каталог плагина слишком велик для проверки черновика")
			}
			raw, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			return os.WriteFile(target, raw, 0o644)
		default:
			return nil
		}
	})
}

// stagingDir — временная копия каталога плагина с наложенными правками.
//
// Зачем копия, а не запись по месту: валидация обязана идти ДО записи. Если
// валидировать прямо в каталоге плагина, то при ошибке в manifest пришлось бы
// сначала его испортить, потом откатывать — а «откат» в редакторе кода живёт
// рядом с самим кодом, и любая ошибка отката выглядит как потеря работы
// человека. Здесь оригинал не трогается вообще: сомнительные файлы уходят в
// temp-каталог и там же проверяются.
func stagingDir(dir string, files map[string]string) (string, func(), error) {
	// Белый список проверяется ДО копирования: невалидный запрос не должен
	// ещё и данные тащить на диск.
	for name := range files {
		if !isPluginSourceFile(name) {
			return "", func() {}, errors.New("файл вне белого списка редактора: " + name)
		}
	}
	tmp, err := os.MkdirTemp("", "wedra-plugin-edit-")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(tmp) }

	// Каталог плагина копируется ПОД СВОИМ ИМЕНЕМ, а не прямо в корень temp.
	//
	// Это не косметика: тесты плагинов законно ссылаются на имя собственного
	// каталога. У detect_secrets кейс ждёт, что путь находки содержит
	// "detect_secrets", и копия в "stage2" роняла честный тест — 15/16 вместо
	// 16/16 при побайтово одинаковом содержимом. В настоящей установке каталог
	// всегда назван по id плагина, и проверка обязана видеть ту же раскладку.
	staged := filepath.Join(tmp, filepath.Base(dir))
	if err := os.MkdirAll(staged, 0o755); err != nil {
		cleanup()
		return "", func() {}, err
	}
	if err := copyPluginTree(dir, staged); err != nil {
		cleanup()
		return "", func() {}, err
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(staged, name), []byte(body), 0o644); err != nil {
			cleanup()
			return "", func() {}, err
		}
	}
	return staged, cleanup, nil
}

// validateStaged — валидация и предупреждения по подставленной копии.
func validateStaged(dir string) (errorsOut []string, warnings []string) {
	errorsOut = core.ValidatePluginDir(dir)
	warnings = core.PluginManifestWarnings(dir)
	return errorsOut, warnings
}

// handlePluginsRoot — один обработчик на корень /api/plugins.
//
// Отдельные HandleFunc на один и тот же путь запрещены: ServeMux паникует на
// дубликате регистрации. Метод здесь — и есть различие между «показать список»
// (GET) и «создать плагин» (POST), поэтому разбирается он, а не мукс.
func (s *Server) handlePluginsRoot(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handlePlugins(w, r)
	case http.MethodPost:
		s.handlePluginCreate(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{
			"error": "каталог плагинов: GET или POST", "code": "E_METHOD_NOT_ALLOWED",
		})
	}
}

// handlePluginSubtree — один обработчик на /api/plugins/… вместо четырёх.
//
// По той же причине: префикс может быть зарегистрирован один раз. Разбор
// хвоста (/source, /validate, /test) живёт здесь, и это единственное место,
// где известен полный список хвостов: новый эндпоинт нельзя добавить,
// забыв внести его сюда, потому что иначе он молча уехал бы в карточку
// плагина и вернул бы её вместо своего ответа.
func (s *Server) handlePluginSubtree(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasSuffix(r.URL.Path, "/source"):
		s.handlePluginSource(w, r)
	case strings.HasSuffix(r.URL.Path, "/validate"):
		s.handlePluginValidate(w, r)
	case strings.HasSuffix(r.URL.Path, "/test"):
		s.handlePluginTest(w, r)
	default:
		s.handlePluginDetail(w, r)
	}
}

// handlePluginSource — GET: прочитать редактируемые файлы; PUT: записать.
//
// Разбор пути сделан здесь, а не отдельными маршрутами: /api/plugins/ уже занят
// handlePluginDetail, который вытаскивает id префиксом. Новые хвосты
// (/source, /validate, /test) разбираются вручную по тому же принципу —
// префикс известен, хвост проверяется по списку, а не подставляется в путь.
func (s *Server) handlePluginSource(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/plugins/")
	if !strings.HasSuffix(rest, "/source") {
		http.Error(w, "not found", 404)
		return
	}
	id := strings.TrimSuffix(rest, "/source")
	dir, err := s.resolvePluginDir(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"error": err.Error(), "code": "E_PLUGIN_NOT_FOUND",
		})
		return
	}
	if r.Method == http.MethodGet {
		files, missing, readErr := readPluginFiles(dir)
		if readErr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
				"error": readErr.Error(), "code": "E_READ_FAILED",
			})
			return
		}
		writeJSON(w, http.StatusOK, s.sourcePayload(id, dir, files, missing))
		return
	}
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", "GET, PUT")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{
			"error": "источник плагина: GET или PUT", "code": "E_METHOD_NOT_ALLOWED",
		})
		return
	}

	var body struct {
		Files map[string]string `json:"files"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20))
	if err := dec.Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error": "ожидался JSON с полем files", "code": "E_BAD_REQUEST",
		})
		return
	}
	if len(body.Files) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error": "пустые правки: нечего сохранять", "code": "E_BAD_REQUEST",
		})
		return
	}
	staged, cleanup, err := stagingDir(dir, body.Files)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error": err.Error(), "code": "E_BAD_REQUEST",
		})
		return
	}
	defer cleanup()

	// Сначала проверка, потом запись. Это не «так красивее»: запись без
	// проверки оставила бы плагин в невалидном состоянии, из которого не
	// запустился бы ни ран, ни registry validate, и винить потом можно только
	// человека, который нажал «сохранить».
	errs, warns := validateStaged(staged)
	if len(errs) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"ok": false, "errors": errs, "warnings": warns,
		})
		return
	}

	// Запись по месту — только по разрешённым именам и только внутрь каталога
	// плагина. secureContainedPath повторяется на каждый файл намеренно:
	// проверка «каталог хороший» не делает проверенными файлы внутри него.
	written := make([]string, 0, len(body.Files))
	for _, name := range sortedKeys(body.Files) {
		target, pathErr := secureContainedPath(dir, name)
		if pathErr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{
				"error": "файл вне каталога плагина: " + name, "code": "E_BAD_REQUEST",
			})
			return
		}
		if err := os.WriteFile(target, []byte(body.Files[name]), 0o644); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
				"error": err.Error(), "code": "E_WRITE_FAILED",
			})
			return
		}
		written = append(written, name)
	}

	// После записи состояние перечитывается с диска, а не собирается из
	// ответа сервера: доверие считается по хэшу СОДЕРЖИМОГО, и если бы мы
	// отдали «как записали», расхождение с реальностью осталось бы незамеченным.
	files, missing, readErr := readPluginFiles(dir)
	if readErr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"error": readErr.Error(), "code": "E_READ_FAILED",
		})
		return
	}
	out := s.sourcePayload(id, dir, files, missing)
	out["ok"] = true
	out["written"] = written
	writeJSON(w, http.StatusOK, out)
}

// sourcePayload — общая часть ответов чтения и записи: файлы, отсутствующие,
// доверие и хэш содержимого.
//
// Доверие вычисляется тем же DecideTrust, что и для списка плагинов, и для
// запуска. Здесь это особенно заметно: ПРАВКА ФАЙЛА МЕНЯЕТ ХЭШ, поэтому после
// сохранения доверенный плагин становится недоверенным. Это не поломка
// редактора, а прямое следствие модели: доверено содержимое, а не имя.
func (s *Server) sourcePayload(id, dir string, files map[string]string, missing []string) map[string]interface{} {
	out := map[string]interface{}{
		"id": id, "dir": dir, "files": files, "missing": missing,
		"editable": pluginSourceFiles,
	}
	m, err := s.Engine.LoadManifest(dir)
	if err == nil {
		dec := plugin.DecideTrust(m, plugin.TrustPolicy{Trusted: s.Trusted})
		out["trusted"] = dec.Trusted
		out["content_sha256"] = dec.Digest
		if !dec.Trusted {
			out["blocked_reason"] = blockedReason(dec.Reason)
		}
	}
	return out
}

// handlePluginValidate — проверить правки БЕЗ записи.
//
// Отдельный эндпоинт, а не режим PUT: человек должен иметь возможность
// спросить «правильно ли» до того, как что-то менять. И он же используется
// перед сохранением на стороне страницы — поэтому ответ обязан быть полным и
// без записи на диск.
func (s *Server) handlePluginValidate(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/plugins/")
	if !strings.HasSuffix(rest, "/validate") {
		http.Error(w, "not found", 404)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{
			"error": "валидация только POST", "code": "E_METHOD_NOT_ALLOWED",
		})
		return
	}
	id := strings.TrimSuffix(rest, "/validate")
	dir, err := s.resolvePluginDir(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"error": err.Error(), "code": "E_PLUGIN_NOT_FOUND",
		})
		return
	}
	var body struct {
		Files map[string]string `json:"files"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20))
	if err := dec.Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error": "ожидался JSON с полем files", "code": "E_BAD_REQUEST",
		})
		return
	}
	staged, cleanup, err := stagingDir(dir, body.Files)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error": err.Error(), "code": "E_BAD_REQUEST",
		})
		return
	}
	defer cleanup()
	errs, warns := validateStaged(staged)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok": len(errs) == 0, "errors": errs, "warnings": warns,
	})
}

// handlePluginTest — прогнать plugin.test.yaml плагина.
//
// Тесты запускают чужой код из браузера, поэтому здесь три ограничителя сразу:
// потолок времени, обязательный контекст (отмена запроса рвёт тест) и запрет на
// файлы вне белого списка через stagingDir.
//
// Файлы из тела подставляются в КОПИЮ. Это то, что делает кнопку «Прогнать
// тесты» честной: она проверяет то, что человек видит в редакторе, а не то,
// что лежит на диске с прошлого сохранения.
func (s *Server) handlePluginTest(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/plugins/")
	if !strings.HasSuffix(rest, "/test") {
		http.Error(w, "not found", 404)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{
			"error": "тесты только POST", "code": "E_METHOD_NOT_ALLOWED",
		})
		return
	}
	id := strings.TrimSuffix(rest, "/test")
	dir, err := s.resolvePluginDir(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"error": err.Error(), "code": "E_PLUGIN_NOT_FOUND",
		})
		return
	}
	var body struct {
		Files map[string]string `json:"files"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20))
	// Пустое тело — обычный случай (тест того, что на диске), поэтому
	// ошибка разбора JSON здесь НЕ фатальна: пустой набор файлов допустим.
	_ = dec.Decode(&body)

	target := dir
	if len(body.Files) > 0 {
		staged, cleanup, stageErr := stagingDir(dir, body.Files)
		if stageErr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{
				"error": stageErr.Error(), "code": "E_BAD_REQUEST",
			})
			return
		}
		defer cleanup()
		target = staged
	}

	ctx, cancel := context.WithTimeout(r.Context(), pluginTestTimeout)
	defer cancel()
	done := make(chan struct{})
	var passed, failed int
	var runErr error
	go func() {
		defer close(done)
		passed, failed, runErr = core.RunPluginTestsCtx(ctx, target, "", true)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		// Потолок не «успех»: возвращаем ошибку с меткой, иначе страница
		// показала бы зелёную строку для прогона, который не закончился.
		writeJSON(w, http.StatusGatewayTimeout, map[string]interface{}{
			"ok": false, "timeout": true, "code": "E_TEST_TIMEOUT",
			"error": "тесты не уложились в отведённое время — результат НЕ получен",
		})
		return
	}
	if runErr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"ok": false, "error": runErr.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok": failed == 0, "passed": passed, "failed": failed,
		"tested_draft": len(body.Files) > 0,
	})
}

// handlePluginCreate — создать новый плагин из шаблона, из интерфейса.
//
// Внутри ровно то же ядро, что у `wedra plugin create`: два пути создания
// плагина разошлись бы, и «из браузера» со временем начал бы отличаться от
// «из терминала». Генератор сам проверяет результат (core.CreatePluginWith
// возвращает результат прогонов), поэтому отдельного вызова ValidatePluginDir
// здесь не нужно — он уже был.
func (s *Server) handlePluginCreate(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/plugins" {
		http.Error(w, "not found", 404)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{
			"error": "создание плагина только POST", "code": "E_METHOD_NOT_ALLOWED",
		})
		return
	}
	var body struct {
		ID          string `json:"id"`
		Template    string `json:"template"`
		Author      string `json:"author"`
		Description string `json:"description"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error": "ожидался JSON {id, template, author, description}", "code": "E_BAD_REQUEST",
		})
		return
	}
	if err := validatePluginName(body.ID); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error": err.Error(), "code": "E_BAD_REQUEST",
		})
		return
	}
	// Каталог создаётся в PluginsDir, а не там, где указал браузер: путь
	// назначения из тела запроса — это путь записи на диск по воле клиента.
	dir, err := secureContainedPath(s.PluginsDir, body.ID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error": "недопустимое имя плагина", "code": "E_BAD_REQUEST",
		})
		return
	}
	if _, err := os.Stat(dir); err == nil {
		writeJSON(w, http.StatusConflict, map[string]interface{}{
			"error": "плагин " + body.ID + " уже существует", "code": "E_EXISTS",
		})
		return
	}
	if _, err := core.CreatePluginWith(dir, core.CreateOptions{
		Template:    body.Template,
		Author:      body.Author,
		Description: body.Description,
	}); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]interface{}{
			"error": err.Error(), "code": "E_CREATE_FAILED",
		})
		return
	}
	files, missing, err := readPluginFiles(dir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"error": err.Error(), "code": "E_READ_FAILED",
		})
		return
	}
	out := s.sourcePayload(body.ID, dir, files, missing)
	out["ok"] = true
	writeJSON(w, http.StatusOK, out)
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
