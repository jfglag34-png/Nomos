package install

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/wykserdex/wedra/internal/common"
	"github.com/wykserdex/wedra/internal/registry"
)

// sourceKind — откуда взяты байты плагина. Нужен для тестов: выбор источника
// проверяется БЕЗ сети и БЕЗ git (см. planSource), потому что ветка клона
// иначе проверялась бы только сетью.
type sourceKind int

const (
	sourceLocalOverride sourceKind = iota // source совпадает с локальным чеккаутом
	sourceLocalDir                        // source — локальный каталог (оффлайн-реестр)
	sourceRegistryDir                     // реестр локальный, и path нашёлся рядом с ним
	sourceClone                           // git clone source@version (сеть)
)

func (k sourceKind) String() string {
	switch k {
	case sourceLocalOverride:
		return "локальный source"
	case sourceLocalDir:
		return "локальный каталог"
	case sourceRegistryDir:
		return "каталог рядом с реестром"
	default:
		return "git clone"
	}
}

// sourcePlan — решение «откуда брать», без исполнения. verify означает «перед
// использованием проверить локальный checkout пином из реестра» (pin есть —
// значит есть и проверка; проверять нечего, если пина нет).
type sourcePlan struct {
	dir    string
	kind   sourceKind
	verify bool
}

// SourceDir — где лежат файлы записи реестра: плагина (каталог) или пресета
// (файл). Тот же порядок выбора, что был в CLI:
//
//  1. source совпадает с этим локальным чеккаутом (--local-source);
//  2. source — локальный каталог (оффлайн-реестр);
//  3. реестр загружен из локального каталога и path существует рядом (шорткат);
//  4. git clone source@version (сеть).
//
// Второй возвращаемый — временный каталог клона, если он был; вызывающий
// обязан его убрать (os.RemoveAll). Пустая строка означает «ничего убирать».
//
// Экспортировано потому, что тем же правилом достаётся пресет: он тоже лежит
// по адресу source+path, и отдельная копия этого выбора разъехалась бы.
func SourceDir(entry registry.Entry, localRegistryDir, version, localSource string) (srcDir, tmp string, err error) {
	plan, err := planSource(entry, localRegistryDir, version, localSource, sameRepo)
	if err != nil {
		return "", "", err
	}
	switch plan.kind {
	case sourceLocalOverride:
		// v0.21: путь обязан резолвиться локально — trust-гейт не молчит и не
		// уходит в клон, если файла в локальном source нет.
		if plan.verify {
			if err := registry.VerifyCheckoutPath(localSource, entry.Commit, entry.Path); err != nil {
				return "", "", fmt.Errorf("локальный source не соответствует pin: %w", err)
			}
		}
		return plan.dir, "", nil
	case sourceLocalDir:
		if plan.verify {
			if err := registry.VerifyCheckoutPath(entry.Source, entry.Commit, entry.Path); err != nil {
				return "", "", fmt.Errorf("локальный source не соответствует pin: %w", err)
			}
		}
		return plan.dir, "", nil
	case sourceRegistryDir:
		return plan.dir, "", nil
	default:
		tmp, err := os.MkdirTemp("", "wedra-plugin-*")
		if err != nil {
			return "", "", err
		}
		if err := registry.CloneToPinned(entry.Source, version, entry.Commit, tmp); err != nil {
			os.RemoveAll(tmp)
			return "", "", err
		}
		return filepath.Join(tmp, entry.Path), tmp, nil
	}
}

// planSource — выбор источника без сети и без git. Всё, что можно решить по
// файловой системе и по переданному sameRepo, решается здесь; исполняет
// решение SourceDir.
//
// Вынесено отдельно не ради красоты: пока выбор источника был внутри функции,
// читающей сеть, единственный способ его проверить был бы «устроить клон».
// Здесь он проверяется таблицей во временных каталогах, а ветку клона
// достаточно увидеть в плане — выполнять её в тестах незачем.
func planSource(entry registry.Entry, localRegistryDir, version, localSource string, sameRepoFn func(sourceURL, dir string) bool) (sourcePlan, error) {
	if sameRepoFn == nil {
		sameRepoFn = func(string, string) bool { return false }
	}
	// v0.17: явное override — source совпадает с этим локальным чеккаутом.
	if localSource != "" && sameRepoFn(entry.Source, localSource) {
		p := filepath.Join(localSource, entry.Path)
		if _, e := os.Stat(p); e != nil {
			return sourcePlan{}, fmt.Errorf("запись %s: путь %s не найден в локальном source (--local-source=%s)", entry.Path, entry.Path, localSource)
		}
		return sourcePlan{dir: p, kind: sourceLocalOverride, verify: entry.Commit != ""}, nil
	}
	if fi, e := os.Stat(entry.Source); e == nil && fi.IsDir() {
		return sourcePlan{dir: filepath.Join(entry.Source, entry.Path), kind: sourceLocalDir, verify: entry.Commit != ""}, nil
	}
	if localRegistryDir != "" && entry.Commit == "" {
		// плагин — каталог, пресет — файл
		cand := filepath.Join(localRegistryDir, entry.Path)
		if _, e := os.Stat(cand); e != nil {
			cand = filepath.Join(localRegistryDir, "plugins", filepath.Base(filepath.FromSlash(entry.Path)))
		}
		if fi, e := os.Stat(cand); e == nil && (fi.IsDir() || fi.Mode().IsRegular()) {
			return sourcePlan{dir: cand, kind: sourceRegistryDir}, nil
		}
	}
	return sourcePlan{kind: sourceClone}, nil
}

// sameRepo — совпадает ли git origin каталога с entry.Source (для
// --local-source). Копия одноимённой функции из internal/cli/registry.go:
// свести их к одной общей можно было бы только правкой того файла, а этот
// пакет обязан быть вызываемым без cli.
func sameRepo(sourceURL, dir string) bool {
	gitCmd := exec.Command("git", "-C", dir, "remote", "get-url", "origin")
	out, err := common.Output(gitCmd)
	if err != nil {
		return false
	}
	return normalizeRepo(string(out)) == normalizeRepo(sourceURL)
}

// normalizeRepo — приводит вид записи репозитория к общему: регистр, хвост
// .git, схема, префикс git@ и двоеточие ssh. Чистая функция, поэтому
// сравнение источников проверяется без git вообще.
func normalizeRepo(u string) string {
	u = strings.ToLower(strings.TrimSpace(u))
	u = strings.TrimSuffix(u, ".git")
	u = strings.TrimPrefix(u, "https://")
	u = strings.TrimPrefix(u, "http://")
	u = strings.TrimPrefix(u, "git@")
	u = strings.Replace(u, ":", "/", 1)
	return strings.TrimSuffix(u, "/")
}
