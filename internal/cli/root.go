package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/wykserdex/wedra/internal/buildinfo"
)

func Run() {
	applyNomosEnvironment()
	if len(os.Args) < 2 {
		printHelp()
		os.Exit(2)
	}
	cmd := os.Args[1]
	if cmd == "help" || cmd == "--help" || cmd == "-h" {
		printHelp()
		return
	}
	switch cmd {
	case "fleet":
		RunFleet(os.Args[2:])
	case "pipeline":
		if helpRequested(os.Args[2:]) {
			printHelp()
			return
		}
		handlePipeline(os.Args[2:])
	case "plugin":
		// Глобальная справка — только когда её просят У САМОЙ команды plugin.
		// Раньше helpRequested смотрел во все аргументы, и `wedra plugin create
		// --help` упирался в общий список команд вместо справки по create.
		if len(os.Args) > 2 && isHelpFlag(os.Args[2]) {
			printHelp()
			return
		}
		handlePlugin(os.Args[2:])
	case "runs", "run":
		if helpRequested(os.Args[2:]) {
			printHelp()
			return
		}
		if cmd == "runs" {
			handleRuns(os.Args[2:])
		} else {
			handlePipeline(append([]string{"run"}, os.Args[2:]...))
		}
	case "gui", "serve":
		if helpRequested(os.Args[2:]) {
			// у gui свои флаги (--allow-remote/--public-host/--trusted-proxy),
			// о которых короткая справка знает: молча печатать общую — значит
			// человек не увидит главного про внешний доступ
			PrintGUIUsage()
			return
		}
		RunGUI(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Printf("Nomos v%s, protocol v0.2 (WEDRA-compatible)\n", buildinfo.Resolve())
	case "registry":
		if helpRequested(os.Args[2:]) {
			printHelp()
			return
		}
		RunRegistryValidate(os.Args[2:])
	case "validate":
		if helpRequested(os.Args[2:]) {
			printHelp()
			return
		}
		handlePipeline(append([]string{"validate"}, os.Args[2:]...))
	case "mcp":
		if helpRequested(os.Args[2:]) {
			printHelp()
			return
		}
		RunMCP(os.Args[2:])
	case "approve":
		if helpRequested(os.Args[2:]) {
			printHelp()
			return
		}
		RunApprove(os.Args[2:])
	case "demo":
		if helpRequested(os.Args[2:]) {
			printHelp()
			return
		}
		RunDemo(os.Args[2:])
	case "schedule":
		if helpRequested(os.Args[2:]) {
			printHelp()
			return
		}
		RunSchedule(os.Args[2:])
	case "daemon":
		if helpRequested(os.Args[2:]) {
			printHelp()
			return
		}
		RunDaemon(os.Args[2:])
	case "doctor":
		if helpRequested(os.Args[2:]) {
			printHelp()
			return
		}
		RunDoctor(os.Args[2:])
	case "check":
		// Своя справка: у check много флагов, общий help их не покажет.
		if helpRequested(os.Args[2:]) {
			printCheckHelp()
			return
		}
		os.Exit(RunCheck(os.Args[2:]))
	default:
		fmt.Printf("неизвестная команда %q\n", cmd)
		printHelp()
		os.Exit(2)
	}
}

func printHelp() {
	ver := buildinfo.Resolve()
	fmt.Printf(`Nomos — контрактный исполнитель цепочек с человеком в петле (v%s)

Команды (мясо, не косметика):
  nomos pipeline run <file.yaml> [--yes] [--resume=<run_id>] [--runs-dir=var/runs] [--store=fs|json] [--no-auto-approve] [--deny-untrusted-plugins] [--allow-untrusted-plugins]
  nomos fleet --config=fleet.yaml [--check]              # parallel Ubuntu scheduler + webhook triggers
  nomos schedule [--pipelines=<dir>] [--runs-dir=<dir>]      # показать next_run для всех cron
  nomos daemon [--pipelines=<dir>] [--runs-dir=<dir>]        # демон: тикает каждую минуту, запускает due пайплайны (solo vs scheduled)
  nomos pipeline install <name|file.yaml|url> [--registry=<url|path>]  # пресет + автоустановка плагинов
  nomos pipeline validate <file.yaml> [--json]      # --json: {ok, issues[]} с кодами (ERRORS.md)
  nomos pipeline plan <file.yaml>
  nomos pipeline lint <file.yaml>          # validate + file_ref error
  nomos plugin install <name>[@version] [--registry=<url|path>]        # из реестра в plugins/
  nomos plugin validate <dir>
  nomos plugin test <dir> [--conformance] [--json]
  nomos plugin test --conformance [--json] [--fixtures=dir]
  nomos plugin create <dir> [--author --description --example]
  nomos plugin inspect <dir>
  nomos plugin search <query>              # поиск по official/community
  nomos plugin list                        # список всех плагинов
  nomos registry validate [--registry=<url|path>] [--local-source=<dir>]
                                                 # v0.17: trust-гейт реестра (манифест, id, конформность)
  nomos runs list [var/runs]               # список прогонов (fs + json)
  nomos runs show <run_id> [var/runs]      # журнал + context + artifacts
  nomos runs resume <run_id> <pipeline.yaml> [--yes] [--runs-dir=var/runs] [--store=fs|json] [--db-path=file] [--no-auto-approve] [--deny-untrusted-plugins] [--allow-untrusted-plugins]
  nomos gui [--listen 127.0.0.1:8765] [--open] [--no-session] [--allow-remote] [--public-host=<имя:порт>] [--trusted-proxy] [--plugins=<dir>] [--pipelines=<dir>] [--runs-dir=<dir>]
                                                  # консоль; вход — по одноразовому коду из терминала, /api/* кроме /api/health требует cookie
  nomos mcp --plugins=<dir> [--workdir=<dir>] [--no-gui]  # MCP-сервер (stdio) для LLM-агентов
  nomos approve <run_id> <step_id>                # только интерактивный TTY
  nomos demo [--runs-dir=<dir>]                    # автономная цепочка: ноль git, ноль Python, ноль сети
  nomos doctor [--json] [--plugins=<dir>]          # что сломано в окружении и что доустановить; ничего не ставит и не запускает

Совместимость:
  nomos check [--list|--fast|--census|--only=<step>] # ЕДИНАЯ проверка проекта: fmt, vet, mod, build, test, conformance, pipelines, plugins, registry
  nomos run <file.yaml> == pipeline run
  nomos validate <file.yaml> == pipeline validate

Версия: v%s (честная 0.x), протокол v0.2
См. protocol/v0.2/PROTOCOL.md, protocol/v0.2/ERRORS.md, docs/mcp.md, CHANGELOG.md
`, ver, ver)
}

// isHelpFlag — сам ли аргумент просит справку.
func isHelpFlag(a string) bool {
	return a == "help" || a == "--help" || a == "-h"
}

func helpRequested(args []string) bool {
	for _, a := range args {
		if a == "help" || a == "--help" || a == "-h" {
			return true
		}
	}
	return false
}

func handlePipeline(args []string) {
	if len(args) == 0 {
		printHelp()
		os.Exit(2)
	}
	sub := args[0]
	switch sub {
	case "run":
		RunPipelineRun(args[1:])
	case "install":
		RunPipelineInstall(args[1:])
	case "validate":
		RunPipelineValidate(args[1:])
	case "lint":
		RunPipelineValidate(append(args[1:], "--lint"))
	case "plan":
		RunPipelinePlan(args[1:])
	default:
		fmt.Printf("неизвестная pipeline команда %q\n", sub)
		os.Exit(2)
	}
}

func handleRuns(args []string) {
	if len(args) == 0 {
		RunRunsList(nil)
		return
	}
	sub := args[0]
	switch sub {
	case "list":
		RunRunsList(args[1:])
	case "show":
		RunRunsShow(args[1:])
	case "resume":
		RunRunsResume(args[1:])
	case "prune":
		RunRunsPrune(args[1:])
	default:
		if strings.HasPrefix(sub, "-") {
			fmt.Printf("неизвестная runs команда %q\n", sub)
			os.Exit(2)
		}
		if len(args) == 1 {
			RunRunsShow(args)
		} else {
			fmt.Printf("неизвестная runs команда %q\n", sub)
			os.Exit(2)
		}
	}
}

func handlePlugin(args []string) {
	if len(args) == 0 {
		printHelp()
		os.Exit(2)
	}
	sub := args[0]
	switch sub {
	case "validate":
		RunPluginValidate(args[1:])
	case "test":
		RunPluginTest(args[1:])
	case "create":
		RunPluginCreate(args[1:])
	case "inspect":
		RunPluginInspect(args[1:])
	case "install":
		RunPluginInstall(args[1:])
	case "search":
		RunPluginSearch(args[1:])
	case "list":
		RunPluginList(args[1:])
	default:
		fmt.Printf("неизвестная plugin команда %q\n", sub)
		os.Exit(2)
	}
}
