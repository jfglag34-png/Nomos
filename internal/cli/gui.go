package cli

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/wykserdex/wedra/internal/api"
	"github.com/wykserdex/wedra/internal/buildinfo"
	"github.com/wykserdex/wedra/internal/guidirs"
	"github.com/wykserdex/wedra/internal/plugin"
)

// RunGUI — wedra gui [--listen 127.0.0.1:8765] [--open] [--allow-remote]
//
//	[--public-host=имя[:порт]] [--trusted-proxy] [--origin-scheme=http|https] [--no-session]
//
// Каталоги: --plugins, --pipelines, --runs-dir; значения относительны CWD.
// По умолчанию: plugins/, examples/, var/runs/.
//
// H4: cookie сессии требуется на ВСЁ под /api/* (кроме /api/health и обмена
// кода), Host проверяется по allow-list, а не-loopback адрес требует явного
// --allow-remote + --public-host. Вход — по одноразовому коду из этого
// терминала; постоянного секрета в ссылке больше нет.
func RunGUI(args []string) {
	dirs, remaining, err := guidirs.Parse(args, guidirs.Default())
	if err != nil {
		fmt.Println("gui:", err)
		os.Exit(2)
	}
	if err := dirs.Ensure(); err != nil {
		fmt.Println("gui:", err)
		os.Exit(1)
	}
	args = remaining
	opts := api.ListenOptions{Addr: "127.0.0.1:8765"}
	open, noSession, printHelp := false, false, false
	// Политика доверия плагинов приходит из конфига оператора (H1) и не
	// связана с политикой прослушивания: allow-list Host и доверие к
	// плагинам решают разные вещи.
	trustCfg := ""
	// Пусто = встроенный фронтенд. Значение по умолчанию — пустое, а не
	// "web/static": подхват каталога из CWD делал GUI зависимым от места запуска.
	staticDir := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case len(a) > 9 && a[:9] == "--listen=":
			opts.Addr = a[9:]
		case a == "--listen" && i+1 < len(args):
			opts.Addr = args[i+1]
			i++
		case len(a) > 7 && a[:7] == "--port=":
			opts.Addr = "127.0.0.1:" + a[7:]
		case a == "--port" && i+1 < len(args):
			opts.Addr = "127.0.0.1:" + args[i+1]
			i++
		case strings.HasPrefix(a, "--trust-config="):
			trustCfg = strings.TrimPrefix(a, "--trust-config=")
		case len(a) > 9 && a[:9] == "--static=":
			staticDir = a[9:]
		case a == "--static" && i+1 < len(args):
			staticDir = args[i+1]
			i++
		case a == "--open":
			open = true
		case a == "--no-session":
			noSession = true
		case a == "--allow-remote":
			opts.AllowRemote = true
		case a == "--trusted-proxy":
			opts.TrustedProxy = true
		case len(a) > 14 && a[:14] == "--public-host=":
			opts.PublicHosts = append(opts.PublicHosts, a[14:])
		case a == "--public-host" && i+1 < len(args):
			opts.PublicHosts = append(opts.PublicHosts, args[i+1])
			i++
		case len(a) > 15 && a[:15] == "--origin-scheme=":
			opts.ExternalScheme = a[15:]
		case a == "--origin-scheme" && i+1 < len(args):
			opts.ExternalScheme = args[i+1]
			i++
		case a == "--help" || a == "-h":
			printHelp = true
		default:
			fmt.Printf("gui: неизвестный аргумент %q (см. wedra gui --help)\n", a)
			os.Exit(2)
		}
	}
	opts.NoSession = noSession
	if printHelp {
		guiUsage()
		return
	}
	// Проверка флагов — ДО поднятия сервера: не-loopback без --allow-remote
	// это отказ, а не предупреждение.
	if err := api.ValidateListen(opts); err != nil {
		fmt.Println("gui:", err)
		os.Exit(2)
	}

	// Политика доверия плагинам загружается до создания сервера: она нужна и
	// списку плагинов в интерфейсе, и запуску (H1).
	trusted, err := plugin.EffectiveAllowList(trustConfigPath(trustCfg))
	if err != nil {
		fmt.Println("gui: ошибка конфига доверия:", err)
		os.Exit(2)
	}
	if staticDir != "" {
		if info, err := os.Stat(staticDir); err != nil || !info.IsDir() {
			fmt.Printf("gui: --static=%s: %s\n", staticDir, "каталог фронтенда не читается")
			os.Exit(2)
		}
	}
	srv := api.NewServer(dirs.Plugins, dirs.Pipelines, dirs.Runs)
	srv.Trusted = trusted
	// Тот же путь, что пошёл в EffectiveAllowList: редактор плагинов на
	// платформе без изолятора обязан сказать, КУДА положить файл доверия, а не
	// только «не доверен».
	srv.TrustConfigPath = trustConfigPath(trustCfg)
	srv.StaticDir = staticDir
	ln, err := net.Listen("tcp", opts.Addr)
	if err != nil {
		fmt.Println("порт недоступен:", err)
		os.Exit(1)
	}
	// Политика (allow-list Host, внешняя схема, имя cookie) строится из
	// opts.Addr, а не из ln.Addr(): при --listen 0.0.0.0 фактический адрес —
	// [::]:порт, и в allow-list попал бы бесполезный «::». Порт — настоящий,
	// из слушателя.
	if _, port, err := net.SplitHostPort(ln.Addr().String()); err == nil {
		opts.ResolvedPort = port
	}
	if err := srv.ConfigureListen(opts); err != nil {
		fmt.Println("gui:", err)
		os.Exit(2)
	}

	code := ""
	if !noSession {
		if code, err = api.NewPairingCode(); err != nil {
			fmt.Println("не удалось сгенерировать код входа:", err)
			os.Exit(1)
		}
		srv.EnableSession(code)
	}
	ver := buildinfo.Resolve()
	link := "http://" + browserHost(ln.Addr().String(), opts) + "/"
	if api.LinkScheme(opts) == "https" {
		link = "https://" + browserHost(ln.Addr().String(), opts) + "/"
	}
	fmt.Printf("▶ GUI v%s — %s\n", ver, link)
	switch {
	case noSession:
		fmt.Println("  ⚠ --no-session: ВСЁ под /api/* открыто — любой локальный процесс читает журналы,")
		fmt.Println("    входы и выходы шагов и одобряет гейты. Для локальной работы флаг не нужен.")
	case len(opts.PublicHosts) > 0:
		fmt.Printf("  ⚠ --allow-remote: сервер доступен из сети (Host: %s). Вход — по одноразовому коду ниже,\n", strings.Join(opts.PublicHosts, ", "))
		fmt.Println("    а cookie ставится только на разрешённые Host; чужие отвергаются (DNS-rebinding).")
	}
	if code != "" {
		fmt.Printf("  код входа (одноразовый): %s\n", code)
		fmt.Println("  открыть GUI: ссылка выше с --open, либо откройте её и введите код на странице.")
		fmt.Println("  второй браузер/вкладка: нажмите Enter в этом терминале — выдадим новый код.")
	}
	fmt.Println("  API: /api/health (без входа), остальное /api/* — с cookie сессии; обмен кода: /api/session")
	if staticDir != "" {
		fmt.Printf("  Frontend: %s с диска (--static) — правки JS видны после перезагрузки страницы\n", staticDir)
	} else {
		fmt.Println("  Frontend: встроенный GUI (go:embed, v0.7) — консоль: раны, live-журнал, DAG; /editor/ — редактор пайплайнов")
	}
	fmt.Println("  Ctrl+C — остановить")

	if code != "" {
		go watchStdinForNewCode(srv)
	}
	if open {
		go func() {
			// Окно открываем сразу с кодом: сервер обменяет его на cookie и
			// перенаправит на адрес без кода. Постоянного секрета в ссылке нет.
			url := link
			if code != "" {
				url += "?c=" + code
			}
			var cmd *exec.Cmd
			switch runtime.GOOS {
			case "darwin":
				cmd = exec.Command("open", url)
			case "windows":
				cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
			default:
				cmd = exec.Command("xdg-open", url)
			}
			_ = cmd.Start()
		}()
	}

	server := srv.HTTPServer(opts.Addr)
	if err := server.Serve(ln); err != nil {
		fmt.Println("ошибка сервера:", err)
		os.Exit(1)
	}
}

// guiUsage — справка по флагам: не-loopback и прокси требуют явного решения.
func guiUsage() {
	fmt.Println(`wedra gui [--listen 127.0.0.1:8765] [--port N] [--open] [--no-session]
             [--allow-remote] [--public-host=имя[:порт]]... [--trusted-proxy] [--origin-scheme=http|https]
             [--plugins=<dir>] [--pipelines=<dir>] [--runs-dir=<dir>] [--static=<dir>]

Вход: одноразовый код печатается здесь и обменивается на cookie. Всё под /api/*
(кроме /api/health и POST /api/session) без cookie → 401 E_SESSION_REQUIRED.
Host проверяется по allow-list: 127.0.0.1, localhost, [::1] и явно заданные
--public-host; чужой Host → 403 E_HOST_NOT_ALLOWED (защита от DNS-rebinding).

--listen на не-loopback адресе требует --allow-remote (иначе отказ), а при
0.0.0.0/:: — ещё и --public-host=<имя или IP>: иначе запросы пришлось бы
отвергать все. --no-session вместе с внешним доступом запрещён.

X-Forwarded-Proto читается только с --trusted-proxy (Secure у cookie);
X-Forwarded-Host не читается никогда.

--static=<dir> отдаёт фронтенд с диска (dev-режим: правки JS без пересборки).
Без флага работает встроенный (go:embed) GUI: содержимое каталога запуска на
отдаваемый фронтенд не влияет.`)
}

// PrintGUIUsage — справка gui для `wedra gui --help`.
func PrintGUIUsage() { guiUsage() }

// browserHost — как открыть GUI в браузере, когда слушаем все интерфейсы:
// localhost вместо 0.0.0.0 (иначе браузер уходит в прокси/никуда).
func browserHost(listenAddr string, opts api.ListenOptions) string {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return listenAddr
	}
	if len(opts.PublicHosts) > 0 {
		if h, _, err := net.SplitHostPort(opts.PublicHosts[0]); err == nil && h != "" {
			return net.JoinHostPort(h, port)
		}
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return "localhost:" + port
	}
	return listenAddr
}

// watchStdinForNewCode — Enter в этом терминале = новый одноразовый код.
// Нужен второму браузеру: код одноразовый, а сессия у уже вошедшей вкладки
// живёт. Не мешает CI: там stdin не терминал, и горутина молча выходит.
func watchStdinForNewCode(srv *api.Server) {
	fi, err := os.Stdin.Stat()
	if err != nil || (fi.Mode()&os.ModeCharDevice) == 0 {
		return
	}
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		// Любая пустая строка (Enter) — запрос нового кода; непустую считаем
		// «пользователь что-то ввёл не туда» и просто игнорируем.
		if strings.TrimSpace(sc.Text()) != "" {
			continue
		}
		fresh, err := api.NewPairingCode()
		if err != nil {
			return
		}
		srv.RotatePairingCode(fresh)
		fmt.Printf("  новый код входа (одноразовый): %s\n", fresh)
	}
}
