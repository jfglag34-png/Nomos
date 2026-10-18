package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/wykserdex/wedra/internal/core"
	"github.com/wykserdex/wedra/internal/cron"
	"github.com/wykserdex/wedra/internal/execution"
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
			opts := execution.RunOptions{
				Yes:     yes,
				RunsDir: dirs.Runs,
				Trusted: trusted,
			}
			// run — execution.Run хочет *PipelineFile + Engine
			eng := core.NewEngine()
			_, err = execution.Run(pf, eng, opts)
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
