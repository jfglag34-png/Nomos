package execution

import (
	"testing"

	"github.com/wykserdex/wedra/internal/pipeline"
)

func TestBuildGraphEmpty(t *testing.T) {
	g := BuildGraph()
	if len(g.Edges) != 0 {
		t.Error("empty edges")
	}
}

func TestBuildGraphFromPipeline(t *testing.T) {
	pf := &pipeline.PipelineFile{
		Pipeline: pipeline.Pipeline{
			Steps: []pipeline.Step{
				{ID: "a"},
				{ID: "b", Bind: map[string]string{"x": "steps.a.out"}},
				{ID: "c", Form: []pipeline.FormField{{Field: "steps.b.out"}}},
			},
		},
	}
	g, err := BuildGraphFromPipeline(pf)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Nodes) != 3 {
		t.Fatalf("nodes %v", g.Nodes)
	}
	// a -> b, b -> c
	if len(g.Edges["a"]) != 1 || g.Edges["a"][0] != "b" {
		t.Fatalf("a edges %v", g.Edges["a"])
	}
	if len(g.Edges["b"]) != 1 || g.Edges["b"][0] != "c" {
		t.Fatalf("b edges %v", g.Edges["b"])
	}
}

func TestBuildGraphDedupAndSelf(t *testing.T) {
	pf := &pipeline.PipelineFile{
		Pipeline: pipeline.Pipeline{
			Steps: []pipeline.Step{
				{ID: "a", Bind: map[string]string{"x": "steps.a.out"}}, // self should be ignored
				{ID: "a"}, // duplicate id
			},
		},
	}
	g, _ := BuildGraphFromPipeline(pf)
	if len(g.Nodes) != 1 {
		t.Fatalf("dedup nodes %v", g.Nodes)
	}
	if len(g.Edges["a"]) != 0 {
		t.Fatalf("self edge should be ignored %v", g.Edges["a"])
	}
}

func TestBuildGraphAllSuffix(t *testing.T) {
	pf := &pipeline.PipelineFile{
		Pipeline: pipeline.Pipeline{
			Steps: []pipeline.Step{
				{ID: "a"},
				{ID: "b", Bind: map[string]string{"x": "steps.a_all"}},
			},
		},
	}
	g, _ := BuildGraphFromPipeline(pf)
	if len(g.Edges["a"]) != 1 || g.Edges["a"][0] != "b" {
		t.Fatalf("all suffix %v", g.Edges["a"])
	}
}

func TestTopoSort(t *testing.T) {
	g := &ExecutionGraph{
		Nodes: []string{"a", "b", "c"},
		Edges: map[string][]string{"a": {"b"}, "b": {"c"}},
	}
	order, err := g.TopoSort()
	if err != nil {
		t.Fatal(err)
	}
	if order[0] != "a" || order[2] != "c" {
		t.Fatalf("order %v", order)
	}
}

func TestTopoSortCycle(t *testing.T) {
	g := &ExecutionGraph{
		Nodes: []string{"a", "b"},
		Edges: map[string][]string{"a": {"b"}, "b": {"a"}},
	}
	if _, err := g.TopoSort(); err == nil {
		t.Fatal("expected cycle error")
	}
}

func TestTopoSortIsolated(t *testing.T) {
	g := &ExecutionGraph{
		Nodes: []string{"a", "b"},
		Edges: map[string][]string{"a": {}, "b": {}},
	}
	order, err := g.TopoSort()
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 {
		t.Fatalf("order %v", order)
	}
}

func TestIndependentBatches(t *testing.T) {
	g := &ExecutionGraph{
		Nodes: []string{"a", "b", "c", "d"},
		Edges: map[string][]string{
			"a": {"c"},
			"b": {"c"},
			"c": {"d"},
			"d": {},
		},
	}
	batches, err := g.IndependentBatches()
	if err != nil {
		t.Fatal(err)
	}
	// a,b level0, c level1, d level2
	if len(batches) != 3 {
		t.Fatalf("batches %v", batches)
	}
	// batch 0 should contain a,b (order may vary but both)
	found := map[string]bool{}
	for _, n := range batches[0] {
		found[n] = true
	}
	if !found["a"] || !found["b"] {
		t.Fatalf("batch0 %v", batches[0])
	}
}

func TestIndependentBatchesCycle(t *testing.T) {
	g := &ExecutionGraph{
		Nodes: []string{"a"},
		Edges: map[string][]string{"a": {"a"}},
	}
	if _, err := g.IndependentBatches(); err == nil {
		t.Fatal("expected cycle")
	}
}

func TestSegmentAndCloneCtx(t *testing.T) {
	steps := []*pipeline.Step{
		{ID: "a"},
		{ID: "b", ParallelGroup: "g1"},
		{ID: "c", ParallelGroup: "g1"},
		{ID: "d"},
	}
	segs := segmentSteps(steps)
	if len(segs) != 3 {
		t.Fatalf("segs %v", segs)
	}
	if segs[1].parallel != true || segs[1].group != "g1" {
		t.Fatalf("parallel seg %v", segs[1])
	}
}
