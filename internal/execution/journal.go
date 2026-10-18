package execution

import (
	"errors"

	"github.com/wykserdex/wedra/internal/journal"
	"github.com/wykserdex/wedra/internal/runctx"
)

func failEvent(j *journal.Journal, ctx *runctx.Ctx, err error, extra map[string]interface{}) {
	kv := map[string]interface{}{"error": err.Error(), "code": ErrorCode(err)}
	for k, v := range extra {
		kv[k] = v
	}
	if errors.Is(err, ErrCancelled) {
		j.Event("run_cancelled", kv)
	} else {
		j.Event("run_failed", kv)
	}
	if ctx != nil {
		// P2 F-04: потеря этого снапшота (снапшот после терминального
		// события — тоже состояние для --resume) уже зафиксирована в
		// журнале самим Journal: событие snapshot_lost + счётчик. Ран уже
		// неуспешен по своей причине — её код и не перебиваем.
		j.Snapshot(ctx)
	}
}

func journalWriteError(j *journal.Journal) error {
	if j == nil {
		return nil
	}
	if n := j.WriteErrors(); n > 0 {
		return runErr("journal_write", "журнал неполный: потеряно записей: %d", n)
	}
	return nil
}

// priorSnapshotLosses — сколько потерь снапшота уже записано в журнал до
// текущего рана. При --resume журнал общий с прошлым прогоном, поэтому его
// счётчик у нового Journal-объекта пуст, а состояние на диске — то же самое.
// Ошибка чтения трактуется как «потерь не найдено»: нечитаемый журнал и так
// ломает resume раньше (resumeCursor).
func priorSnapshotLosses(dir string) int {
	n := 0
	if _, err := journal.NewReader(dir).ScanMeta(func(meta journal.EventMeta) error {
		if meta.Type == "snapshot_lost" {
			n++
		}
		return nil
	}); err != nil {
		return 0
	}
	return n
}

// snapshotLossError — P2 F-04: терминальная ошибка потерянных снапшотов
// контекста (nil, если потерь нет). Источник истины — журнал: потеря
// считается там же, где пишется событие snapshot_lost, поэтому её нельзя
// потерять, «забыв проверить» ошибку в горячем пути. В сообщении только
// причина и размер: ни данных контекста, ни секретов.
func snapshotLossError(j *journal.Journal, prior int) error {
	if j == nil {
		return nil
	}
	n := j.SnapshotLosses() + prior
	if n == 0 {
		return nil
	}
	cause := j.SnapshotLossError()
	if cause == nil {
		// потери только прошлого рана: причина осталась в его журнале
		cause = errors.New("потери прошлого прогона (см. событие snapshot_lost)")
	}
	return runErr("snapshot_lost", "снапшот контекста потерян: не записано %d (context.json за ран неполон, --resume восстановит не всё): %v", n, cause)
}

// finish — терминальная запись рана. Пока снапшоты целы, это run_end. Если
// снапшот потерян, run_end был бы враньём: на диске лежит устаревшее
// состояние, а журнал сказал бы «ок». Поэтому журнал получает
// run_failed/snapshot_lost, а вызывающий — ошибку с кодом snapshot_lost
// (CLI: ненулевой код; API/MCP: статус failed). Потеря событий журнала без
// потери снапшота — прежний journal_write уже после run_end.
func finish(j *journal.Journal, prior int, kv map[string]interface{}) error {
	if err := snapshotLossError(j, prior); err != nil {
		failKV := map[string]interface{}{
			"error":           err.Error(),
			"code":            "snapshot_lost",
			"snapshot_losses": j.SnapshotLosses() + prior,
		}
		j.Event("run_failed", failKV)
		return err
	}
	j.Event("run_end", kv)
	return journalWriteError(j)
}
