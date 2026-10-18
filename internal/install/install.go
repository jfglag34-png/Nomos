// Package install — ядро установки плагина, которое можно вызвать не из CLI.
//
// Зачем пакет: `wedra plugin install` был единственным входом в установку и
// печатал прогресс в stdout, поэтому вызвать его из HTTP-эндпоинта было
// нельзя — там нужен результат структурой, а печать уходит мимо ответа и
// ломает тесты. Ядро вынесено сюда, internal/cli остался обёрткой, которая
// печатает тот же текст, что и раньше.
//
// Правила пакета:
//
//   - ни одного fmt.Print*: весь прогресс уходит в переданный io.Writer,
//     nil = отбросить. Проверяется тестом на исходниках самого пакета;
//   - точка входа Run возвращает Result, а не только error;
//   - fail-closed поведение прежнее: публикация каталога только после
//     core.ValidatePluginDir, ничего не «best effort».
package install

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wykserdex/wedra/internal/core"
	"github.com/wykserdex/wedra/internal/registry"
)

// DefaultDest — куда ставится плагин, если --dest не задан. Тот же дефолт, что
// был у CLI.
const DefaultDest = "plugins"

// ErrNoRef — в аргументах нет имени плагина. Отдельная ошибка, а не строка:
// CLI по ней печатает подсказку и выходит с кодом 2, а не с 1.
var ErrNoRef = errors.New("не указан плагин")

// Request — что ставим. Поля повторяют флаги `plugin install`, чтобы вызов из
// CLI и вызов из HTTP описывались одинаково.
type Request struct {
	Name        string // имя плагина в реестре
	Version     string // "" = версия из реестра
	RegistrySrc string // "", путь или URL реестра; "" = реестр по умолчанию
	Dest        string // корневой каталог установки; см. DefaultDest

	// LocalSource — локальный чеккаут того же репозитория (тот же флаг
	// --local-source у `registry validate`). Пусто = не задан.
	LocalSource string
}

// Result — чем закончилась установка. Заполняется даже при ошибке настолько,
// насколько успело: HTTP-ответ должен уметь показать, что именно ставилось и
// откуда, даже когда установка сорвалась.
type Result struct {
	Name    string // имя плагина
	Version string // фактически установленная версия (из запроса или реестра)
	Dir     string // куда установился; пусто, если не дошло до публикации
	Source  string // откуда ставил (registry.Entry.Source)
	Commit  string // зафиксированный commit из реестра; "" = пин не задан
	Notes   []string
}

// ParseArgs — разбор аргументов `wedra plugin install <ref> [--registry=…]
// [--dest=…]`. Чистая функция: ни сети, ни git, ни файлов.
//
// Повтор последнего позиционного аргумента побеждает, а неизвестный флаг молча
// пропускается — так было в CLI, и менять это здесь нельзя: разбор переехал
// вместе с ядром, а текст ошибок остался прежним.
func ParseArgs(args []string) (Request, error) {
	req := Request{Dest: DefaultDest}
	ref := ""
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "--registry="):
			req.RegistrySrc = a[len("--registry="):]
		case strings.HasPrefix(a, "--dest="):
			req.Dest = a[len("--dest="):]
		case !strings.HasPrefix(a, "-"):
			ref = a
		}
	}
	if ref == "" {
		return req, ErrNoRef
	}
	req.Name, req.Version = ParseRef(ref)
	return req, nil
}

// ParseRef — «name» или «name@version». Разбор тот же, что у реестра
// (registry.SplitRef): делит последний «@», и «@» в позиции 0 разделителем не
// считается. Отдельная функция, чтобы «как именно делится ссылка на плагин»
// было зафиксировано в одном месте и проверялось таблицей.
func ParseRef(ref string) (name, version string) {
	return registry.SplitRef(ref)
}

// Run — установить плагин из реестра. w — куда писать прогресс (nil = молча).
// Ошибка возвращается fail-closed: либо плагин не тронут, либо на его месте
// стоит полностью валидная копия.
func Run(w io.Writer, req Request) (Result, error) {
	out := writerOrDiscard(w)
	res := Result{Name: req.Name, Version: req.Version}
	if err := registry.ValidateComponent(req.Name); err != nil {
		return res, err
	}
	h, err := registry.Load(req.RegistrySrc)
	if err != nil {
		return res, err
	}
	defer h.Close()

	entry, ok := h.GetPlugin(req.Name)
	if !ok {
		names := h.PluginNames()
		sort.Strings(names)
		return res, fmt.Errorf("плагин %q нет в реестре (доступно: %s)", req.Name, strings.Join(names, ", "))
	}
	if err := registry.ValidateEntry(entry, true); err != nil {
		return res, err
	}
	version := req.Version
	if version == "" {
		version = entry.Version
	}
	res.Version = version
	res.Source = entry.Source
	res.Commit = entry.Commit

	srcDir, tmp, err := SourceDir(entry, h.Dir, version, req.LocalSource)
	if err != nil {
		return res, err
	}
	defer os.RemoveAll(tmp)

	if _, err := os.Stat(filepath.Join(srcDir, "plugin.yaml")); err != nil {
		return res, fmt.Errorf("в %s нет plugin.yaml (source=%s path=%s version=%s)", srcDir, entry.Source, entry.Path, version)
	}

	destDir := filepath.Join(req.Dest, req.Name)
	if err := stageAndCommit(out, srcDir, destDir, req.Name, entry, version, &res.Notes); err != nil {
		return res, err
	}
	res.Dir = destDir
	fmt.Fprintf(out, "  + %s (%s) → %s\n", req.Name, version, destDir)
	return res, nil
}

// stageAndCommit — публикация каталога плагина. Порядок прежний и
// fail-closed: копия во временный каталог рядом с целью → .wedra (lock) →
// core.ValidatePluginDir → прежняя копия в backup → rename. Ни один шаг не
// деградирует в «best effort»: непрошедший манифест не доходит до rename, а
// сорванный rename откатывается из backup, и пользователь о нём узнаёт из
// ошибки, а не из тишины.
//
// Замечания складываются в Result.Notes, а не только печатаются: HTTP-ответ
// должен показать причины неудачи, не заставляя разбирать stdout.
func stageAndCommit(out io.Writer, srcDir, destDir, name string, entry registry.Entry, version string, notes *[]string) error {
	parent := filepath.Dir(destDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(parent, "."+filepath.Base(destDir)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if err := registry.CopyDir(srcDir, staging); err != nil {
		return err
	}
	lock := registry.Lock{Name: name, Source: entry.Source, Path: entry.Path, Version: version}
	if err := registry.WriteLock(staging, lock); err != nil {
		return err
	}
	if errs := core.ValidatePluginDir(staging); len(errs) > 0 {
		for _, e := range errs {
			fmt.Fprintln(out, "  ✗ манифест:", e)
			*notes = append(*notes, "манифест: "+e)
		}
		return fmt.Errorf("плагин %s не прошёл проверку манифеста", name)
	}

	backup := ""
	if _, err := os.Stat(destDir); err == nil {
		backup, err = os.MkdirTemp(parent, "."+filepath.Base(destDir)+".old-*")
		if err != nil {
			return err
		}
		if err := os.Remove(backup); err != nil {
			return err
		}
		if err := os.Rename(destDir, backup); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(staging, destDir); err != nil {
		if backup != "" {
			if restoreErr := os.Rename(backup, destDir); restoreErr != nil {
				return fmt.Errorf("replace %s: %w; restore: %v", destDir, err, restoreErr)
			}
		}
		return err
	}
	if backup != "" {
		// Замена состоялась — отмечаем в Result.Notes. На stdout это не идёт:
		// текст CLI менять нельзя.
		*notes = append(*notes, "прежняя установка в "+destDir+" заменена")
		if err := os.RemoveAll(backup); err != nil {
			return err
		}
	}
	return nil
}

// writerOrDiscard — nil-воркер = отбросить. Явная ветка нужна, потому что
// fmt.Fprintf(nil, …) падает, а «молчаливый» вызов — законный сценарий
// (HTTP-эндпоинту прогресс не нужен).
func writerOrDiscard(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}
