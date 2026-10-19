// Command wedragui — WEDRA desktop (v0.8): двойной клик = окно с GUI.
//
// Внутри: тот же встроенный сервер, что у `wedra gui` (go:embed, v0.7) —
// консоль (раны, live-журнал, DAG) + редактор (/editor/). На Windows GUI
// живёт в собственном окне (WebView2, чистый Go, без CGO); если рантайм
// WebView2 не найден — честно падаем в браузер (URL в консоли и в логе).
//
// Каталоги — рядом с exe (CWD при двойном клике = папка exe):
//
//	plugins/    — плагины (wedra plugin install / tool)
//	examples/   — YAML-пайплайны (консоль их видит в списке)
//	var/runs/   — журналы ранов
//
// Лог: %APPDATA%/WEDRA/wedragui.log (Windows) / $XDG_CONFIG_HOME/WEDRA/.
// Выход: закрыть окно (на фолбэк-пути — закрыть консоль / Ctrl+C).
package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/wykserdex/wedra/internal/api"
	"github.com/wykserdex/wedra/internal/buildinfo"
	"github.com/wykserdex/wedra/internal/guidirs"
	"github.com/wykserdex/wedra/internal/plugin"
)

func resolveDesktopTrustConfig(override string) string {
	if override != "" {
		return override
	}
	if env := strings.TrimSpace(os.Getenv("WEDRA_TRUST_CONFIG")); env != "" {
		return env
	}
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), plugin.TrustConfigFile)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

func main() {
	dirs, args, err := guidirs.Parse(os.Args[1:], guidirs.Default())
	if err != nil {
		fmt.Println("каталоги:", err)
		os.Exit(2)
	}
	var port int
	debug := false
	trustCfg := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case strings.HasPrefix(a, "--port="):
			fmt.Sscanf(a[7:], "%d", &port)
		case a == "--port" && i+1 < len(args):
			fmt.Sscanf(args[i+1], "%d", &port)
			i++
		case strings.HasPrefix(a, "--trust-config="):
			trustCfg = strings.TrimPrefix(a, "--trust-config=")
		case a == "--trust-config" && i+1 < len(args):
			trustCfg = args[i+1]
			i++
		case a == "--debug":
			debug = true
		}
	}

	if err := dirs.Ensure(); err != nil {
		fmt.Println("каталоги:", err)
		os.Exit(1)
	}

	trusted, err := plugin.EffectiveAllowList(resolveDesktopTrustConfig(trustCfg))
	if err != nil {
		fmt.Println("ошибка конфига доверия:", err)
		os.Exit(2)
	}

	srv := api.NewServer(dirs.Plugins, dirs.Pipelines, dirs.Runs)
	srv.Trusted = trusted
	srv.TrustConfigPath = resolveDesktopTrustConfig(trustCfg)
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		fmt.Println("порт недоступен:", err)
		os.Exit(1)
	}
	// H4: адрес объявляем серверу ДО EnableSession — от него зависят имя cookie
	// (в нём порт) и allow-list Host. Десктоп всегда слушает только loopback.
	_, lnPort, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		fmt.Println("не удалось определить порт:", err)
		os.Exit(1)
	}
	if err := srv.ConfigureListen(api.ListenOptions{Addr: "127.0.0.1:" + lnPort, ResolvedPort: lnPort}); err != nil {
		fmt.Println("политика прослушивания:", err)
		os.Exit(1)
	}
	// v0.9 → H4: вход по одноразовому коду. Постоянного ключа в ссылке и в
	// логе нет: окно получает ссылку с кодом, сервер обменивает его на cookie.
	// Лог-файл получает только адрес без кода.
	code, err := api.NewPairingCode()
	if err != nil {
		fmt.Println("не удалось сгенерировать код входа:", err)
		os.Exit(1)
	}
	srv.EnableSession(code)
	url := "http://" + ln.Addr().String()

	logPath, logFile := openLog()
	defer logFile.Close()

	ver := buildinfo.Resolve()

	logf := func(format string, a ...interface{}) {
		line := "[" + time.Now().Format("15:04:05") + "] " + fmt.Sprintf(format, a...) + "\n"
		fmt.Print(line)
		if logFile != nil {
			io.WriteString(logFile, line)
		}
	}

	logf("Nomos desktop v%s — GUI: %s", ver, url)
	logf("каталоги: %s (plugins=%s, pipelines=%s, runs=%s)", workingDir(), dirs.Plugins, dirs.Pipelines, dirs.Runs)
	logf("лог: %s", logPath)
	// Код — только в консоль и в окно. В лог-файл он не пишется: лог обычно
	// переживает сессию, а код живёт 10 минут и обменивается на cookie.
	fmt.Printf("  код входа (одноразовый): %s\n", code)
	fmt.Println("  Закрыть окно (или Ctrl+C) — остановить Nomos.")

	httpErr := make(chan error, 1)
	go func() { httpErr <- srv.HTTPServer(ln.Addr().String()).Serve(ln) }()

	// окно (Windows) или браузер + ожидание (остальные ОС / фолбэк)
	desktop(url+"/?c="+code, debug, logf)

	ln.Close()
	select {
	case err := <-httpErr:
		if err != nil && !strings.Contains(err.Error(), "closed") {
			logf("сервер: %v", err)
		}
	default:
	}
}

// openLog — лог в каталоге настроек: %APPDATA%/WEDRA (Windows),
// $XDG_CONFIG_HOME/WEDRA (unix). Фолбэк — рядом с exe.
func openLog() (string, *os.File) {
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		dir, _ = os.Getwd()
	}
	dir = filepath.Join(dir, "WEDRA")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", nil
	}
	p := filepath.Join(dir, "wedragui.log")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return p, nil
	}
	return p, f
}

func workingDir() string {
	d, err := os.Getwd()
	if err != nil {
		return "."
	}
	return d
}

// openBrowser — URL в системный браузер (фолбэк и dev-путь).
func openBrowser(url string) {
	var (
		name string
		args []string
	)
	switch runtime.GOOS {
	case "darwin":
		name, args = "open", []string{url}
	case "windows":
		name, args = "rundll32", []string{"url.dll,FileProtocolHandler", url}
	default:
		name, args = "xdg-open", []string{url}
	}
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		fmt.Println("  (браузер не открылся:", err.Error(), "— URL выше)")
	}
}

// waitSignal — ждать Ctrl+C / SIGTERM (фолбэк-путь: консоль открыта).
func waitSignal() {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
}
