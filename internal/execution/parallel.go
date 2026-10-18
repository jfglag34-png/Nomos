package execution

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/wykserdex/wedra/internal/journal"
	"github.com/wykserdex/wedra/internal/pipeline"
	"github.com/wykserdex/wedra/internal/runctx"
)

type stepSegment struct {
	parallel bool
	group    string
	steps    []*pipeline.Step
}

// segmentSteps — разбивает список шагов на одиночные шаги и смежные блоки
// с одинаковым parallel_group (v0.20). Группа из одного шага — обычный шаг
// (валидатор уже предупреждает).
func segmentSteps(steps []*pipeline.Step) []stepSegment {
	var segs []stepSegment
	for i := 0; i < len(steps); {
		g := steps[i].ParallelGroup
		if g != "" {
			j := i
			for j < len(steps) && steps[j].ParallelGroup == g {
				j++
			}
			segs = append(segs, stepSegment{parallel: j-i > 1, group: g, steps: steps[i:j]})
			i = j
			continue
		}
		segs = append(segs, stepSegment{steps: steps[i : i+1]})
		i++
	}
	return segs
}

// runParallelSegments — исполняет сегменты (шаги/группы) последовательно,
// параллельную группу — goroutine'ами с барьером. Возвращает actions в
// порядке списка шагов. stopOnAbort: при on_error=stop в одиночном шаге
// последующие шаги не исполняются (abort_item присутствует в actions).
// Внутри параллельной группы stop = остановка рана (барьер не терпит половин).
func runParallelSegments(eng Engine, pf *pipeline.PipelineFile, steps []*pipeline.Step, ctx *runctx.Ctx, j *journal.Journal, opts RunOptions, stopOnAbort bool) ([]string, error) {
	var actions []string
	for _, seg := range segmentSteps(steps) {
		if opts.cancelled() {
			return actions, ErrCancelled
		}
		if seg.parallel {
			acts, err := runParallelGroup(eng, pf, seg, ctx, j, opts)
			if err != nil {
				return actions, err
			}
			actions = append(actions, acts...)
			continue
		}
		st := seg.steps[0]
		action, err := runStepFlow(eng, pf, st, ctx, j, opts)
		if err != nil {
			return actions, err
		}
		actions = append(actions, action)
		if stopOnAbort && action == "abort_item" {
			return actions, nil
		}
	}
	return actions, nil
}

// runParallelGroup — v0.20: ветки группы исполняются параллельно, каждая на
// своей копии контекста; после барьера выходы сливаются в общий контекст в
// порядке списка (детерминизм). Платформенная ошибка или on_error=stop в
// любой ветке останавливает ран. human_gate и foreach в группах запрещены
// валидатором.
func runParallelGroup(eng Engine, pf *pipeline.PipelineFile, seg stepSegment, ctx *runctx.Ctx, j *journal.Journal, opts RunOptions) ([]string, error) {
	if len(seg.steps) > pipeline.MaxParallelWidth {
		return nil, runErr("resource_limit", "parallel_group %q: %d шагов, максимум %d", seg.group, len(seg.steps), pipeline.MaxParallelWidth)
	}
	ids := make([]string, 0, len(seg.steps))
	for _, st := range seg.steps {
		ids = append(ids, st.ID)
	}
	start := time.Now()
	j.Event("parallel_start", map[string]interface{}{"group": seg.group, "steps": ids})
	opts.logf("  ‖ группа %q параллельно: %s", seg.group, strings.Join(ids, ", "))

	type branchOut struct {
		st     *pipeline.Step
		out    map[string]interface{}
		action string
		err    error
	}
	outs := make([]branchOut, len(seg.steps))
	var wg sync.WaitGroup
	for i, st := range seg.steps {
		wg.Add(1)
		go func(i int, st *pipeline.Step) {
			defer wg.Done()
			bctx, err := cloneCtx(ctx)
			if err != nil {
				outs[i] = branchOut{st: st, err: runErr("context_serialization", "параллельная группа %q, шаг %s: %v", seg.group, st.ID, err)}
				return
			}
			action, err := runStepFlow(eng, pf, st, bctx, j, opts)
			bo := branchOut{st: st, action: action, err: err}
			if err == nil {
				if m, ok := bctx.Data["steps"].(map[string]interface{}); ok {
					if v, ok := m[st.ID].(map[string]interface{}); ok {
						bo.out = v
					}
				}
			}
			outs[i] = bo
		}(i, st)
	}
	wg.Wait()

	for _, o := range outs {
		if o.err != nil {
			return nil, fmt.Errorf("параллельная группа %q, шаг %s: %w", seg.group, o.st.ID, o.err)
		}
		if o.action == "abort_item" {
			return nil, fmt.Errorf("параллельная группа %q, шаг %s: on_error=stop — ран остановлен", seg.group, o.st.ID)
		}
	}
	var actions []string
	statuses := map[string]string{}
	for _, o := range outs {
		if o.out != nil {
			ctx.SetStep(o.st.ID, o.out)
			statuses[o.st.ID] = "ok"
			actions = append(actions, "ok")
		} else {
			statuses[o.st.ID] = "skipped"
			actions = append(actions, "skipped")
		}
	}
	j.Snapshot(ctx)
	j.Event("parallel_end", map[string]interface{}{"group": seg.group, "statuses": statuses, "duration_ms": time.Since(start).Milliseconds()})
	return actions, nil
}

// cloneCtx — глубокая копия контекста (JSON-roundtrip; значения контекста
// всегда JSON-безопасны).
func cloneCtx(ctx *runctx.Ctx) (*runctx.Ctx, error) {
	if ctx == nil {
		return nil, fmt.Errorf("context: nil")
	}
	b, err := json.Marshal(ctx.Data)
	if err != nil {
		return nil, fmt.Errorf("context: не сериализуется: %w", err)
	}
	var data map[string]interface{}
	if err := json.Unmarshal(b, &data); err != nil {
		return nil, fmt.Errorf("context: не десериализуется: %w", err)
	}
	return &runctx.Ctx{Data: data}, nil
}

// runStepFlow — обёртка над runStep с управляющим потоком v0.20:
// when-условие (skip) и foreach на уровне шага (per-item мини-цикл).
