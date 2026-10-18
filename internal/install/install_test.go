package install

// Тесты чистых функций пакета: разбор аргументов, разбор ссылки на плагин,
// приведение адреса репозитория к общему виду и выбор источника. Ни сети, ни
// git: выбор источника вынесен в planSource, поэтому клон виден в плане, но
// не исполняется.
//
// Отдельно проверяется инвариант пакета (в nofmtprint_test.go): в
// internal/install нет ни одного fmt.Print*, потому что весь прогресс уходит в
// переданный io.Writer.

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/wykserdex/wedra/internal/registry"
)

func TestParseArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    Request
		wantErr error
	}{
		{
			name: "только имя — дефолты на месте",
			args: []string{"safe"},
			want: Request{Name: "safe", Dest: "plugins"},
		},
		{
			name: "версия из ссылки",
			args: []string{"safe@v1"},
			want: Request{Name: "safe", Version: "v1", Dest: "plugins"},
		},
		{
			name: "флаги до и после позиционного аргумента",
			args: []string{"--registry=./registry.yaml", "safe@v2", "--dest=/tmp/p"},
			want: Request{Name: "safe", Version: "v2", RegistrySrc: "./registry.yaml", Dest: "/tmp/p"},
		},
		{
			name: "последний позиционный побеждает",
			args: []string{"first", "second"},
			want: Request{Name: "second", Dest: "plugins"},
		},
		{
			name: "неизвестный флаг пропускается молча",
			args: []string{"--local-source=.", "--что-то=ещё", "safe"},
			want: Request{Name: "safe", Dest: "plugins"},
		},
		{
			name: "пустой --dest остаётся пустым",
			args: []string{"safe", "--dest="},
			want: Request{Name: "safe", Dest: ""},
		},
		{
			name:    "имя плагина не указано",
			args:    []string{},
			want:    Request{Dest: "plugins"},
			wantErr: ErrNoRef,
		},
		{
			name:    "только флаги — имени нет",
			args:    []string{"--registry=./registry.yaml"},
			want:    Request{RegistrySrc: "./registry.yaml", Dest: "plugins"},
			wantErr: ErrNoRef,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseArgs(tt.args)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ошибка = %v, ждём %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("Request = %+v, ждём %+v", got, tt.want)
			}
		})
	}
}

func TestParseRef(t *testing.T) {
	tests := []struct {
		ref     string
		name    string
		version string
	}{
		{ref: "safe", name: "safe"},
		{ref: "safe@v1", name: "safe", version: "v1"},
		{ref: "safe@1.2.3", name: "safe", version: "1.2.3"},
		// Делит ПОСЛЕДНИЙ «@»: версия с «@» внутри не должна ломать разбор.
		{ref: "safe@1.0@rc1", name: "safe@1.0", version: "rc1"},
		// Хвостовой «@» — пустая версия, а не отдельное имя.
		{ref: "safe@", name: "safe"},
		// «@» в позиции 0 разделителем не считается.
		{ref: "@v1", name: "@v1"},
		{ref: "plugins/community/foo", name: "plugins/community/foo"},
	}
	for _, tt := range tests {
		t.Run(tt.ref, func(t *testing.T) {
			name, version := ParseRef(tt.ref)
			if name != tt.name || version != tt.version {
				t.Fatalf("ParseRef(%q) = (%q, %q), ждём (%q, %q)", tt.ref, name, version, tt.name, tt.version)
			}
		})
	}
}

func TestNormalizeRepo(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: "https://github.com/Wykserdex/wedra.git", want: "github.com/wykserdex/wedra"},
		{in: "http://github.com/Wykserdex/wedra", want: "github.com/wykserdex/wedra"},
		{in: "git@github.com:wykserdex/wedra.git", want: "github.com/wykserdex/wedra"},
		{in: "https://github.com/wykserdex/wedra/", want: "github.com/wykserdex/wedra"},
		{in: "  https://github.com/wykserdex/wedra\n", want: "github.com/wykserdex/wedra"},
		// userinfo не нормализуется: двоеточие в кредях съедается раньше, чем
		// в ssh-форме. Такая строка просто не совпадёт с origin — установка
		// уйдёт в клон, а не объявит локальный checkout «тем же репозиторием».
		{in: "https://user:token@github.com/wykserdex/wedra", want: "user/token@github.com/wykserdex/wedra"},
		{in: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := normalizeRepo(tt.in); got != tt.want {
				t.Fatalf("normalizeRepo(%q) = %q, ждём %q", tt.in, got, tt.want)
			}
		})
	}
}

// writePluginFixture — оффлайн-реестр и каталог плагина рядом с ним. Ни сети,
// ни git: source указывает на локальный каталог, поэтому выбор источника
// доходит до второго шага и дальше не идёт.
func writePluginFixture(t *testing.T, manifest string) (registryDir, sourceDir, pluginDir string) {
	t.Helper()
	sourceDir = t.TempDir()
	pluginDir = filepath.Join(sourceDir, "plugin")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "main.py"), []byte("pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	registryDir = t.TempDir()
	body := "version: \"0.1\"\nplugins:\n  safe:\n    source: " + strconv.Quote(sourceDir) +
		"\n    path: plugin\n    version: main\n"
	if err := os.WriteFile(filepath.Join(registryDir, "registry.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return registryDir, sourceDir, pluginDir
}

const validManifest = `id: safe
version: 0.1.0
platform_api: "0.1"
runtime:
  type: python
  entry: main.py
input:
  text:
    from: input.text
    type: string
output:
  value:
    type: string
permissions:
  network: []
  filesystem: none
  secrets: []
`

const brokenManifest = "id: safe\nversion: 0.1.0\nplatform_api: \"0.1\"\nruntime:\n  type: python\n  entry: main.py\ninput: {}\noutput: {}\n"

func TestPlanSource(t *testing.T) {
	registryDir, sourceDir, pluginDir := writePluginFixture(t, validManifest)
	// Реестровый шорткат: запись рядом с registry.yaml, но source никуда не
	// ведёт и клонить его нельзя.
	shortcutDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(shortcutDir, "plugins"), 0o755); err != nil {
		t.Fatal(err)
	}
	presetFile := filepath.Join(shortcutDir, "examples")
	if err := os.MkdirAll(presetFile, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(presetFile, "rt.yaml"), []byte("format_version: \"0.2\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Шорткат ищет ещё и <реестр>/plugins/<имя> — «официальная» раскладка.
	if err := os.MkdirAll(filepath.Join(shortcutDir, "plugins", "safe"), 0o755); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "нет-такого")

	no := func(string, string) bool { return false }
	yes := func(string, string) bool { return true }

	tests := []struct {
		name      string
		entry     registry.Entry
		regDir    string
		local     string
		sameRepo  func(string, string) bool
		wantKind  sourceKind
		wantDir   string
		wantErr   bool
		wantVerif bool
	}{
		{
			name:     "source — локальный каталог",
			entry:    registry.Entry{Source: sourceDir, Path: "plugin", Version: "main"},
			regDir:   registryDir,
			sameRepo: no,
			wantKind: sourceLocalDir,
			wantDir:  pluginDir,
		},
		{
			name:      "локальный каталог с пином требует сверки",
			entry:     registry.Entry{Source: sourceDir, Path: "plugin", Version: "main", Commit: "0" + strings.Repeat("a", 39)},
			regDir:    registryDir,
			sameRepo:  no,
			wantKind:  sourceLocalDir,
			wantDir:   pluginDir,
			wantVerif: true,
		},
		{
			name:     "локальный чеккаут берёт своё",
			entry:    registry.Entry{Source: "https://example.invalid/x.git", Path: "plugin", Version: "main"},
			regDir:   registryDir,
			local:    sourceDir,
			sameRepo: yes,
			wantKind: sourceLocalOverride,
			wantDir:  pluginDir,
		},
		{
			name:     "локальный чеккаут без нужного пути — отказ, а не клон",
			entry:    registry.Entry{Source: sourceDir, Path: "нет-такого", Version: "main"},
			regDir:   registryDir,
			local:    sourceDir,
			sameRepo: yes,
			wantErr:  true,
		},
		{
			name:     "локальный source, но репозиторий другой",
			entry:    registry.Entry{Source: "https://example.invalid/x.git", Path: "plugin", Version: "main"},
			regDir:   registryDir,
			local:    sourceDir,
			sameRepo: no,
			wantKind: sourceClone,
		},
		{
			name:     "шорткат: запись рядом с реестром",
			entry:    registry.Entry{Source: missing, Path: "examples/rt.yaml", Version: "main"},
			regDir:   shortcutDir,
			sameRepo: no,
			wantKind: sourceRegistryDir,
			wantDir:  filepath.Join(presetFile, "rt.yaml"),
		},
		{
			name:     "шорткат: плагин в plugins/ рядом с реестром",
			entry:    registry.Entry{Source: missing, Path: "official/safe", Version: "main"},
			regDir:   shortcutDir,
			sameRepo: no,
			wantKind: sourceRegistryDir,
			wantDir:  filepath.Join(shortcutDir, "plugins", "safe"),
		},
		{
			name:     "пин выключает шорткат — только клон",
			entry:    registry.Entry{Source: missing, Path: "examples/rt.yaml", Version: "main", Commit: "0" + strings.Repeat("a", 39)},
			regDir:   shortcutDir,
			sameRepo: no,
			wantKind: sourceClone,
		},
		{
			name:     "реестра рядом нет — клон",
			entry:    registry.Entry{Source: "https://example.invalid/x.git", Path: "plugin", Version: "main"},
			regDir:   "",
			sameRepo: no,
			wantKind: sourceClone,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := planSource(tt.entry, tt.regDir, tt.entry.Version, tt.local, tt.sameRepo)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ожидался отказ, план: %+v", plan)
				}
				if !strings.Contains(err.Error(), "--local-source=") {
					t.Fatalf("невнятный отказ: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if plan.kind != tt.wantKind {
				t.Fatalf("источник = %s (%d), ждём %s (%d)", plan.kind, int(plan.kind), tt.wantKind, int(tt.wantKind))
			}
			if plan.dir != tt.wantDir {
				t.Fatalf("каталог = %q, ждём %q", plan.dir, tt.wantDir)
			}
			if plan.verify != tt.wantVerif {
				t.Fatalf("verify = %v, ждём %v", plan.verify, tt.wantVerif)
			}
		})
	}
}

// Выбор источника должен оставаться в том же порядке при nil- функции
// sameRepo (вызывающий её не подставил): override не срабатывает молча.
func TestPlanSourceWithoutSameRepo(t *testing.T) {
	_, sourceDir, _ := writePluginFixture(t, validManifest)
	plan, err := planSource(registry.Entry{Source: sourceDir, Path: "plugin", Version: "main"}, "", "main", sourceDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.kind != sourceLocalDir {
		t.Fatalf("источник = %s, ждём локальный каталог", plan.kind)
	}
}

func TestRunRejectsUnsafeNameBeforeTouchingRegistry(t *testing.T) {
	// Реестра нет и он не понадобится: имя отвергается раньше загрузки.
	if _, err := Run(nil, Request{Name: "../../escape", Dest: t.TempDir()}); err == nil {
		t.Fatal("небезопасное имя принято")
	}
}
