package main

// Генератор встроенного allow-list (internal/plugin/trustseed_gen.go).
//
// Запуск (из корня репозитория):
//
//	go run ./internal/plugin/cmd/genseed            # перезаписать
//	go run ./internal/plugin/cmd/genseed -check     # только проверить (CI)
//
// Откуда берутся хэши — по одному на КАЖДУЮ запись реестра, и это важно.
//
// Доверие даётся не по имени, а по хэшу содержимого, поэтому у плагина может
// быть столько доверенных состояний, сколько у него выпусков. Источник этих
// состояний — пины реестра (registry.yaml: version + commit). Для каждой записи
// дерево по пину разворачивается во временный каталог и считается ContentDigest.
//
// Именно по пину, а не по HEAD: registry.yaml — это то, что реально уедет
// пользователю, и доверенным должен быть ровно тот байт-в-байт код, который
// развернёт `wedra plugin install`. Хэш HEAD-плагина был бы доверием к
// содержимому, которого в реестре нет.
//
// Клон НЕ делается по сети: пины лежат в истории этого же репозитория, и CI
// специально checkout-ит с fetch-depth: 0 (см. .github/workflows/ci.yml).
// Генератор читает их через `git archive` — операция без состояния и без
// сети, поэтому один и тот же пин даёт один и тот же хэш на любой машине.
//
// Записи сгруппированы по пину, и на коммит делается ОДИН проход распаковки,
// после чего хэш считается для всех записей этого коммита. Это не «почти тот
// же» расчёт, а буквально тот же: разворачиваемый tar по пину один и тот же,
// распаковка детерминирована (права намеренно не восстанавливаются, symlink
// пропускаются в обоих режимах), а ContentDigest — функция ровно от байтов
// поддерева плагина, от путей внутри него и от их сортировки. Ни размер
// развёрнутого дерева, ни число записей в группе в эту функцию не входят, так
// что 99 развёрток и 11 дают побайтово один и тот же trustseed_gen.go.
//
// Без группировки на каждую из 99 записей приходились свой `git archive` всего
// дерева (8 МБ, ~860 файлов) и своя распаковка — при том, что уникальных
// пинов всего 11. Прогон занимал минуты и рядом с пакетами тестов выглядел
// зависшим. Теперь распаковок столько, сколько уникальных пинов, а по каждому
// пину печатается прогресс.
//
// Что НЕ попадает в seed:
//   - тестовые фикстуры (internal/core/testdata, conformance/fixtures) — это не
//     плагины продукта, и их доверие выдаётся явно в тестах;
//   - community-плагины, которых нет в registry.yaml: доверенным может быть
//     только то, за что реестр отвечает.
//
// Файл перезаписывается целиком. Правки руками теряются — это генератор.

import (
	"archive/tar"
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wykserdex/wedra/internal/plugin"
	"github.com/wykserdex/wedra/internal/registry"
)

const outPath = "internal/plugin/trustseed_gen.go"

func main() {
	repo := flag.String("repo", ".", "корень репозитория")
	check := flag.Bool("check", false, "только проверить, что файл совпадает (для CI)")
	flag.Parse()

	reg, err := registry.Load(filepath.Join(*repo, registry.RegistryFile))
	if err != nil {
		fail("загрузить реестр: %v", err)
	}
	defer reg.Close()

	names := reg.PluginNames()
	sort.Strings(names)

	tmp, err := os.MkdirTemp("", "wedra-genseed-*")
	if err != nil {
		fail("временный каталог: %v", err)
	}
	defer os.RemoveAll(tmp)

	seeds := map[string][]string{}

	// Записи с одним пином разворачиваются вместе: развёртка — это функция от
	// пина, а не от записи, поэтому плагины одного коммита берут хэш из одного
	// развёрнутого дерева (см. обоснование в шапке файла).
	byPin := map[string][]pinEntry{}
	var pins []string
	for _, name := range names {
		entry, _ := reg.GetPlugin(name)
		if err := registry.ValidateEntry(entry, true); err != nil {
			fail("запись %s: %v", name, err)
		}
		if entry.Commit == "" {
			// Локальный source без пина: доверенного выпуска не существует, и
			// придумывать его нельзя. Пропускаем молча — запись без commit
			// ValidateEntry выше уже отверг для удалённых source.
			continue
		}
		if _, seen := byPin[entry.Commit]; !seen {
			pins = append(pins, entry.Commit)
		}
		byPin[entry.Commit] = append(byPin[entry.Commit], pinEntry{name: name, path: entry.Path})
	}
	sort.Strings(pins)

	started := time.Now()
	for i, pin := range pins {
		group := byPin[pin]
		dir := filepath.Join(tmp, pin)
		pinStart := time.Now()
		if err := extractAt(*repo, pin, pluginPaths(group), dir); err != nil {
			fail("развернуть %s: %v", pin, err)
		}
		fmt.Fprintf(os.Stderr, "пин %d/%d %s: развёрнуто, %d записей, %.1f с (всего %.1f с)\n",
			i+1, len(pins), shortPin(pin), len(group), time.Since(pinStart).Seconds(), time.Since(started).Seconds())
		for _, e := range group {
			digest, err := plugin.ContentDigest(filepath.Join(dir, filepath.FromSlash(e.path)))
			if err != nil {
				fail("хеш %s@%s (%s): %v", e.name, pin, e.path, err)
			}
			seeds[e.name] = append(seeds[e.name], digest)
		}
		// Каталог коммита больше не нужен: следующий пин разворачивается в свой.
		// Ошибку удаления глушим намеренно — это временный каталог, и падение
		// из-за него уронило бы прогон, который отработал правильно.
		_ = os.RemoveAll(dir)
	}

	var b strings.Builder
	b.WriteString(header)
	b.WriteString("var builtinTrusted = map[string][]string{\n")
	for _, id := range sortedKeys(seeds) {
		fmt.Fprintf(&b, "\t%q: {\n", id)
		for _, d := range dedupe(seeds[id]) {
			fmt.Fprintf(&b, "\t\t%q,\n", d)
		}
		b.WriteString("\t},\n")
	}
	b.WriteString("}\n")

	target := filepath.Join(*repo, outPath)
	if *check {
		cur, err := os.ReadFile(target)
		if err != nil {
			fail("прочитать %s: %v", outPath, err)
		}
		if string(cur) != b.String() {
			fail("%s устарел: перегенерируй (go run ./internal/plugin/cmd/genseed)", outPath)
		}
		fmt.Println("seed совпадает с пинами реестра")
		return
	}
	if err := os.WriteFile(target, []byte(b.String()), 0o644); err != nil {
		fail("записать %s: %v", outPath, err)
	}
	fmt.Printf("%s: %d плагинов, %d хэшей\n", outPath, len(seeds), countHashes(seeds))
}

// pinEntry — запись реестра, ждущая своего пина.
type pinEntry struct {
	name string
	path string
}

// pluginPaths — поддеревья плагинов группы, без повторов.
func pluginPaths(group []pinEntry) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range group {
		if p := strings.TrimSpace(e.path); p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

func shortPin(pin string) string {
	if len(pin) > 12 {
		return pin[:12]
	}
	return pin
}

// extractAt — развернуть в dst то, из чего считаются хэши записей пина pin.
//
// `git archive` вместо checkout: не трогает состояние рабочего копии вызывающей
// машины, не требует сети и не зависит от настроек core.autocrlf — а значит,
// хэш не «поплывёт» от того, где и как запустили генератор.
//
// paths — только поддеревья плагинов этой группы. Разворачивать всё дерево
// ради хэшей, которые считаются по поддереву, было лишней работой: tar на всю
// репозиторий это ~8 МБ и ~860 файлов, на группу плагинов — сотни КБ. На байты
// поддеревья это не влияет (untar кладёт ровно те файлы, что лежат по пину в
// этом поддереве), поэтому хэш остаётся тем же.
//
// Если paths не пуст, а `git archive` с ними не справляется (путь не найден на
// этом пине, экзотический путь, который git не понимает как pathspec) —
// разворачиваем всё дерево, как раньше. Это страховка, а не оптимизация:
// определение «не нашлось» должно остаться прежним, то есть прийти от
// ContentDigest на отсутствующем каталоге.
func extractAt(repo, pin string, paths []string, dst string) error {
	if len(paths) > 0 {
		if err := archiveInto(repo, pin, paths, dst); err == nil {
			return nil
		}
	}
	return archiveInto(repo, pin, nil, dst)
}

// archiveInto — `git archive` пина (при непустых paths — только этих поддеревьев)
// в dst. pathspec задан литерально (`:(literal)`), иначе путь вроде `a[1].py`
// был бы шаблоном и тихо развернул бы чужое поддерево.
func archiveInto(repo, pin string, paths []string, dst string) error {
	args := []string{"-C", repo, "archive", "--format=tar", pin}
	if len(paths) > 0 {
		args = append(args, "--")
		for _, p := range paths {
			args = append(args, ":(literal)"+filepath.ToSlash(p))
		}
	}
	cmd := exec.Command("git", args...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git archive %s: %v: %s", shortPin(pin), err, strings.TrimSpace(stderr.String()))
	}
	return untar(bytes.NewReader(stdout.Bytes()), dst)
}

// untar — распаковать tar в dst. Права не восстанавливаются намеренно: хэш
// содержимого от них не зависит, а молчаливое изменение прав при распаковке
// сделало бы генератор зависимым от umask машины.
func untar(r io.Reader, dst string) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(filepath.FromSlash(hdr.Name))
		if name == "." || strings.HasPrefix(name, "..") {
			continue
		}
		target := filepath.Join(dst, name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		default:
			// symlink и прочее пропускаем: ContentDigest такой каталог всё
			// равно отверг бы, а распаковка symlink в temp — лишний риск.
		}
	}
}

func sortedKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

func countHashes(m map[string][]string) int {
	n := 0
	for _, v := range m {
		n += len(v)
	}
	return n
}

func fail(format string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, "genseed: "+format+"\n", a...)
	os.Exit(1)
}

var header = `package plugin

// Code generated by internal/plugin/cmd/genseed. DO NOT EDIT.
//
// Встроенный allow-list: плагины, содержимое которых совпадает с пин-ом
// реестра (registry.yaml), доверены без записи в конфиге оператора. Он нужен
// для совместимости: до инверсии плагин считался доверенным по умолчанию, и без
// засева реестровые плагины стали бы untrusted, а вне Linux (где изолятора
// нет) перестали бы работать вовсе.
//
// Что это значит для пользователя: доверенность проверяется по хэшу
// содержимого, поэтому подмена файла в установленном плагине понижает
// доверие, даже если id остался прежним. Community-плагин, которого нет в
// registry.yaml, доверен не будет — для него нужна запись в
// wedra-trust.yaml.

`
