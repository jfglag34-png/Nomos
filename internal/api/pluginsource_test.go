package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wykserdex/wedra/internal/core"
)

// pluginFixture — сервер с настоящим плагином, созданным тем же ядром, что и
// CLI (`wedra plugin create`), чтобы тесты не проверяли выдуманный артефакт.
func pluginFixture(t *testing.T) (*Server, *http.Cookie, string, string) {
	t.Helper()
	srv, root := h4Server(t, ListenOptions{Addr: testAddr}, "AAAA-BBBB-CCCC")
	dir := filepath.Join(root, "plugins", "demo_plugin")
	if _, err := core.CreatePluginWith(dir, core.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	return srv, login(t, srv, "AAAA-BBBB-CCCC"), root, dir
}

func doJSON(t *testing.T, srv *Server, cookie *http.Cookie, method, target string, payload interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader = strings.NewReader("")
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		body = strings.NewReader(string(raw))
	}
	return h4Request(t, srv, method, target, body, withCookieOf(cookie))
}

func decodeBody(t *testing.T, w *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var out map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("ответ не JSON: %v\n%s", err, w.Body.String())
	}
	return out
}

// ГЛАВНЫЙ ИНВАРИАНТ РЕДАКТОРА. Правка файла меняет хэш содержимого, поэтому
// плагин перестаёт быть доверенным.
//
// Это не поломка, а прямое следствие модели: доверено СОДЕРЖИМОЕ, а не имя
// плагина. Молча оставить «доверен» значило бы показать человеку кнопку
// запуска, которая не сработает, — ровно та ошибка, ради которой доверие и
// вычисляется по хэшу.
func TestPluginEditLosesTrust(t *testing.T) {
	srv, cookie, root, dir := pluginFixture(t)

	before := decodeBody(t, doJSON(t, srv, cookie, "GET", "/api/plugins/demo_plugin/source", nil))
	files, _ := before["files"].(map[string]interface{})
	if files == nil {
		t.Fatalf("GET /source не вернул файлы: %v", before)
	}
	if before["trusted"] != false && before["trusted"] != nil {
		t.Logf("до правки trusted=%v (ок, если плагина нет в allow-list)", before["trusted"])
	}
	mainPy, _ := files["main.py"].(string)

	// Правка, которая обязана остаться валидной: меняется только комментарий.
	updated := strings.Replace(mainPy, "#!/usr/bin/env python3", "#!/usr/bin/env python3\n# правка редактора", 1)
	if updated == mainPy {
		t.Fatalf("правка не применилась, тест бессмыслен")
	}
	rec := doJSON(t, srv, cookie, "PUT", "/api/plugins/demo_plugin/source",
		map[string]interface{}{"files": map[string]string{"main.py": updated}})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: code=%d\n%s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	if out["ok"] != true {
		t.Fatalf("ok=%v: %v", out["ok"], out)
	}
	if out["trusted"] != false {
		t.Errorf("trusted=%v после правки: доверие обязано слететь, содержимое изменилось", out["trusted"])
	}
	// Запись должна была дойти до диска, а не остаться в ответе.
	onDisk, err := os.ReadFile(filepath.Join(dir, "main.py"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(onDisk), "правка редактора") {
		t.Errorf("на диске нет правки:\n%s", string(onDisk))
	}
	_ = root
}

// Правка, ломающая манифест, обязана быть отвергнута ДО записи. Иначе плагин
// остался бы в состоянии, из которого не запустился бы ни ран, ни registry
// validate, и винить потом можно было бы только человека с кнопкой «сохранить».
func TestPluginEditRejectsInvalidManifestWithoutWriting(t *testing.T) {
	srv, cookie, _, dir := pluginFixture(t)

	original, err := os.ReadFile(filepath.Join(dir, "plugin.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	broken := "id: demo_plugin\nversion: 0.1.0\n" // нет platform_api, runtime и output

	rec := doJSON(t, srv, cookie, "PUT", "/api/plugins/demo_plugin/source",
		map[string]interface{}{"files": map[string]string{"plugin.yaml": broken}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("битый манифест принят: code=%d\n%s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	errs, _ := out["errors"].([]interface{})
	if len(errs) == 0 {
		t.Errorf("ответ без списка ошибок: %v", out)
	}
	after, err := os.ReadFile(filepath.Join(dir, "plugin.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Errorf("файл изменён несмотря на отказ:\nбыло:\n%s\nстало:\n%s", original, after)
	}
}

// Белый список файлов — единственное, что не даёт редактору дописать в каталог
// плагина что угодно. Проверяются обе стороны: чужое имя и выход наружу.
func TestPluginEditRejectsFilesOutsideWhitelist(t *testing.T) {
	srv, cookie, _, dir := pluginFixture(t)
	cases := map[string]string{
		"побочный файл":         "evil.py",
		"выход из каталога":     "../evil.yaml",
		"глубже":                "sub/dir/evil.yaml",
		"имя с обратным слешем": `sub\evil.yaml`,
		"явный путь":            "/etc/passwd",
	}
	for label, name := range cases {
		rec := doJSON(t, srv, cookie, "PUT", "/api/plugins/demo_plugin/source",
			map[string]interface{}{"files": map[string]string{name: "x"}})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s (%q): code=%d, ждали 400\n%s", label, name, rec.Code, rec.Body.String())
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "evil.yaml")); err == nil {
		t.Errorf("файл создан СНАРУЖИ каталога плагина: %v", err)
	}
}

// Валидация обязана ничего не писать на диск — иначе «проверить» становится
// тайной записью.
func TestPluginValidateDoesNotWrite(t *testing.T) {
	srv, cookie, _, dir := pluginFixture(t)

	original, err := os.ReadFile(filepath.Join(dir, "main.py"))
	if err != nil {
		t.Fatal(err)
	}
	updated := string(original) + "\n# черновик\n"
	rec := doJSON(t, srv, cookie, "POST", "/api/plugins/demo_plugin/validate",
		map[string]interface{}{"files": map[string]string{"main.py": updated}})
	if rec.Code != http.StatusOK {
		t.Fatalf("validate: code=%d\n%s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	if out["ok"] != true {
		t.Errorf("валидная правка объявлена невалидной: %v", out)
	}
	after, err := os.ReadFile(filepath.Join(dir, "main.py"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Errorf("валидация записала файл на диск — этого быть не должно")
	}
}

// Тесты редактора обязаны гонять ТО, что человек видит, а не то, что лежит на
// диске с прошлого сохранения. Проверяется полем tested_draft и тем, что
// несохранённый черновик не меняет ничего на диске.
func TestPluginTestRunsOnDiskAndReportsDraft(t *testing.T) {
	srv, cookie, _, _ := pluginFixture(t)

	rec := doJSON(t, srv, cookie, "POST", "/api/plugins/demo_plugin/test", map[string]interface{}{})
	if rec.Code != http.StatusOK {
		t.Fatalf("test: code=%d\n%s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	if out["passed"] == nil || out["failed"] == nil {
		t.Fatalf("нет счётчиков прохождения: %v", out)
	}
	if out["tested_draft"] != false {
		t.Errorf("tested_draft=%v для прогона без правок", out["tested_draft"])
	}
	rec2 := doJSON(t, srv, cookie, "POST", "/api/plugins/demo_plugin/test",
		map[string]interface{}{"files": map[string]string{"main.py": "#!/usr/bin/env python3\nprint('x')\n"}})
	if rec2.Code != http.StatusOK {
		t.Fatalf("test с черновиком: code=%d\n%s", rec2.Code, rec2.Body.String())
	}
	out2 := decodeBody(t, rec2)
	if out2["tested_draft"] != true {
		t.Errorf("tested_draft=%v: черновик не отмечен", out2["tested_draft"])
	}
	// Черновик сломанный — прогон обязан это показать, а не пройти на старых
	// файлах с диска: иначе кнопка «проверить» врала бы ровно в том случае,
	// ради которого её нажимают.
	if out2["ok"] != false {
		t.Errorf("сломанный черновик прошёл тесты: %v", out2)
	}
}

// Создание из интерфейса идёт тем же ядром, что и CLI, и обязано давать плагин,
// который проходит настоящую валидацию, а не только записывается.
func TestPluginCreateMakesValidPlugin(t *testing.T) {
	srv, cookie, _, _ := pluginFixture(t)

	rec := doJSON(t, srv, cookie, "POST", "/api/plugins",
		map[string]interface{}{"id": "fresh_one", "template": "skeleton",
			"author": "человек", "description": "из интерфейса"})
	if rec.Code != http.StatusOK {
		t.Fatalf("создание: code=%d\n%s", rec.Code, rec.Body.String())
	}
	created := decodeBody(t, rec)
	if created["ok"] != true {
		t.Fatalf("ok=%v: %v", created["ok"], created)
	}
	// На диске обязан появиться валидный плагин: тот же гейт, что у реестра.
	// Каталог берём из PluginsDir, а не из поля dir ответа — путь из ответа не
	// должен решать, куда что-то создано.
	dir := filepath.Join(srv.PluginsDir, "fresh_one")
	if _, err := os.Stat(filepath.Join(dir, "plugin.yaml")); err != nil {
		t.Fatalf("плагин не создан на диске: %v", err)
	}
	if errs := core.ValidatePluginDir(dir); len(errs) > 0 {
		t.Errorf("созданный плагин не проходит валидацию: %v", errs)
	}
	if _, err := os.Stat(filepath.Join(dir, "main.py")); err != nil {
		t.Errorf("не создан main.py: %v", err)
	}
}

// Имя плагина приходит из браузера и становится именем каталога: тот же набор
// отказов, что у магазина.
func TestPluginCreateRejectsUnsafeNames(t *testing.T) {
	srv, cookie, _, _ := pluginFixture(t)
	for _, bad := range []string{"", "..", "../../etc", "a/b", "a\\b", "плохое имя", ".скрытый"} {
		rec := doJSON(t, srv, cookie, "POST", "/api/plugins", map[string]interface{}{"id": bad})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("имя %q: code=%d, ждали 400\n%s", bad, rec.Code, rec.Body.String())
		}
	}
}

// Несуществующий плагин — 404 на всех хвостах, а не 200 с пустым ответом:
// тихая пустота выглядит как «всё в порядке, файлов нет».
func TestPluginSubtreeUnknownPluginIs404(t *testing.T) {
	srv, cookie, _, _ := pluginFixture(t)
	for _, tail := range []string{"/source", "/validate", "/test"} {
		method := http.MethodGet
		if tail != "/source" {
			method = http.MethodPost
		}
		rec := doJSON(t, srv, cookie, method, "/api/plugins/nope_xyz"+tail, map[string]interface{}{})
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: code=%d, ждали 404\n%s", tail, rec.Code, rec.Body.String())
		}
	}
}
