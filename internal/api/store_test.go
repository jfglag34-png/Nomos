package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// storeRegistryYAML — реестр с одной записью. Оформление повторяет боевой
// registry.yaml (version + plugins + source/path/version/commit/description),
// потому что витрина читает его тем же загрузчиком.
//
// path намеренно plugins/community/demo_plugin_impl, а ключ записи —
// demo_plugin: имя каталога ОТЛИЧАЕТСЯ от имени записи, как в боевом реестре
// (plugins/community/<имя>, а не plugins/<имя>). Пока совпадение было
// случайным, тест проходил и при склейке пути по имени, то есть пропускал
// живой баг: все 99 стоящих плагинов показывались неустановленными.
const storeRegistryYAML = `version: "0.1"
plugins:
  demo_plugin:
    source: https://github.com/wykserdex/wedra
    path: plugins/community/demo_plugin_impl
    version: v0.33c
    commit: 0000000000000000000000000000000000000000
    description: "демонстрационный плагин для витрины"
`

// storePluginYAML — минимальный валидный манифест: без внешнего донора и без
// runtime.requires, поэтому doctor не должен просить ничего доустановить.
const storePluginYAML = `id: demo_plugin
version: 0.1.0
platform_api: "^0.1"
description: "демо"

runtime:
  type: python
  entry: main.py
  requires: []

output:
  value: { type: string }
`

// storeFixture — витрина с корнем рядом с plugins/, где может лежать реестр.
// root это filepath.Dir(PluginsDir) во всём остальном API, и подмена registry
// происходит именно там.
func storeFixture(t *testing.T, withRegistry, withPlugin bool) (*Server, *http.Cookie, string) {
	t.Helper()
	srv, root := h4Server(t, ListenOptions{Addr: testAddr}, "AAAA-BBBB-CCCC")
	if withRegistry {
		if err := os.WriteFile(filepath.Join(root, "registry.yaml"), []byte(storeRegistryYAML), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if withPlugin {
		dir := filepath.Join(root, "plugins", "community", "demo_plugin_impl")
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(storePluginYAML), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte("import json\nprint(json.dumps({'status':'ok','output':{'value':'x'}}))\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return srv, login(t, srv, "AAAA-BBBB-CCCC"), root
}

func storeGet(t *testing.T, srv *Server, cookie *http.Cookie, target string) map[string]interface{} {
	t.Helper()
	if target == "" {
		target = "/api/store"
	}
	rec := h4Request(t, srv, "GET", target, nil, withCookieOf(cookie))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/store: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("ответ витрины не JSON: %v\n%s", err, rec.Body.String())
	}
	return out
}

func withCookieOf(c *http.Cookie) func(*http.Request) {
	return func(r *http.Request) { r.AddCookie(c) }
}

// Реестра нет — витрина обязана сказать об этом прямо, а не отдать «0 плагинов,
// всё хорошо»: пустой список в магазине читается как «у нас ничего нет», а
// здесь реестр просто не найден.
func TestStoreReportsMissingRegistry(t *testing.T) {
	srv, cookie, _ := storeFixture(t, false, false)
	// Замера тут не нужно: реестра нет, мерить нечего. И проверяем, что
	// молчание о неизвестном не выдаётся за успех.
	out := storeGet(t, srv, cookie, "")
	if out["status_measured"] != false {
		t.Errorf("status_measured=%v без ?status=1: молчание выдано за замер", out["status_measured"])
	}

	if out["available"] != false {
		t.Fatalf("available=%v, ждали false при отсутствующем реестре", out["available"])
	}
	reason, _ := out["reason"].(string)
	if !strings.Contains(reason, "registry.yaml") {
		t.Errorf("причина не называет файл реестра: %q", reason)
	}
}

// ГЛАВНЫЙ ИНВАРИАНТ ВИТРИНЫ. Плагин, которого нет на диске, не должен
// выглядеть готовым к запуску.
//
// Раньше doctor отвечал на вопрос «установлен ли пакет ЗДЕСЬ», а витрина
// склонна склеивать «есть в реестре» и «можно запускать». Для записи реестра без
// каталога на диске это разные вещи: донор может стоять, а может и нет, и
// узнать это можно только прогоном. Поэтому у неустановленного плагина статус
// пустой и помечен status_unknown — иначе человек поставит галку «готов» и
// упрётся в ошибку запуска.
func TestStoreDoesNotClaimUninstalledPluginIsReady(t *testing.T) {
	srv, cookie, _ := storeFixture(t, true, false)
	// Именно БЕЗ ?status=1: инвариант в том, что витрина по умолчанию не
	// притворяется знающей готовность того, чего нет на диске. Если бы это
	// требовало явного ?status=1, тест проверял бы опцию, а не поведение.
	out := storeGet(t, srv, cookie, "")

	items, _ := out["plugins"].([]interface{})
	if len(items) != 1 {
		t.Fatalf("в витрине %d записей, ждали 1: %v", len(items), out)
	}
	item, _ := items[0].(map[string]interface{})
	if item["installed"] != false {
		t.Fatalf("installed=%v, ждали false: каталога нет", item["installed"])
	}
	if status, _ := item["status"].(string); status != "" {
		t.Errorf("status=%q у неустановленного плагина: готовность без каталога выдумана", status)
	}
	if item["status_unknown"] != true {
		t.Errorf("status_unknown не выставлен: витрина не признаёт, что не знает: %v", item)
	}
	// Доверие не выдаётся: нечего хэшировать.
	if _, ok := item["trusted"]; ok {
		t.Errorf("неустановленному плагину выдано доверие: %v", item)
	}
}

// Обратная сторона того же инварианта: установленный плагин обязан получить
// измеренный статус. Иначе «готовность» не измеряется, а рисуется.
func TestStoreReportsMeasuredStatusForInstalledPlugin(t *testing.T) {
	srv, cookie, _ := storeFixture(t, true, true)
	out := storeGet(t, srv, cookie, "/api/store?status=1")

	items, _ := out["plugins"].([]interface{})
	if len(items) != 1 {
		t.Fatalf("в витрине %d записей, ждали 1: %v", len(items), out)
	}
	item, _ := items[0].(map[string]interface{})
	if item["installed"] != true {
		t.Fatalf("installed=%v, ждали true", item["installed"])
	}
	if _, ok := item["status_unknown"]; ok {
		t.Errorf("установленному плагину выставлен status_unknown: %v", item)
	}
	status, ok := item["status"].(string)
	if !ok || status == "" {
		t.Fatalf("у установленного плагина нет измеренного статуса: %v", item)
	}
	if status != "ready" {
		t.Errorf("status=%q, а плагин на голом Python без донора обязан быть ready: %v", status, item)
	}
}

// Витрина только читается. POST на неё — не «установить неявно», а ошибка
// метода: установка обязана быть явным действием с явным именем.
func TestStoreRejectsNonGET(t *testing.T) {
	srv, cookie, _ := storeFixture(t, true, false)
	rec := h4Request(t, srv, "POST", "/api/store", strings.NewReader(`{"name":"demo_plugin"}`), withCookieOf(cookie))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/store: code=%d, ждали 405\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "E_METHOD_NOT_ALLOWED") {
		t.Errorf("нет кода E_METHOD_NOT_ALLOWED: %s", rec.Body.String())
	}
}

// Имя плагина попадает в имя каталога и в ключ записи реестра, поэтому
// приходит из тела запроса и проверяется ДО любой работы с файлами.
func TestStoreInstallRejectsUnsafeNames(t *testing.T) {
	srv, cookie, root := storeFixture(t, true, false)

	for _, bad := range []string{
		"", "..", "../../etc", "a/b", "a\\b", "demo plugin", ".hidden", "a..b", "C:evil",
	} {
		body, _ := json.Marshal(map[string]string{"name": bad})
		rec := h4Request(t, srv, "POST", "/api/store/install", strings.NewReader(string(body)), withCookieOf(cookie))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("имя %q: code=%d, ждали 400\n%s", bad, rec.Code, rec.Body.String())
		}
		// Ошибка валидации не должна тащить следы файловой системы.
		if strings.Contains(rec.Body.String(), root) {
			t.Errorf("имя %q: в ответе засветился корень: %s", bad, rec.Body.String())
		}
	}
}
