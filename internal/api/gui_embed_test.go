package api

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestGUIEmbeddedStatic — v0.7: GUI вшит в бинарник (go:embed).
// Тесты гонятся из internal/api — там нет web/static (он в корне репо),
// то есть этот тест проверяет именно release-путь: frontend из embedded FS,
// без диска рядом.
func TestGUIEmbeddedStatic(t *testing.T) {
	ts, _ := gateTestServer(t)

	cases := []struct {
		path string
		want string // подстрока, обязана быть в теле
	}{
		{"/", "WEDRA"},
		{"/index.html", "WEDRA"},
		{"/editor/", "редактор"},
		{"/editor/app.js", "pluginNetworkHint"},
		// Подсказку о том, как связывать шаги, легко потерять при рефакторинге
		// редактора, и заметят это те, кто уже споткнулся. Пином маркер по
		// конвенции остальных кусков UI. v0.36: жест — два щелчка, а панель
		// связи и кнопка запуска обязаны быть в раздаче.
		// Игла здесь — структурная (id элемента), а не проза подсказки: текст
		// «Связи — протяжкой…» менялся вместе с редизайном слоёв, и тест падал на
		// честной правке, а не на битом embedded FS.
		{"/editor/", "id=\"layer-inputs\""},
		// v0.38: режим артефактов — ОТДЕЛЬНАЯ страница. Пины на то, что он
		// есть, и на то, что редактор о нём не знает: смешивать два разных
		// инструмента в один экран уже пробовали, вёрстка поехала.
		{"/artifacts/", "режим артефактов"},
		// v0.34: в артефакты из редактора ведёт раздел в шапке, а не кнопка
		// «Режим артефактов →»: навигация стала разделами. Игла — структурная.
		{"/editor/", "href=\"/artifacts/\""},
		{"/artifacts/", "id=\"drop\""},
		{"/artifacts/app.js", "upload"},
		{"/artifacts/app.js", "buildYAML"},
		{"/artifacts/app.js", "suggest"},
		// v0.34: панель связи (#link-bar) снята — выход выбирается по типам
		// прямо при протяжке. В разметке обязан остаться слой рёбер: без него
		// связь не нарисовать, а это тот же класс поломки.
		{"/editor/", "id=\"edges\""},
		{"/editor/", "btn-run"},
		{"/editor/app.js", "canvas-hint"}, // маркер v0.6 UI
		{"/editor/app.js", "armLink"},
		{"/editor/app.js", "startLinkDrag"},
		{"/editor/app.js", "link-ghost"},
		{"/app.js", "api/health"},
		{"/app.js", "api/presets"},
		// v0.39: магазин — вкладка «Плагины» показывает и то, чего нет на диске.
		// Пины на функции, а не на прозу: текст «не установлен» легко переписать
		// при редизайне, а потеря функции означала бы, что установка из
		// интерфейса молча исчезла.
		{"/app.js", "storeRows"},
		{"/app.js", "renderStoreCard"},
		{"/app.js", "installStorePlugin"},
		{"/app.js", "api/store"},
		{"/index.html", "plug-filter-store"},
		{"/index.html", "plug-health"},
		// v0.39: редактор плагинов — ОТДЕЛЬНАЯ страница, как артефакты: писать
		// плагин и собирать пайплайн — разные инструменты, смешивать их в один
		// экран уже пробовали, вёрстка поехала. Пины на ФУНКЦИИ, а не на прозу.
		{"/plugins/", "Создать плагин"},
		{"/plugins/", "id=\"btn-test\""},
		{"/plugins/app.js", "async function save()"},
		{"/plugins/app.js", "async function runTests()"},
		{"/plugins/app.js", "async function createPlugin"},
		// Подсказка доверия отвечает на вопрос «почему не запускается и что
		// делать». На Windows/macOS без неё редактор упирается в стену.
		{"/plugins/", "id=\"trust-hint\""},
		{"/plugins/app.js", "renderTrustHint"},
	}
	for _, c := range cases {
		resp, err := http.Get(ts.URL + c.path)
		if err != nil {
			t.Fatalf("GET %s: %v", c.path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("GET %s: status=%d (ждём 200 из embedded FS)", c.path, resp.StatusCode)
		}
		if !strings.Contains(string(body), c.want) {
			t.Fatalf("GET %s: нет %q в теле (embedded FS битый?)", c.path, c.want)
		}
	}

	// честный 404 на несуществующее
	resp, err := http.Get(ts.URL + "/definitely_not_there.html")
	if err != nil {
		t.Fatalf("GET 404: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("GET /definitely_not_there.html: status=%d (ждём 404)", resp.StatusCode)
	}
}
