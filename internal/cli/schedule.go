package cli

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wykserdex/wedra/internal/core"
	"github.com/wykserdex/wedra/internal/cron"
	"github.com/wykserdex/wedra/internal/execution"
	"github.com/wykserdex/wedra/internal/gate"
	"github.com/wykserdex/wedra/internal/guidirs"
	"github.com/wykserdex/wedra/internal/journal"
	"github.com/wykserdex/wedra/internal/pipeline"
	"github.com/wykserdex/wedra/internal/plugin"
)

// RunSchedule — wedra schedule [--pipelines=dir] [--runs-dir=dir]
// Сканирует пайплайны с schedule и показывает next_run
func RunSchedule(args []string) {
	dirs, _, err := guidirs.Parse(args, guidirs.Default())
	if err != nil {
		fmt.Println("schedule:", err)
		os.Exit(2)
	}
	// сканируем examples/
	files, _ := cron.ScanPipelines(dirs.Pipelines)
	if len(files) == 0 {
		files, _ = cron.ScanPipelines("examples")
	}
	if len(files) == 0 {
		fmt.Println("нет пайплайнов с schedule (добавь schedule.cron в YAML)")
		return
	}
	now := time.Now()
	fmt.Printf("Найдено %d планировщиков:\n", len(files))
	for _, f := range files {
		pf, err := pipeline.LoadPipelineFile(f)
		if err != nil {
			fmt.Printf("  %s — ошибка парсинга: %v\n", f, err)
			continue
		}
		sched := pf.Pipeline.Schedule
		if sched == nil {
			continue
		}
		next := cron.NextRunFor(sched, now)
		enabled := ""
		if !sched.IsEnabled() {
			enabled = " [выключен]"
		}
		nextStr := "—"
		if next != nil {
			nextStr = next.Format("02.01 15:04 MST") + fmt.Sprintf(" (%s)", cron.Human(sched.Cron))
		}
		fmt.Printf("  %s → %s%s  cron=%q tz=%s overlap=%s human_gate=%s\n", pf.Pipeline.Name, nextStr, enabled, sched.Cron, sched.Timezone, sched.Overlap, sched.HumanGate)
		if sched.Alerts.OnWaiting != "" || sched.Alerts.Webhook != "" {
			fmt.Printf("    alerts: on_waiting=%s webhook=%s\n", sched.Alerts.OnWaiting, sched.Alerts.Webhook)
		}
	}
}

// RunDaemon — wedra daemon [--pipelines=dir] [--runs-dir=dir]
// Тикает каждую минуту, запускает due пайплайны
func RunDaemon(args []string) {
	dirs, _, err := guidirs.Parse(args, guidirs.Default())
	if err != nil {
		fmt.Println("daemon:", err)
		os.Exit(2)
	}
	dirs.Ensure()
	fmt.Printf("daemon: pipelines=%s runs=%s (Ctrl+C to stop)\n", dirs.Pipelines, dirs.Runs)

	// Раздача ссылок одобрения. Ключ подписи живёт рядом с ранами, чтобы
	// ссылка пережила перезапуск демона: человек получил её, демон рестартнул,
	// и она должна остаться рабочей.
	//
	// Адрес для ссылки и адрес слушателя — разные вещи: слушатель может быть
	// на 127.0.0.1 (за обратным прокси), а в ссылке может стоять публичный
	// адрес, иначе человек её не откроет.
	approveListen := strings.TrimSpace(os.Getenv("WEDRA_APPROVE_LISTEN"))
	if approveListen == "" {
		approveListen = "127.0.0.1:8766"
	}
	approveURL := strings.TrimSpace(os.Getenv("WEDRA_APPROVE_URL"))
	if approveURL == "" {
		approveURL = "http://" + approveListen
	}
	signer, err := gate.LoadSigner(filepath.Join(dirs.Runs, "approve.key"))
	if err != nil {
		fmt.Println("daemon: ключ подписи ссылок одобрения:", err)
		os.Exit(2)
	}
	approver := gate.NewApprover(signer, approveURL, time.Hour)
	ln, lerr := net.Listen("tcp", approveListen)
	if lerr != nil {
		fmt.Println("daemon: слушатель ссылок одобрения:", lerr)
		os.Exit(2)
	}
	go func() {
		if err := http.Serve(ln, approver.Handler()); err != nil {
			fmt.Fprintln(os.Stderr, "daemon: раздача ссылок:", err)
		}
	}()
	fmt.Printf("daemon: ссылки одобрения %s (слушатель %s, ключ %s)\n",
		approveURL, approveListen, filepath.Join(dirs.Runs, "approve.key"))

	daemon := &cron.Daemon{
		PipelinesDir: dirs.Pipelines,
		RunsDir:      dirs.Runs,
		Runner: func(pipelineFile string, autoApprove bool) (string, error) {
			// загружаем pipeline, валидируем, запускаем
			pf, err := pipeline.LoadPipelineFile(pipelineFile)
			if err != nil {
				return "", err
			}
			// gate handling: autoApprove => --yes
			yes := autoApprove
			// если human_gate: wait => no auto approve, ран встанет в waiting
			trusted, _ := plugin.LoadTrustConfig(trustConfigPath(""))
			_ = pf
			_ = journal.DefaultRunsDir
			// RunID задаётся ДО запуска: фабрика гейта вызывается внутри
			// прогона, и без заранее известного id ссылка не знала бы, к какому
			// рану относится.
			runID, err := execution.NewRunID(pf.Pipeline.Name)
			if err != nil {
				return "", err
			}
			opts := execution.RunOptions{
				Yes:     yes,
				RunsDir: dirs.Runs,
				Trusted: trusted,
				RunID:   runID,
			}
			// Гейт ждёт решения человека, а на сервере человека за терминалом
			// нет. Поэтому гейт получает КАНАЛ, а решение приходит по
			// подписанной ссылке: демон печатает её, человек открывает её где
			// угодно — с телефона, из мессенджера, с другой машины.
			//
			// Канал вместо stdin обязателен: у демона нет терминала, и чтение
			// os.Stdin в фоне либо сразу даёт EOF, либо блокируется навсегда в
			// зависимости от того, как запустили.
			sched := pf.Pipeline.Schedule
			timeout, onTimeout := time.Duration(0), ""
			if sched != nil {
				timeout, onTimeout = sched.GateTimeout.Duration, sched.OnTimeout
			}
			opts.GateUI = func(st *pipeline.Step) gate.GateUI {
				ui := gate.NewChannelUI()
				link, rerr := approver.Register(runID, st.ID, ui, st)
				if rerr != nil {
					fmt.Fprintf(os.Stderr, "daemon: ссылка одобрения не выдана: %v\n", rerr)
				} else {
					fmt.Fprintf(os.Stderr, "daemon: гейт %s ждёт решения — откройте %s\n", st.ID, link)
				}
				if timeout > 0 {
					return gate.NewTimeoutUI(ui, timeout, onTimeout)
				}
				return ui
			}
			// run — execution.Run хочет *PipelineFile + Engine
			eng := core.NewEngine()
			_, err = execution.Run(pf, eng, opts)
			approver.Unregister(runID, "")
			if err != nil {
				return "", err
			}
			return pf.Pipeline.Name, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	// ловим Ctrl+C уже в caller? пока просто run
	_ = cancel
	if err := daemon.Run(ctx); err != nil && err != context.Canceled {
		fmt.Println("daemon:", err)
	}
}
