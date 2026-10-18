package execution

import (
	"testing"

	"github.com/wykserdex/wedra/internal/pipeline"
	"github.com/wykserdex/wedra/internal/runctx"
)

func TestRunStepForeachValidation(t *testing.T) {
	if ErrorCode(nil) != "" { t.Error("empty") }
	if ErrorCode(&RunError{Code:"x", Err: &fakeErr{}}) != "x" { t.Error("code") }
}

type fakeErr struct{}
func (f *fakeErr) Error() string { return "fake" }

func TestLoopBudget(t *testing.T) {
	opts := RunOptions{loopBudget: &loopBudgetCounter{limit: 2}}
	if !opts.consumeLoopIteration() { t.Fatal("first") }
	if !opts.consumeLoopIteration() { t.Fatal("second") }
	if opts.consumeLoopIteration() { t.Fatal("third should fail") }
	if opts.loopIterations()!=3 { t.Fatalf("got %d", opts.loopIterations()) }
	if (&RunOptions{}).consumeLoopIteration()!=true { t.Error("nil budget") }
	if (&RunOptions{}).loopIterations()!=0 { t.Error("0") }
	if (&loopBudgetCounter{}).effectiveLimit()!=pipeline.MaxTotalLoopBudget { t.Error("default limit") }
	if (&loopBudgetCounter{limit:5}).effectiveLimit()!=5 { t.Error("5") }
}

func TestCloneCtxDeep(t *testing.T) {
	if _, err := cloneCtx(nil); err == nil { t.Fatal("nil") }
	ctx := runctx.NewCtx(map[string]interface{}{"a": "1"})
	c2, err := cloneCtx(ctx)
	if err != nil { t.Fatal(err) }
	c2.SetInput("b", "2")
	if _, ok := ctx.Get("input.b"); ok { t.Fatal("clone not deep") }
}

func TestSegmentSteps(t *testing.T) {
	steps := []*pipeline.Step{
		{ID: "a"},
		{ID: "b", ParallelGroup: "g1"},
		{ID: "c", ParallelGroup: "g1"},
		{ID: "d"},
	}
	segs := segmentSteps(steps)
	if len(segs) != 3 { t.Fatalf("segs %v", segs) }
	if !segs[1].parallel || segs[1].group!="g1" { t.Fatalf("parallel %v", segs[1]) }
	if len(segs[1].steps)!=2 { t.Error("2 steps") }
}

func TestRunErrorCode(t *testing.T) {
	if ErrorCode(ErrCancelled)!="cancelled" { t.Error("cancelled") }
	if ErrorCode(&RunError{Code:"contract_output", Err: &fakeErr{}})!="contract_output" { t.Error("contract") }
}

func TestNewRunID(t *testing.T) {
	if _, err := NewRunID("тест"); err!=nil {t.Fatal(err)}
	if Sanitize("привет мир")=="" {t.Error("sanitize")}
	if Sanitize("   ")!="run" {t.Error("empty sanitize")}
}

func TestValidateRun(t *testing.T) {
	p := &pipeline.PipelineFile{FormatVersion:"0.2", Pipeline: pipeline.Pipeline{Name:"x", Steps: []pipeline.Step{{ID:"a"}, {ID:"a"}}}}
	if err:=validateRun(p, &fakeEngine2{}); err==nil {t.Fatal("should fail: no bind")}
}

type fakeEngine2 struct{}
func (f *fakeEngine2) LoadManifest(ref string)(*pipeline.Manifest,error){return &pipeline.Manifest{ID:ref, Input: map[string]pipeline.Port{"x": {Type:"string"}}, Output: map[string]pipeline.Port{}},nil}

func TestRuntimeDataIssue(t *testing.T){
	if !runtimeDataIssue(pipeline.E_PORT_UNBOUND) {t.Error("should true")}
	if runtimeDataIssue("unknown") {t.Error("should false")}
}
