package execution

import (
	"sync/atomic"

	"github.com/wykserdex/wedra/internal/pipeline"
)

type loopBudgetCounter struct {
	total int64
	limit int
}

func (c *loopBudgetCounter) effectiveLimit() int {
	if c.limit <= 0 {
		return pipeline.MaxTotalLoopBudget
	}
	return c.limit
}

// consumeLoopIteration — записать одну итерацию. false = бюджет исчерпан,
// и ран обязан упасть с resource_limit, а не продолжить.
func (o *RunOptions) consumeLoopIteration() bool {
	if o.loopBudget == nil {
		return true
	}
	limit := o.loopBudget.effectiveLimit()
	return atomic.AddInt64(&o.loopBudget.total, 1) <= int64(limit)
}

// loopIterations — сколько итераций loop исполнено за ран (для отчёта).
func (o *RunOptions) loopIterations() int {
	if o.loopBudget == nil {
		return 0
	}
	return int(atomic.LoadInt64(&o.loopBudget.total))
}

// ErrCancelled — ран отменён через RunOptions.Ctx. errors.Is(err, ErrCancelled).
