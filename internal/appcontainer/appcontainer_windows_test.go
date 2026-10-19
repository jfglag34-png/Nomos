//go:build windows

package appcontainer

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// TestRunIsolatesExternalCode — главный поведенческий тест пакета.
//
// Он проверяет не «процесс создался», а ровно те свойства, ради которых
// изолятор существует. Всё измеряется ОТ ЛИЦА РЕБЁНКА внутри контейнера: если
// смотреть снаружи, все проверки проходят тривиально, потому что ограничения
// применяются к другому токену.
//
// Тест пропускается, если рядом нет настоящего интерпретатора: на CI-раннере
// Python живёт не под %LOCALAPPDATA%\Python\pythoncore-*, и без него проверять
// нечего. Молчаливый «прогон без проверки» здесь опаснее явного скипа.
func TestRunIsolatesExternalCode(t *testing.T) {
	py := testRealPython(t)

	root := t.TempDir()
	pluginDir := filepath.Join(root, "plugin")
	scratch := filepath.Join(root, "scratch")
	denied := filepath.Join(root, "denied")
	for _, d := range []string{pluginDir, scratch, denied} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Файл в каталоге плагина обязателен: без него «прочитал» и «недостижим»
	// дают одинаковый пустой список каталога, и проверка на чтение проходит вхолостую.
	if err := os.WriteFile(filepath.Join(pluginDir, "data.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// В denied кладём файл, чтобы отказ в чтении был осмысленным.
	if err := os.WriteFile(filepath.Join(denied, "secret.txt"), []byte("s"), 0o644); err != nil {
		t.Fatal(err)
	}

	script := filepath.Join(pluginDir, "probe.py")
	if err := os.WriteFile(script, []byte(testProbeScript), 0o644); err != nil {
		t.Fatal(err)
	}

	plan := Plan{
		Name:    ProfileName("probe-" + strings.ReplaceAll(t.Name(), "/", "-")),
		Argv:    []string{py, script},
		Read:    Readable{pluginDir, filepath.Dir(py)},
		Scratch: scratch,
		WorkDir: "",
		Env:     []string{"PLUGIN_DIR=" + pluginDir, "SCRATCH=" + scratch, "DENIED=" + denied, "OUT=" + filepath.Join(scratch, "out.json")},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	res, err := Run(ctx, plan)
	if err != nil {
		t.Fatalf("запуск в контейнере: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("контейнеризованный процесс закончился кодом %d", res.ExitCode)
	}

	raw, err := os.ReadFile(filepath.Join(scratch, "out.json"))
	if err != nil {
		t.Fatalf("ребёнок не записал результат: %v", err)
	}
	t.Logf("измерения ребёнка:\n%s", raw)

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("результат не разобран: %v\n%s", err, raw)
	}
	// Разбором, а не поиском подстроки: скрипт печатает JSON с отступами, и
	// подстрочная проверка вида `"socket":"DENIED"` не сработала бы никогда.
	want := []struct{ name, key, val string }{
		{"каталог плагина читается", "plugin_list_len", ""},
		{"каталог плагина только для чтения", "plugin_write", "PermissionError"},
		{"scratch доступен на запись", "scratch_write", "OK"},
		{"каталог без выдачи недостижим", "denied_read", "PermissionError"},
		{"исходящая сеть запрещена", "socket", "DENIED"},
	}
	for _, c := range want {
		if c.key == "plugin_list_len" {
			// Проверяем отдельно: длина списка, а не строка.
			n, _ := got[c.key].(float64)
			if n < 1 {
				t.Errorf("НЕ ВЫПОЛНЕНО: %s: %s=%v, ожидалось не меньше 1\n%s", c.name, c.key, got[c.key], raw)
			}
			continue
		}
		v, _ := got[c.key].(string)
		if v != c.val {
			t.Errorf("НЕ ВЫПОЛНЕНО: %s: %s=%q, ожидалось %q\n%s", c.name, c.key, v, c.val, raw)
		}
	}
}

// TestPrepareIsIdempotent — выдача прав обязана быть повторяемой.
//
// Профиль на плагин переживает запуски, а Prepare вызывается на каждом прогоне,
// поэтому второй вызов на тех же путях обязан быть успешным и безвредным. Без
// этой проверки повторный запуск падал бы с «доступ запрещён» или размножал бы
// записи в DCL.
func TestPrepareIsIdempotent(t *testing.T) {
	py := testRealPython(t)
	root := t.TempDir()
	scratch := filepath.Join(root, "scratch")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		t.Fatal(err)
	}
	plan := Plan{
		Name:    ProfileName("idem-" + strings.ReplaceAll(t.Name(), "/", "-")),
		Argv:    []string{py, "-c", "pass"},
		Read:    Readable{filepath.Dir(py)},
		Scratch: scratch,
	}
	for i := 1; i <= 2; i++ {
		if err := Prepare(plan); err != nil {
			t.Fatalf("Prepare, попытка %d: %v", i, err)
		}
	}
}

// TestProfileNameIsStableAndSafe — имя профиля обязано быть устойчивым и
// безопасным: от него зависит, переживёт ли выданная ACE переустановку плагина.
func TestProfileNameIsStableAndSafe(t *testing.T) {
	if got := ProfileName("otklik"); got != "wedra.otklik" {
		t.Errorf("ProfileName(otklik) = %q, ожидалось wedra.otklik", got)
	}
	if got := ProfileName("a/b c"); got != "wedra.a_b_c" {
		t.Errorf("ProfileName(a/b c) = %q, ожидалось wedra.a_b_c (разделители путей в имя профиля не идут)", got)
	}
	// Очень длинный id обязан уместиться в ограничение ядра.
	if got := ProfileName(strings.Repeat("x", 300)); len(got) > 64 {
		t.Errorf("имя профиля длиной %d превышает 64: %q", len(got), got)
	}
}

// TestValidateFailsClosed — пустой ввод отвергается, а не превращается в
// запуск «как получится».
func TestValidateFailsClosed(t *testing.T) {
	for name, p := range map[string]Plan{
		"пустая команда": {Name: "wedra.x", Scratch: "s"},
		"нет имени":      {Argv: []string{"x"}, Scratch: "s"},
		"нет scratch":    {Name: "wedra.x", Argv: []string{"x"}},
	} {
		if err := p.Validate(); err == nil {
			t.Errorf("%s: Validate вернул nil, ожидался отказ", name)
		}
	}
}

// testRealPython ищет настоящий интерпретатор.
//
// Именно настоящий, а не bin\python.exe: тот файл — шимб PyManager, который
// перенаправляет запуск в соседний pythoncore-*, и внутри контейнера падает с
// кодом 5, потому что до целевого каталога доступа нет. На поиск шимба ушла
// отдельная проверка, и ошибка выглядит как «контейнер сломан».
func testRealPython(t *testing.T) string {
	t.Helper()
	local := os.Getenv("LOCALAPPDATA")
	if local == "" {
		t.Skip("нет LOCALAPPDATA")
	}
	root := filepath.Join(local, "Python")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Skipf("нет каталога %s: на этой машине проверить нечего", root)
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "pythoncore-") {
			continue
		}
		cand := filepath.Join(root, e.Name(), "python.exe")
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
	}
	t.Skip("нет настоящего интерпретатора под Python\\pythoncore-*")
	return ""
}

// testProbeScript — сценарий ребёнка. Пишет JSON в scratch и НИКОГДА не
// падает: любой отказ превращается в значение, иначе «изоляция нарушена» и
// «скрипт упал» неразличимы.
const testProbeScript = `import json, os, socket

out = {}

def try_write(path):
    try:
        with open(path, "w") as f:
            f.write("x")
        return "OK"
    except Exception as e:
        return type(e).__name__

def try_list(path):
    try:
        return len(os.listdir(path))
    except Exception as e:
        return type(e).__name__

out["plugin_list_len"] = try_list(os.environ["PLUGIN_DIR"])
out["plugin_write"] = try_write(os.path.join(os.environ["PLUGIN_DIR"], "intruder.txt"))
out["scratch_write"] = try_write(os.path.join(os.environ["SCRATCH"], "mine.txt"))
out["denied_read"] = try_list(os.environ["DENIED"])
try:
    s = socket.create_connection(("127.0.0.1", 9), timeout=2)
    s.close()
    out["socket"] = "OK"
except Exception:
    out["socket"] = "DENIED"

with open(os.environ["OUT"], "w") as f:
    f.write(json.dumps(out))
`

// TestEnvBlockHasExactlyTwoTrailingNuls — регрессия на конкретную ошибку.
//
// windows.UTF16FromString добавляет СВОЙ нулевой терминатор. Если поверх него
// положить ещё один, а потом закрыть блок, на стыке появляется лишний ноль,
// то есть переменная с пустым именем. CreateProcessW на таком блоке отвечает
// ERROR_ENVVAR_NOT_FOUND — и это читается как «AppContainer на хосте сломан»,
// хотя до контейнера дело даже не доходит: сломана склейка блока окружения.
//
// Проверка на хвост из РОВНО двух нулей: закрытие последней переменной и
// закрытие блока. Три — регрессия, один — обрезанный блок.
func TestEnvBlockHasExactlyTwoTrailingNuls(t *testing.T) {
	block, err := envBlock([]string{"PLUGIN_DIR=/p", "OUT=/o"})
	if err != nil {
		t.Fatal(err)
	}
	zeros := 0
	for i := len(block) - 1; i >= 0 && block[i] == 0; i-- {
		zeros++
	}
	if zeros != 2 {
		t.Errorf("хвост блока окружения: %d нулей, ожидалось ровно 2", zeros)
	}
	// Обязательные для AppContainer переменные обязаны присутствовать: без них
	// контейнер не выводит путь профиля.
	//
	// Блок разбирается по нулям вручную, а не через windows.UTF16ToString: тот
	// обрезает строку на ПЕРВОМ нуле и вернул бы только «PLUGIN_DIR=/p», из-за
	// чего проверка выглядела бы как «переменных нет», хотя они есть.
	var vars []string
	start := 0
	for i, c := range block {
		if c != 0 {
			continue
		}
		if i > start {
			vars = append(vars, windows.UTF16ToString(block[start:i]))
		}
		start = i + 1
	}
	for _, k := range []string{"USERPROFILE", "LOCALAPPDATA"} {
		found := false
		for _, v := range vars {
			if strings.HasPrefix(v, k+"=") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("в блоке окружения нет %s; разобрано: %v", k, vars)
		}
	}
}
