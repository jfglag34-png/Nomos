package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/wykserdex/wedra/internal/journal"
)

// RunRunsPrune — wedra runs prune [--keep-last=N] [--keep-days=N] [--dry-run]
//
// Убирает старые прогоны с диска. Без флагов политики ничего не удаляется:
// команда без спроса не трогает данные.
//
// --dry-run ничего не удаляет и только показывает, что было бы убрано.
func RunRunsPrune(args []string) {
	runsDir := journal.DefaultRunsDir()
	storeType := "fs"
	dbPath := ""
	policy := journal.PrunePolicy{}
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "--keep-last="):
			n, err := strconv.Atoi(strings.TrimPrefix(a, "--keep-last="))
			if err != nil || n < 0 {
				fmt.Printf("--keep-last=%s: нужно неотрицательное число\n", strings.TrimPrefix(a, "--keep-last="))
				os.Exit(2)
			}
			policy.KeepLast = n
		case strings.HasPrefix(a, "--keep-days="):
			n, err := strconv.Atoi(strings.TrimPrefix(a, "--keep-days="))
			if err != nil || n < 0 {
				fmt.Printf("--keep-days=%s: нужно неотрицательное число\n", strings.TrimPrefix(a, "--keep-days="))
				os.Exit(2)
			}
			policy.KeepDays = n
		case a == "--dry-run":
			policy.DryRun = true
		case strings.HasPrefix(a, "--runs-dir="):
			runsDir = strings.TrimPrefix(a, "--runs-dir=")
		case strings.HasPrefix(a, "--store="):
			storeType = strings.TrimPrefix(a, "--store=")
		case strings.HasPrefix(a, "--db-path="):
			dbPath = strings.TrimPrefix(a, "--db-path=")
		case strings.HasPrefix(a, "-"):
			fmt.Printf("неизвестный флаг %q\n", a)
			os.Exit(2)
		default:
			runsDir = a
		}
	}
	if policy.Empty() {
		fmt.Println("нечего делать: укажите --keep-last=N или --keep-days=N")
		fmt.Println("без них команда не удаляет НИ ОДНОГО прогона")
		return
	}
	var store journal.RunStore
	if storeType == "json" {
		store = journal.NewJsonStore(runsDir, dbPath)
	} else {
		store = journal.NewFilesystemStore(runsDir)
	}
	pruner, ok := store.(interface {
		Prune(journal.PrunePolicy, time.Time) (journal.PruneResult, error)
	})
	if !ok {
		fmt.Println("это хранилище не умеет чистку")
		os.Exit(2)
	}
	res, err := pruner.Prune(policy, time.Now())
	if err != nil {
		fmt.Println("ошибка чистки:", err)
		os.Exit(1)
	}
	verb := "удалено"
	if policy.DryRun {
		verb = "было бы удалено"
	}
	if len(res.Removed) == 0 {
		fmt.Printf("нечего удалять (осталось %d)\n", res.Kept)
		return
	}
	fmt.Printf("%s %d прогонов, освобождено %s; осталось %d\n",
		verb, len(res.Removed), humanBytes(res.FreedBytes), res.Kept)
	for _, id := range res.Removed {
		fmt.Println("  -", id)
	}
}

// humanBytes — размер по-человечески. Точности тут не нужно, округление
// вниз сделано намеренно: «меньше 1 МБ» честнее, чем «0.99 МБ».
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d Б", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 3; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cБ", float64(n)/float64(div), "KMGT"[exp])
}
