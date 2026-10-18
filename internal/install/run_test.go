package install

// Точка входа Run: успех (структура + текст прогресса) и отказ
// (fail-closed: прежняя копия на месте). Оба без сети — реестр и source
// лежат во временных каталогах.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wykserdex/wedra/internal/registry"
)

func TestRunInstallsAndReportsResult(t *testing.T) {
	registryDir, sourceDir, _ := writePluginFixture(t, validManifest)
	dest := filepath.Join(t.TempDir(), "plugins")

	var out bytes.Buffer
	res, err := Run(&out, Request{Name: "safe", RegistrySrc: registryDir, Dest: dest})
	if err != nil {
		t.Fatalf("установка: %v", err)
	}

	if res.Name != "safe" || res.Version != "main" {
		t.Fatalf("Result = %+v, ждём имя safe и версию main из реестра", res)
	}
	if want := filepath.Join(dest, "safe"); res.Dir != want {
		t.Fatalf("Result.Dir = %q, ждём %q", res.Dir, want)
	}
	if res.Source != sourceDir {
		t.Fatalf("Result.Source = %q, ждём %q", res.Source, sourceDir)
	}
	if res.Commit != "" {
		t.Fatalf("Result.Commit = %q, ждём пусто: пин в реестре не задан", res.Commit)
	}
	if len(res.Notes) != 0 {
		t.Fatalf("Notes = %v, для чистой установки ждём пусто", res.Notes)
	}

	// Прогресс — в переданный writer, байт в байт прежний текст CLI.
	want := "  + safe (main) → " + res.Dir + "\n"
	if out.String() != want {
		t.Fatalf("прогресс = %q, ждём %q", out.String(), want)
	}

	// Пин для пайплайнов на месте и совпадает с тем, что ставили.
	lock, err := registry.ReadLock(res.Dir)
	if err != nil {
		t.Fatalf("lock-файла нет: %v", err)
	}
	if lock.Name != "safe" || lock.Version != "main" || lock.Path != "plugin" || lock.Source != sourceDir {
		t.Fatalf("lock = %+v", lock)
	}
	if _, err := os.Stat(filepath.Join(res.Dir, "plugin.yaml")); err != nil {
		t.Fatalf("манифест не установлен: %v", err)
	}
}

// nil-writer — законный молчаливый вызов (HTTP-эндпоинту прогресс не нужен):
// падать он не должен ровно так же, как раньше падал бы fmt.Printf в никуда.
func TestRunAcceptsNilWriter(t *testing.T) {
	registryDir, _, _ := writePluginFixture(t, validManifest)
	dest := filepath.Join(t.TempDir(), "plugins")
	if _, err := Run(nil, Request{Name: "safe", RegistrySrc: registryDir, Dest: dest}); err != nil {
		t.Fatalf("установка с nil-writer: %v", err)
	}
}

func TestRunKeepsOldVersionOnValidationFailure(t *testing.T) {
	registryDir, _, _ := writePluginFixture(t, brokenManifest)
	dest := filepath.Join(t.TempDir(), "plugins")
	oldDir := filepath.Join(dest, "safe")
	if err := os.MkdirAll(oldDir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(oldDir, "marker")
	if err := os.WriteFile(marker, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	res, err := Run(&out, Request{Name: "safe", RegistrySrc: registryDir, Dest: dest})
	if err == nil {
		t.Fatal("невалидный плагин установлен")
	}
	if !strings.Contains(err.Error(), "не прошёл проверку манифеста") {
		t.Fatalf("невнятный отказ: %v", err)
	}
	// Прежняя копия не тронута — отказ fail-closed, а не «установилось и сломалось».
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("старая установка убрана: %v", err)
	}
	if res.Dir != "" {
		t.Fatalf("Result.Dir = %q при отказе — публикации не было", res.Dir)
	}
	// Причины отказа обязаны быть и в тексте, и в Result: HTTP-ответ не должен
	// зависеть от stdout.
	if len(res.Notes) == 0 {
		t.Fatal("причины отказа не попали в Result.Notes")
	}
	if !strings.Contains(out.String(), "  ✗ манифест:") {
		t.Fatalf("причины отказа не напечатаны в writer: %q", out.String())
	}
	// Никакого мусора рядом с целью: staging убран.
	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "safe" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("в %s остался мусор: %v", dest, names)
	}
}

func TestRunReportsUnknownPlugin(t *testing.T) {
	registryDir, _, _ := writePluginFixture(t, validManifest)
	_, err := Run(nil, Request{Name: "absent", RegistrySrc: registryDir, Dest: t.TempDir()})
	if err == nil {
		t.Fatal("плагина нет в реестре, а установка прошла")
	}
	if !strings.Contains(err.Error(), `плагин "absent" нет в реестре`) {
		t.Fatalf("невнятный отказ: %v", err)
	}
}
