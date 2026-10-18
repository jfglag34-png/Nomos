package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/wykserdex/wedra/internal/core"
	"github.com/wykserdex/wedra/internal/install"
	"github.com/wykserdex/wedra/internal/pipeline"
	"github.com/wykserdex/wedra/internal/registry"
)

// ── plugin install ─────────────────────────────────────────────────────────
//
// Ядро установки живёт в internal/install: там же, где HTTP-эндпоинт берёт
// структурный результат. Здесь только разбор аргументов, текст и коды выхода —
// печать в stdout осталась ровно той же.

func RunPluginInstall(args []string) {
	req, err := install.ParseArgs(args)
	if err != nil {
		fmt.Println("нужно имя плагина: wedra plugin install <name>[@version] [--registry=<url|path>] [--dest=plugins]")
		os.Exit(2)
	}
	if err := doPluginInstall(req.Name, req.Version, req.RegistrySrc, req.Dest); err != nil {
		fmt.Println("установка не удалась:", err)
		os.Exit(1)
	}
}

// doPluginInstall — установка для `plugin install` и для автоустановки из
// `pipeline install`. Прогресс уходит в os.Stdout, как уходил раньше;
// структурный результат тем же вызовом достаётся из install.Run напрямую.
func doPluginInstall(name, ver, registrySrc, dest string) error {
	_, err := install.Run(os.Stdout, install.Request{
		Name:        name,
		Version:     ver,
		RegistrySrc: registrySrc,
		Dest:        dest,
	})
	return err
}

// ── pipeline install ───────────────────────────────────────────────────────

type pipelineInstallResult struct {
	PresetName string
	OutFile    string
	Installed  int
	Present    int
	Digest     string // sha256 установленного пресета (провенанс)
	Warnings   []string
}

// presetProvenance — откуда взяты байты пресета. Пишется sidecar'ом рядом с
// установленным файлом, чтобы «что именно лежит в examples/» можно было
// сверить позже, а не верить на слово.
type presetProvenance struct {
	Source    string // URL без userinfo/query, либо путь/источник реестра
	SourceSum string // "sha256:<hex>" исходных байт (пусто, если не считали)
}

// presetMarshal — шов для тестов. Прод — это yaml.Marshal; подменяется он
// ровно одним способом: тест кладёт сюда несимметричный парсингу маршалинг и
// убеждается, что installPipelinePreset отказывается публиковать его вывод.
// Без этого шва проверка «файл на диске читается» проверяла бы сегодняшний
// маршалинг, а не затвор: сломанный маршалинг завтра просто сделал бы install
// зелёным.
var presetMarshal = func(pf *pipeline.PipelineFile) ([]byte, error) {
	return yaml.Marshal(pf)
}

// verifyStagedPreset — читает уже записанный на диск пресет тем же
// загрузчиком, которым его потом прочитает пользователь, и прогоняет тем же
// валидатором. installedPath — имя, под которым файл окажется в examples/:
// в ошибке пользователь должен видеть свой путь, а не имя временного файла.
//
// Зачем это вообще нужно: до правки всё проверяло СТРУКТУРУ в памяти, а на диск
// уходил результат yaml.Marshal, и эти две вещи расходились. Разошлись один
// раз и уехали в релиз вместе с сообщением «пресет готов».
func verifyStagedPreset(staged, installedPath string) error {
	written, err := pipeline.LoadPipelineFile(staged)
	if err != nil {
		return fmt.Errorf("установленный пресет не читается ядром (%s): %w", installedPath, err)
	}
	errs, _ := core.Validate(written, core.NewEngine())
	if len(errs) > 0 {
		return fmt.Errorf("установленный пресет не проходит валидацию после записи (%s): %s",
			installedPath, strings.Join(errs, "; "))
	}
	return nil
}

func commitPresetFile(staged, target string) error {
	if _, err := os.Stat(target); err != nil {
		if os.IsNotExist(err) {
			return os.Rename(staged, target)
		}
		return err
	}
	backup, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".old-*")
	if err != nil {
		return err
	}
	backupName := backup.Name()
	if err := backup.Close(); err != nil {
		os.Remove(backupName)
		return err
	}
	if err := os.Remove(backupName); err != nil {
		return err
	}
	if err := os.Rename(target, backupName); err != nil {
		return err
	}
	if err := os.Rename(staged, target); err != nil {
		if restoreErr := os.Rename(backupName, target); restoreErr != nil {
			return fmt.Errorf("replace %s: %w; restore: %v", target, err, restoreErr)
		}
		return err
	}
	return os.Remove(backupName)
}

func installPipelinePreset(raw []byte, fallbackName, registrySrc string, prov presetProvenance) (pipelineInstallResult, error) {
	pf, err := pipeline.LoadPipelineFileFromBytes(raw)
	if err != nil {
		return pipelineInstallResult{}, fmt.Errorf("пресет не распарсился как пайплайн: %w", err)
	}
	for i := range pf.Pipeline.Steps {
		if name, version, ok := registry.NormalizePluginRef(pf.Pipeline.Steps[i].Plugin); ok {
			if version != "" {
				pf.Pipeline.Steps[i].Plugin = name + "@" + version
			} else {
				pf.Pipeline.Steps[i].Plugin = name
			}
		}
	}
	normalized, err := presetMarshal(pf)
	if err != nil {
		return pipelineInstallResult{}, fmt.Errorf("не удалось нормализовать пресет: %w", err)
	}
	pname := pf.Pipeline.Name
	if pname == "" {
		pname = fallbackName
	}
	if err := registry.ValidateComponent(pname); err != nil {
		return pipelineInstallResult{}, fmt.Errorf("небезопасное имя пресета: %w", err)
	}
	if err := os.MkdirAll("examples", 0o755); err != nil {
		return pipelineInstallResult{}, err
	}
	outFile := filepath.Join("examples", pname+".yaml")
	staged, err := os.CreateTemp("examples", "."+pname+".yaml.tmp-*")
	if err != nil {
		return pipelineInstallResult{}, err
	}
	stagedName := staged.Name()
	committed := false
	defer func() {
		if !committed {
			os.Remove(stagedName)
		}
	}()
	if _, err := staged.Write(normalized); err != nil {
		staged.Close()
		return pipelineInstallResult{}, err
	}
	if err := staged.Close(); err != nil {
		return pipelineInstallResult{}, err
	}
	if err := os.Chmod(stagedName, 0o644); err != nil {
		return pipelineInstallResult{}, err
	}

	wantVer := map[string]string{}
	for _, st := range pf.Pipeline.Steps {
		if registry.IsLocalRef(st.Plugin) {
			continue
		}
		nm, vr := registry.SplitRef(st.Plugin)
		if prev, ok := wantVer[nm]; ok && prev != "" && vr != "" && prev != vr {
			return pipelineInstallResult{}, fmt.Errorf("плагин %s требует разные версии: %s и %s", nm, prev, vr)
		}
		if prev, ok := wantVer[nm]; !ok || prev == "" {
			wantVer[nm] = vr
		}
	}
	result := pipelineInstallResult{PresetName: pname, OutFile: outFile}
	for nm, vr := range wantVer {
		installedDir, err := registry.RefToDir(nm, "plugins")
		need := err != nil
		if !need && vr != "" {
			if iv, ok := registry.InstalledVersion(installedDir); !ok || iv != vr {
				need = true
			}
		}
		if need {
			if err := doPluginInstall(nm, vr, registrySrc, "plugins"); err != nil {
				return result, fmt.Errorf("автоустановка %s: %w", nm, err)
			}
			result.Installed++
		} else {
			result.Present++
		}
	}

	errs, warns := core.Validate(pf, core.NewEngine())
	result.Warnings = warns
	if len(errs) > 0 {
		return result, fmt.Errorf("валидация пресета: %s", strings.Join(errs, "; "))
	}
	// Fail-closed на ЗАПИСАННЫЕ байты, а не на структуру в памяти.
	//
	// Проверка стоит ДО commit: негодный файл не должен ни появиться в
	// examples/, ни вытеснить оттуда прежний рабочий. Поэтому вопроса «оставить
	// плохой файл на разбор» не возникает — на диск он не попадает, временный
	// файл убирает defer ниже. Отказ виден пользователю как ошибка установки,
	// а не как «пресет готов», после которого следующая же команда падает.
	if err := verifyStagedPreset(stagedName, outFile); err != nil {
		return result, err
	}
	// Провенанс: sidecar есть, а файл после установки разошёлся — предупреждаем
	// (не блокируем: examples/ правят руками), но факт фиксируем.
	if _, err := os.Stat(outFile + presetProvenanceExt); err == nil {
		if err := verifyPresetProvenance(outFile); err != nil {
			result.Warnings = append(result.Warnings, "провенанс: "+err.Error())
		}
	}
	if err := commitPresetFile(stagedName, outFile); err != nil {
		return result, err
	}
	committed = true
	digest, err := writePresetProvenance(outFile, prov)
	if err != nil {
		return result, err
	}
	result.Digest = digest
	return result, nil
}

func RunPipelineInstall(args []string) {
	preset, registrySrc := "", ""
	for _, a := range args {
		if strings.HasPrefix(a, "--registry=") {
			registrySrc = a[len("--registry="):]
		} else if !strings.HasPrefix(a, "-") {
			preset = a
		}
	}
	if preset == "" {
		fmt.Println("нужен пресет: wedra pipeline install <name|file.yaml|url> [--registry=<url|path>]")
		os.Exit(2)
	}

	raw, name, prov, err := fetchPreset(preset, registrySrc, "")
	if err != nil {
		fmt.Println("ошибка:", err)
		os.Exit(1)
	}
	result, err := installPipelinePreset(raw, name, registrySrc, prov)
	if err != nil {
		fmt.Println("ошибка:", err)
		os.Exit(1)
	}
	fmt.Printf("▶ пресет %q → %s\n", result.PresetName, result.OutFile)
	fmt.Printf("  плагины: %d установлено, %d уже на месте\n", result.Installed, result.Present)
	fmt.Printf("  провенанс: %s %s\n", result.Digest, result.OutFile+presetProvenanceExt)
	for _, warning := range result.Warnings {
		fmt.Println("  · предупреждение:", warning)
	}
	fmt.Printf("■ пресет %q готов: %s --yes\n", result.PresetName, result.OutFile)
}

// fetchPreset — имя из реестра, локальный .yaml или https-URL.
func fetchPreset(preset, registrySrc, localSource string) ([]byte, string, presetProvenance, error) {
	// 1) локальный файл
	if strings.HasSuffix(preset, ".yaml") || strings.HasSuffix(preset, ".yml") {
		if _, e := os.Stat(preset); e == nil {
			raw, e2 := os.ReadFile(preset)
			if e2 != nil {
				return nil, "", presetProvenance{}, e2
			}
			name := strings.TrimSuffix(filepath.Base(preset), filepath.Ext(preset))
			return raw, name, presetProvenance{Source: filepath.Clean(preset), SourceSum: sha256Hex(raw)}, nil
		}
	}
	// 2) URL — только https, без cleartext и без уходов на чужой host
	if isPresetURLRef(preset) {
		return downloadPresetHTTPS(preset)
	}
	// 3) реестр
	h, e2 := registry.Load(registrySrc)
	if e2 != nil {
		return nil, "", presetProvenance{}, e2
	}
	defer h.Close()
	entry, ok := h.GetPreset(preset)
	if !ok {
		names := h.PresetNames()
		sort.Strings(names)
		return nil, "", presetProvenance{}, fmt.Errorf("пресет %q нет в реестре (доступно: %s)", preset, strings.Join(names, ", "))
	}
	if err := registry.ValidateEntry(entry, true); err != nil {
		return nil, "", presetProvenance{}, err
	}
	// для пресета src — путь к самому файлу
	src, tmp, e2 := install.SourceDir(entry, h.Dir, entry.Version, localSource)
	if e2 != nil {
		return nil, "", presetProvenance{}, e2
	}
	defer os.RemoveAll(tmp)
	raw, e2 := os.ReadFile(src)
	if e2 != nil {
		return nil, "", presetProvenance{}, fmt.Errorf("пресет %s: %w", preset, e2)
	}
	return raw, preset, presetProvenance{Source: redactProvenanceSource(entry.Source), SourceSum: sha256Hex(raw)}, nil
}

// ── загрузка пресета по URL ────────────────────────────────────────────────
//
// Прямой URL — это недоверенный вход наравне с реестром: цепочку «имя пресета →
// байты → исполняемый конвейер» нельзя ронять до явной проверки источника.
//   - только https: cleartext http:// отклоняется (пресет = исполняемый код);
//   - редирект проверяется на каждом ходу: та же схема и тот же host, иначе отказ;
//   - опциональный пин «#sha256=<hex>» сверяется с полученными байтами;
//   - побочка: рядом с установленным пресетом пишется .sha256 sidecar.

const (
	presetFetchTimeout  = 15 * time.Second
	presetMaxRedirects  = 5
	presetMaxBytes      = 1 << 20
	presetProvenanceExt = ".sha256"
)

// presetHTTPClient — фабрика клиента загрузки (тесты подменяют транспорт).
var presetHTTPClient = newPresetClient

// newPresetClient — https-only клиент: редирект допускается только внутри того
// же источника (та же схема + тот же host). Смена схемы/host, cleartext в
// редиректе и длинная цепочка — отказ, а не тихая смена того, что ставим.
func newPresetClient() *http.Client {
	c := &http.Client{Timeout: presetFetchTimeout}
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= presetMaxRedirects {
			return fmt.Errorf("больше %d редиректов", presetMaxRedirects)
		}
		if err := checkPresetURL(req.URL); err != nil {
			return fmt.Errorf("редирект отклонён: %w", err)
		}
		origin := via[0].URL
		if !strings.EqualFold(req.URL.Scheme, origin.Scheme) || !strings.EqualFold(req.URL.Host, origin.Host) {
			return fmt.Errorf("редирект на другой источник: %s → %s", origin.Redacted(), req.URL.Redacted())
		}
		return nil
	}
	return c
}

// isPresetURLRef — ref похож на URL (любая схема). Ветка загрузки сама решает,
// что схема не https: отказ с внятной ошибкой вместо «пресет не найден».
func isPresetURLRef(ref string) bool {
	return strings.Contains(strings.ToLower(strings.TrimSpace(ref)), "://")
}

// checkPresetURL — только https и только с host. Redacted(): креды в лог не идут.
func checkPresetURL(u *url.URL) error {
	if u == nil {
		return fmt.Errorf("пустой URL пресета")
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "http":
		return fmt.Errorf("cleartext http:// запрещён (%s) — только https://", u.Redacted())
	case "":
		return fmt.Errorf("URL пресета без схемы: %s", u.Redacted())
	default:
		return fmt.Errorf("неподдерживаемая схема %q — только https://", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("URL пресета без host: %s", u.Redacted())
	}
	return nil
}

// parsePresetRef — разбирает ref: снимает фрагмент-пин (#sha256=…),
// требует https. Возвращает чистый URL, ожидаемый digest ("" — пин не задан).
func parsePresetRef(ref string) (*url.URL, string, error) {
	u, err := url.Parse(strings.TrimSpace(ref))
	if err != nil {
		return nil, "", fmt.Errorf("некорректный URL пресета %q: %w", ref, err)
	}
	want, err := presetDigestPin(u.Fragment)
	if err != nil {
		return nil, "", fmt.Errorf("URL пресета %q: %w", ref, err)
	}
	u.Fragment = ""
	u.RawFragment = ""
	if err := checkPresetURL(u); err != nil {
		return nil, "", err
	}
	return u, want, nil
}

// presetDigestPin — «sha256=<64 hex>» из фрагмента URL. Любой другой фрагмент —
// ошибка: молча выкинуть нельзя, иначе пин «не сработал» и это не видно.
func presetDigestPin(fragment string) (string, error) {
	f := strings.TrimSpace(fragment)
	if f == "" {
		return "", nil
	}
	v, ok := strings.CutPrefix(strings.ToLower(f), "sha256=")
	if !ok {
		return "", fmt.Errorf("нераспознанный фрагмент %q (ожидается sha256=<hex>)", f)
	}
	v = strings.TrimSpace(v)
	if len(v) != 64 {
		return "", fmt.Errorf("пин sha256 должен быть 64 hex-символа, got %d", len(v))
	}
	if _, err := hex.DecodeString(v); err != nil {
		return "", fmt.Errorf("пин sha256 не hex: %q", v)
	}
	return "sha256:" + v, nil
}

// downloadPresetHTTPS — загрузка пресета: https, редиректы внутри host,
// тело ограничено, пин (если задан) сверяется с полученными байтами.
func downloadPresetHTTPS(ref string) ([]byte, string, presetProvenance, error) {
	var none presetProvenance
	u, want, err := parsePresetRef(ref)
	if err != nil {
		return nil, "", none, err
	}
	name := strings.TrimSuffix(filepath.Base(u.Path), filepath.Ext(u.Path))
	if name == "" || name == "." || name == string(filepath.Separator) {
		return nil, "", none, fmt.Errorf("URL пресета без имени файла: %s", u.Redacted())
	}
	target := u.String()
	resp, err := presetHTTPClient().Get(target)
	if err != nil {
		return nil, "", none, fmt.Errorf("загрузка %s: %w", target, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, "", none, fmt.Errorf("%s: HTTP %d", target, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, presetMaxBytes))
	if err != nil {
		return nil, "", none, err
	}
	got := sha256Hex(raw)
	if want != "" && !strings.EqualFold(want, got) {
		return nil, "", none, fmt.Errorf("пресет %s: sha256 не совпал (ожидали %s, получили %s)", target, want, got)
	}
	// Провенанс — фактический адрес, откуда пришли байты (тот же host: редиректы
	// проверены), без userinfo/query.
	final := u
	if resp.Request != nil && resp.Request.URL != nil {
		final = resp.Request.URL
	}
	return raw, name, presetProvenance{Source: redactPresetURL(final), SourceSum: got}, nil
}

// redactPresetURL — источник для sidecar: без userinfo и query (там токены),
// только схема+host+путь — этого хватает, чтобы понять, откуда файл.
func redactPresetURL(u *url.URL) string {
	clean := *u
	clean.User = nil
	clean.RawQuery = ""
	clean.Fragment = ""
	clean.RawFragment = ""
	return clean.String()
}

// redactProvenanceSource — то же для источника из реестра: URL-вид без
// userinfo/query, локальный путь как есть.
func redactProvenanceSource(source string) string {
	trimmed := strings.TrimSpace(source)
	if !strings.Contains(trimmed, "://") {
		return trimmed
	}
	u, err := url.Parse(trimmed)
	if err != nil || u.Host == "" {
		return trimmed
	}
	return redactPresetURL(u)
}

func sha256Hex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ── провенанс пресета (sidecar) ────────────────────────────────────────────

// writePresetProvenance — пишет <preset>.yaml.sha256 рядом с установленным
// пресетом и сразу сверяет записанное с файлом на диске. Формат первой строки
// совпадает с sha256sum, поэтому sidecar проверяется и им:
//
//	cd examples && sha256sum -c foo.yaml.sha256
//
// Возвращает digest установленного файла.
func writePresetProvenance(outFile string, prov presetProvenance) (string, error) {
	raw, err := os.ReadFile(outFile)
	if err != nil {
		return "", fmt.Errorf("провенанс %s: %w", outFile, err)
	}
	digest := sha256Hex(raw)
	body := strings.TrimPrefix(digest, "sha256:") + "  " + filepath.Base(outFile) + "\n"
	if prov.Source != "" {
		body += "# source: " + prov.Source + "\n"
	}
	if prov.SourceSum != "" {
		body += "# source_sha256: " + prov.SourceSum + "\n"
	}
	sidecar := outFile + presetProvenanceExt
	if err := writeFileAtomic(sidecar, []byte(body), 0o644); err != nil {
		return "", fmt.Errorf("провенанс %s: %w", sidecar, err)
	}
	if err := verifyPresetProvenance(outFile); err != nil {
		return "", err
	}
	return digest, nil
}

// verifyPresetProvenance — sidecar против файла на диске. Нет sidecar —
// os.ErrNotExist (вызывающий сам решает, это ошибка или повод промолчать).
func verifyPresetProvenance(outFile string) error {
	raw, err := os.ReadFile(outFile + presetProvenanceExt)
	if err != nil {
		return err
	}
	sum, name := "", ""
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		sum = fields[0]
		if len(fields) > 1 {
			name = fields[1]
		}
		break
	}
	if len(sum) != 64 {
		return fmt.Errorf("провенанс %s%s: нечитаемая сумма %q", outFile, presetProvenanceExt, sum)
	}
	if _, err := hex.DecodeString(sum); err != nil {
		return fmt.Errorf("провенанс %s%s: сумма не hex: %q", outFile, presetProvenanceExt, sum)
	}
	if name != "" && name != filepath.Base(outFile) {
		return fmt.Errorf("провенанс %s%s: записан для %q, а лежит %q", outFile, presetProvenanceExt, name, filepath.Base(outFile))
	}
	content, err := os.ReadFile(outFile)
	if err != nil {
		return err
	}
	if got := strings.TrimPrefix(sha256Hex(content), "sha256:"); !strings.EqualFold(got, sum) {
		return fmt.Errorf("пресет %s изменён после установки: %s ≠ %s", outFile, got, sum)
	}
	return nil
}

// writeFileAtomic — запись через temp + rename в том же каталоге.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	staged, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := staged.Name()
	if _, err := staged.Write(data); err != nil {
		staged.Close()
		os.Remove(name)
		return err
	}
	if err := staged.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, mode); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}
