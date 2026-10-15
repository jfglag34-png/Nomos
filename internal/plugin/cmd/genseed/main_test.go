package main

// Тесты ветки отката (extractAt) и её последствий для засева.
//
// Оптимизация «разворачивать только поддеревья плагинов» держится на страховке:
// если `git archive <pin> -- :(literal)<поддерево>` не срабатывает, генератор
// обязан развернуть ПОЛНОЕ дерево — ровно то, что он делал до оптимизации.
// Иначе сломались бы две вещи сразу: запись реестра, чей путь на пине не
// нашёлся, получила бы «нет каталога» вместо хэша, а плагин, лежащий рядом на
// том же пине, — вообще ничего.
//
// Чего тесты НЕ делают: не дублируют логику генератора. Хэш-эталон считается
// тем же ContentDigest, но по развёртке, сделанной честным полным деревом
// (`git archive <pin>` без pathspec) — это буквально то, что делал генератор до
// оптимизации. Команда же запускается как команда: те же аргументы, тот же код
// выхода, тот же stderr.
//
// Каждый тест сначала проверяет, что откат вообще ДОЛЖЕН был сработать
// (`git archive` с этим pathspec падает). Без такой проверки тест был бы пустым:
// он проходил бы и на коде без отката.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/wykserdex/wedra/internal/plugin"
	"github.com/wykserdex/wedra/internal/registry"
)

const (
	// pluginPath — поддерево плагина, лежащее на пине.
	pluginPath = "plugins/demo"
	// ghostPath — поддерево, которого на пине нет: запись реестра ссылается на
	// то, чего в этом коммите ещё не было (переименование, переезд, опечатка).
	ghostPath = "plugins/ghost"
	// fixtureSource — источник «удалённый», и это неслучайно: генератор обязан
	// брать пины из локальной истории и НЕ ходить за плагином в сеть. Репозиторий
	// теста офлайновый, поэтому источник недостижим — если генератор однажды
	// начнёт его клонировать, тест упадёт на отсутствии сети, а не пройдёт.
	fixtureSource = "https://example.invalid/wedra.git"
	// digestPrefix — форма ContentDigest. Хэш, который не начинается с него, —
	// это уже не хэш, и засев с таким «доверием» опаснее его отсутствия.
	digestPrefix = "sha256:"
)

// fixture — подставной репозиторий: коммит с плагином, его SHA и есть пин.
type fixture struct {
	repo string
	pin  string
}

// newFixture — репозиторий с одним плагином и посторонним файлом в корне.
//
// README.md нужен не для красоты: он лежит ВНЕ поддерева плагина, поэтому
// присутствие после отката отличает «развёрнуто полное дерево» от «развёрнуто
// поддерево». Это единственный признак, по которому видно, какая из двух веток
// extractAt отработала.
//
// Каталог internal/plugin создаётся заранее: иначе проверка «seed не записан» в
// тесте отсутствующего плагина ничего не значила бы — записи некуда было бы
// класть даже при попытке.
func newFixture(t *testing.T) fixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git недоступен, фикстура не собирается: %v", err)
	}
	repo := t.TempDir()
	runGit(t, repo, "init", "-q", "-b", "main")
	runGit(t, repo, "config", "user.email", "genseed@test")
	runGit(t, repo, "config", "user.name", "genseed")
	// Байты в архиве = байты в коммите: иначе core.autocrlf машины молча
	// переписал бы их, и эталонный хэш «плавал» бы вместе с настройкой.
	runGit(t, repo, "config", "core.autocrlf", "false")
	runGit(t, repo, "config", "commit.gpgsign", "false")

	write(t, filepath.Join(repo, pluginPath, "main.py"), "def run(**kwargs):\n    return {\"ok\": True}\n")
	write(t, filepath.Join(repo, pluginPath, "plugin.yaml"), "name: demo\nversion: 0.1.0\nruntime:\n  language: python\n  entry: main.py\n")
	write(t, filepath.Join(repo, "README.md"), "# fixture\n\nНе плагин: после отката на полное дерево\nэтот файл обязан появиться.\n")
	if err := os.MkdirAll(filepath.Join(repo, filepath.FromSlash(filepath.Dir(outPath))), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "-A", ".")
	runGit(t, repo, "commit", "-q", "-m", "fixture")

	return fixture{repo: repo, pin: runGit(t, repo, "rev-parse", "HEAD")}
}

// entry — запись реестра на пин фикстуры.
func (fx fixture) entry(name, path string) registry.Entry {
	return registry.Entry{Source: fixtureSource, Path: path, Version: "v1", Commit: fx.pin}
}

// writeRegistry — registry.yaml в корне фикстуры, НЕ закоммиченный. Пины
// генератор берёт из истории, а реестр читает с диска, поэтому файл можно менять
// между прогонами, не сдвигая пин: единственной переменной остаётся путь.
//
// Реестр собирается настоящими типами registry.Entry и сразу проверяется
// настоящим парсером: фикстура, которую генератор не может прочитать, должна
// падать здесь, а не превращаться в «генератор молча ничего не посчитал».
func (fx fixture) writeRegistry(t *testing.T, entries map[string]registry.Entry) {
	t.Helper()
	doc := struct {
		Version string                    `yaml:"version"`
		Plugins map[string]registry.Entry `yaml:"plugins"`
	}{Version: registry.FormatVersion, Plugins: entries}
	raw, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(fx.repo, registry.RegistryFile)
	write(t, path, string(raw))
	handle, err := registry.Load(path)
	if err != nil {
		t.Fatalf("фикстурный реестр не проходит настоящий парсер:\n%s\n%v", raw, err)
	}
	defer handle.Close()
	if len(handle.Registry.Plugins) != len(entries) {
		t.Fatalf("в реестре %d записей, ожидалось %d", len(handle.Registry.Plugins), len(entries))
	}
}

// fullTree — развёртывание полного дерева, то есть ДО оптимизации.
func (fx fixture) fullTree(t *testing.T) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "full")
	if err := archiveInto(fx.repo, fx.pin, nil, dst); err != nil {
		t.Fatalf("развернуть полное дерево пина: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "README.md")); err != nil {
		t.Fatalf("эталон не похож на полное дерево: %v", err)
	}
	return dst
}

// referenceDigest — эталонный хэш поддерева: ровно тот, что дало бы развёртывание
// полного дерева. Сравнение с ним и есть проверка «откат не меняет смысл».
func referenceDigest(t *testing.T, full, rel string) string {
	t.Helper()
	dir := filepath.Join(full, filepath.FromSlash(rel))
	digest, err := plugin.ContentDigest(dir)
	if err != nil {
		t.Fatalf("эталонный хэш %s: %v", rel, err)
	}
	if !strings.HasPrefix(digest, digestPrefix) || len(digest) != len(digestPrefix)+64 {
		t.Fatalf("эталонный хэш %s — не sha256: %q", rel, digest)
	}
	return digest
}

// ШАГ 1, ветка отката: pathspec, которого нет на пине.
//
// Первое утверждение — антивакуумное: если `git archive` с таким pathspec вдруг
// отработает, откат не будет проверен вообще, и тест обязан упасть на этом, а
// не пройти на первой же проверке.
func TestExtractAtFallsBackToFullTree(t *testing.T) {
	fx := newFixture(t)
	paths := []string{pluginPath, ghostPath}

	// Путь из группы отсутствует на пине — git об этом скажет, и именно на этом
	// строится вся оптимизация.
	subtree := filepath.Join(t.TempDir(), "subtree")
	if err := archiveInto(fx.repo, fx.pin, paths, subtree); err == nil {
		t.Fatalf("git archive %v не упал: откат не был проверен, тест пуст", paths)
	}
	if _, err := os.Stat(subtree); !os.IsNotExist(err) {
		t.Fatalf("поддерево развернулось вопреки ошибке git archive (быстрый путь отработал вместо отката): %v", err)
	}

	// ШАГ 2: откат обязан отработать и не упасть.
	got := filepath.Join(t.TempDir(), "fallback")
	if err := extractAt(fx.repo, fx.pin, paths, got); err != nil {
		t.Fatalf("откат на полное дерево не сработал: %v", err)
	}
	if _, err := os.Stat(filepath.Join(got, "README.md")); err != nil {
		t.Fatalf("развёрнуто не полное дерево (нет README.md вне поддерева): %v", err)
	}

	// ШАГ 3 (б): смысл тот же, что у развёртывания полного дерева.
	full := fx.fullTree(t)
	if want, got := referenceDigest(t, full, pluginPath), mustDigest(t, got, pluginPath); got != want {
		t.Fatalf("откат изменил хэш %s:\n откат: %q\n полное дерево: %q", pluginPath, got, want)
	}

	// ШАГ 4 (в): «не нашлось» осталось прежним. Откат развернул полное дерево, а
	// плагина ghost нет и в нём — значит, ошибка приходит от ContentDigest, а не
	// от неудачного git archive, и текст тот же, что был до оптимизации.
	gotErr := digestErr(t, got, ghostPath)
	wantErr := digestErr(t, full, ghostPath)
	if !errors.Is(gotErr, plugin.ErrDigestUnavailable) {
		t.Fatalf("отсутствующий поддеревьевый путь должен давать ErrDigestUnavailable, получено: %v", gotErr)
	}
	if osReason(gotErr, ghostPath) != osReason(wantErr, ghostPath) {
		t.Fatalf("текст ошибки после отката разошёлся с полным деревом:\n откат:  %s\n полное: %s",
			osReason(gotErr, ghostPath), osReason(wantErr, ghostPath))
	}
}

// Контроль к шагу 1: когда путь на пине есть, откат не нужен и git справляется
// сам. Без этого теста проверка «после отката развёрнуто полное дерево» ничего
// не значила бы — тот же результат дал бы и быстрый путь, и тест был бы пустым.
func TestArchiveIntoPathsSucceedsWithoutFallback(t *testing.T) {
	fx := newFixture(t)
	subtree := filepath.Join(t.TempDir(), "subtree")
	if err := archiveInto(fx.repo, fx.pin, []string{pluginPath}, subtree); err != nil {
		t.Fatalf("путь есть на пине, git archive обязан был справиться: %v", err)
	}
	if _, err := os.Stat(filepath.Join(subtree, pluginPath, "main.py")); err != nil {
		t.Fatalf("поддерево не развёрнуто: %v", err)
	}
	if _, err := os.Stat(filepath.Join(subtree, "README.md")); !os.IsNotExist(err) {
		t.Fatalf("развёрнуто полное дерево, а не поддерево: быстрый путь и откат неразличимы (%v)", err)
	}
}

// (а)+(б) на уровне команды: запись, чей путь git archive на пине не находит,
// всё равно получает хэш — тот же, что у честного развёртывания полного дерева,
// и засев выходит побайтово тем же, что без отката.
//
// Триггер отката здесь — путь, отличающийся от пина ТОЛЬКО регистром: pathspec в
// git регистр учитывает всегда, а каталог на регистронезависимой ФС находится.
// Это и есть содержательный случай страховки (регистр в реестре разъехался с
// деревом), а не искусственный.
//
// На регистрозависимой ФС (ext4 в CI) такой путь не нашёлся бы ни при каком
// развёртывании, и сравнивать засев было бы не с чем: тест там пропускается, и
// его смысл несут остальные тесты файла, работающие на любой ФС.
func TestGeneratorSeedUnchangedWhenPathNotFoundOnPin(t *testing.T) {
	if !caseInsensitiveFS(t) {
		t.Skip("ФС различает регистр: путь, отличающийся от пина только регистром, не найдётся ни при каком развёртывании — сравнение засева невозможно")
	}
	fx := newFixture(t)
	exotic := strings.ToUpper(pluginPath)

	// Антивакуумное утверждение: этот pathspec действительно не находится на пине.
	if err := archiveInto(fx.repo, fx.pin, []string{exotic}, filepath.Join(t.TempDir(), "x")); err == nil {
		t.Fatalf("git archive %q не упал: откат не был проверен, тест пуст", exotic)
	}

	// Прогон с путём, которого нет на пине: откат, успех, запись засева.
	fx.writeRegistry(t, map[string]registry.Entry{"demo": fx.entry("demo", exotic)})
	viaFallback := runGenerator(t, fx.repo)
	if viaFallback.err != nil {
		t.Fatalf("откат упал, команда не отработала: %v\n%s", viaFallback.err, viaFallback.stderr)
	}
	if strings.Contains(viaFallback.stderr, "did not match any files") {
		t.Fatalf("ошибка pathspec всплыла наружу — отката не было:\n%s", viaFallback.stderr)
	}
	full := fx.fullTree(t)
	ref := referenceDigest(t, full, pluginPath)
	if !strings.Contains(viaFallback.seed, ref) {
		t.Fatalf("засев после отката не содержит хэша полного дерева %q:\n%s", ref, viaFallback.seed)
	}

	// Прогон с тем же путём, написанным точно: быстрый путь, без отката. Засев
	// обязан совпасть побайтово — иначе откат меняет смысл.
	fx.writeRegistry(t, map[string]registry.Entry{"demo": fx.entry("demo", pluginPath)})
	viaSubtree := runGenerator(t, fx.repo)
	if viaSubtree.err != nil {
		t.Fatalf("контрольный прогон (без отката) упал: %v\n%s", viaSubtree.err, viaSubtree.stderr)
	}

	if viaFallback.seed != viaSubtree.seed {
		t.Fatalf("откат изменил засев:\n--- откат ---\n%s\n--- без отката ---\n%s", viaFallback.seed, viaSubtree.seed)
	}
	if strings.Count(viaSubtree.seed, digestPrefix) != 1 {
		t.Fatalf("в засеве должен быть ровно один настоящий хэш, иначе это «пустой seed»:\n%s", viaSubtree.seed)
	}
	t.Logf("команда отработала: stdout=%q stderr=%q", viaFallback.stdout, viaFallback.stderr)
}

// (в) на уровне команды: плагина нет ни по поддереву, ни в полном дереве —
// значит, должен быть тот же доменный отказ, что и до оптимизации, и НЕ должен
// появиться засев (даже пустой). Откат при этом отработать обязан: полное дерево
// развернулось, дело в отсутствующем плагине.
func TestGeneratorMissingPluginKeepsOldFailure(t *testing.T) {
	fx := newFixture(t)
	fx.writeRegistry(t, map[string]registry.Entry{
		"demo":  fx.entry("demo", pluginPath),
		"ghost": fx.entry("ghost", ghostPath),
	})

	run := runGenerator(t, fx.repo)

	var exitErr *exec.ExitError
	if !errors.As(run.err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("ожидался отказ с кодом 1 (os.Exit в fail), получено %v:\n%s", run.err, run.stderr)
	}
	// Отказ доменный: «не нашлось содержимое», а не «сломался git archive».
	want := fmt.Sprintf("genseed: хеш ghost@%s (%s): %s", fx.pin, ghostPath, plugin.ErrDigestUnavailable.Error())
	if !strings.Contains(run.stderr, want) {
		t.Fatalf("текст ошибки другой — определение «не нашлось» изменилось.\n ждали: %s\n получили: %s", want, run.stderr)
	}
	if strings.Contains(run.stderr, "развернуть") {
		t.Fatalf("откат на полное дерево не отработал, до хэша дело не дошло:\n%s", run.stderr)
	}
	if strings.Contains(run.stderr, "did not match any files") {
		t.Fatalf("ошибка pathspec всплыла наружу — откат её не поглотил:\n%s", run.stderr)
	}
	// Плагин, который на пине ЕСТЬ, посчитан: после отката развернулось полное
	// дерево, и в вину попал только отсутствующий.
	if strings.Contains(run.stderr, "demo@"+fx.pin) {
		t.Fatalf("плагин demo есть на пине и обязан был посчитаться:\n%s", run.stderr)
	}
	// Тот же низкоуровневый текст, что дало бы развёртывание полного дерева.
	full := fx.fullTree(t)
	wantReason := osReason(digestErr(t, full, ghostPath), ghostPath)
	if osReasonString(run.stderr, ghostPath) != wantReason {
		t.Fatalf("причина отказа после отката разошлась с полным деревом:\n команда: %s\n полное: %s",
			osReasonString(run.stderr, ghostPath), wantReason)
	}
	// И главное: никакого засева. Каталог для записи был создан заранее, так
	// что «файла нет» — это решение генератора, а не невозможность записать.
	seed := filepath.Join(fx.repo, filepath.FromSlash(outPath))
	if _, err := os.Stat(seed); !os.IsNotExist(err) {
		t.Fatalf("засев записан при отсутствующем плагине (%v):\n%s", err, run.seed)
	}
	t.Logf("команда отработала: stdout=%q", run.stdout)
}

// generatorRun — результат одного запуска генератора как команды.
type generatorRun struct {
	seed   string // засев, если он записан; "" если файла нет
	stdout string
	stderr string
	err    error
}

// runGenerator — генератор как команда: `go run ./internal/plugin/cmd/genseed
// -repo <фикстура>`. Именно на этом уровне видны «команда упала», «записан пустой
// seed» и текст ошибки, ради которых страховка и написана. Поддерево генератора
// берётся из этого репозитория, а -repo указывает на подставной: пины читаются
// из истории фикстуры, сеть не нужна.
func runGenerator(t *testing.T, repo string) generatorRun {
	t.Helper()
	cmd := exec.Command("go", "run", "./internal/plugin/cmd/genseed", "-repo", repo)
	cmd.Dir = wedraRoot(t)
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	run := generatorRun{stdout: outBuf.String(), stderr: withoutGoRunDiag(errBuf.String()), err: err}
	raw, readErr := os.ReadFile(filepath.Join(repo, filepath.FromSlash(outPath)))
	if readErr != nil {
		if !os.IsNotExist(readErr) {
			t.Fatalf("прочитать засев: %v", readErr)
		}
		return run
	}
	run.seed = string(raw)
	return run
}

// withoutGoRunDiag — stderr без служебной строки `go run`. `go run` дописывает в
// stderr «exit status 1» от своего имени, и в сравнении текстов она выглядела бы
// как часть сообщения генератора. Убираем её здесь, чтобы дальше сравнивался
// ровно тот вывод, который пишет генератор.
func withoutGoRunDiag(stderr string) string {
	lines := strings.Split(stderr, "\n")
	kept := lines[:0]
	for _, line := range lines {
		if strings.HasPrefix(line, "exit status ") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// caseInsensitiveFS — различает ли файловая система регистр.
func caseInsensitiveFS(t *testing.T) bool {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "case-probe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "marker.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := os.Stat(filepath.Join(dir, "MARKER.TXT"))
	return err == nil
}

// digestErr — ContentDigest на отсутствующем поддереве, как его вызывает main.
func digestErr(t *testing.T, dst, rel string) error {
	t.Helper()
	_, err := plugin.ContentDigest(filepath.Join(dst, filepath.FromSlash(rel)))
	if err == nil {
		t.Fatalf("поддерево %s есть в %s, а тест ждал отказа", rel, dst)
	}
	return err
}

// mustDigest — хэш поддерева в развёртке.
func mustDigest(t *testing.T, dst, rel string) string {
	t.Helper()
	digest, err := plugin.ContentDigest(filepath.Join(dst, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("хеш %s в %s: %v", rel, dst, err)
	}
	return digest
}

// osReason — причина отказа от ContentDigest: текст ОС («файл не найден» и т.п.).
// Каталог в сообщении встречается дважды и у генератора, и у эталона свой, поэтому
// сравнивается только то, что от каталога не зависит. Иначе равенство текстов
// означало бы сравнение двух временных путей, а не смыслов.
func osReason(err error, rel string) string {
	return osReasonString(err.Error(), rel)
}

func osReasonString(msg, rel string) string {
	tail := msg
	if i := strings.LastIndex(tail, filepath.FromSlash(rel)); i >= 0 {
		tail = tail[i+len(filepath.FromSlash(rel)):]
	}
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(tail), ":"))
}

// wedraRoot — корень этого репозитория (где go.mod), а не фикстуры.
func wedraRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("выше рабочего каталога пакета нет go.mod — нечего запускать")
		}
		dir = parent
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
