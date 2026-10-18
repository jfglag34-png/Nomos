package execution

import (
	"fmt"
	"strings"

	"github.com/wykserdex/wedra/internal/pipeline"
)

type ExecutionGraph struct {
	Nodes []string
	Edges map[string][]string
}

func BuildGraph() *ExecutionGraph {
	return &ExecutionGraph{Edges: map[string][]string{}}
}

func BuildGraphFromPipeline(pf *pipeline.PipelineFile) (*ExecutionGraph, error) {
	g := &ExecutionGraph{
		Nodes: []string{},
		Edges: map[string][]string{},
	}
	seen := map[string]bool{}
	for _, st := range pf.Pipeline.Steps {
		if !seen[st.ID] {
			g.Nodes = append(g.Nodes, st.ID)
			seen[st.ID] = true
		}
		if _, ok := g.Edges[st.ID]; !ok {
			g.Edges[st.ID] = []string{}
		}
	}
	for _, st := range pf.Pipeline.Steps {
		deps := map[string]bool{}
		for _, v := range st.Bind {
			if strings.HasPrefix(v, "steps.") {
				parts := strings.Split(v, ".")
				if len(parts) >= 2 {
					dep := parts[1]
					dep = strings.TrimSuffix(dep, "_all")
					deps[dep] = true
				}
			}
		}
		for _, f := range st.Form {
			if strings.HasPrefix(f.Field, "steps.") {
				parts := strings.Split(f.Field, ".")
				if len(parts) >= 2 {
					dep := parts[1]
					dep = strings.TrimSuffix(dep, "_all")
					deps[dep] = true
				}
			}
		}
		for dep := range deps {
			if dep == st.ID {
				continue
			}
			g.Edges[dep] = append(g.Edges[dep], st.ID)
		}
	}
	return g, nil
}

func (g *ExecutionGraph) TopoSort() ([]string, error) {
	inDeg := map[string]int{}
	for _, n := range g.Nodes {
		if _, ok := inDeg[n]; !ok {
			inDeg[n] = 0
		}
	}
	for _, tos := range g.Edges {
		for _, to := range tos {
			inDeg[to]++
		}
	}
	queue := []string{}
	for _, n := range g.Nodes {
		if inDeg[n] == 0 {
			queue = append(queue, n)
		}
	}
	var order []string
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		order = append(order, cur)
		for _, nb := range g.Edges[cur] {
			inDeg[nb]--
			if inDeg[nb] == 0 {
				queue = append(queue, nb)
			}
		}
	}
	if len(order) != len(g.Nodes) {
		return nil, fmt.Errorf("цикл в графе выполнения")
	}
	return order, nil
}

func (g *ExecutionGraph) IndependentBatches() ([][]string, error) {
	order, err := g.TopoSort()
	if err != nil {
		return nil, err
	}
	level := map[string]int{}
	for _, n := range order {
		maxDep := -1
		for from, tos := range g.Edges {
			for _, to := range tos {
				if to == n {
					if l, ok := level[from]; ok && l > maxDep {
						maxDep = l
					}
				}
			}
		}
		level[n] = maxDep + 1
	}
	maxLevel := 0
	for _, l := range level {
		if l > maxLevel {
			maxLevel = l
		}
	}
	batches := make([][]string, maxLevel+1)
	for _, n := range g.Nodes {
		l := level[n]
		batches[l] = append(batches[l], n)
	}
	return batches, nil
}
