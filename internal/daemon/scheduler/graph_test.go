package scheduler

import (
	"fmt"
	"testing"
)

func TestIndependentTasks_AllSameColor(t *testing.T) {
	t.Parallel()
	g := NewConflictGraph()
	g.AddNode(TaskNode{ID: "a", ExpectedPaths: []string{"pkg/auth"}})
	g.AddNode(TaskNode{ID: "b", ExpectedPaths: []string{"pkg/billing"}})
	g.AddNode(TaskNode{ID: "c", ExpectedPaths: []string{"pkg/logging"}})
	g.BuildEdges()
	colors := g.ColorGraph()

	if g.EdgeCount() != 0 {
		t.Fatalf("expected 0 edges, got %d", g.EdgeCount())
	}
	// All should have same color (0).
	for _, id := range []string{"a", "b", "c"} {
		if colors[id] != 0 {
			t.Errorf("node %s: expected color 0, got %d", id, colors[id])
		}
	}

	groups := g.GetParallelGroups()
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	if len(groups[0]) != 3 {
		t.Errorf("expected 3 nodes in group, got %d", len(groups[0]))
	}
}

func TestPathOverlap_DifferentColors(t *testing.T) {
	t.Parallel()
	g := NewConflictGraph()
	g.AddNode(TaskNode{ID: "a", ExpectedPaths: []string{"internal/daemon"}})
	g.AddNode(TaskNode{ID: "b", ExpectedPaths: []string{"internal/daemon/scheduler"}})
	g.BuildEdges()
	colors := g.ColorGraph()

	if g.EdgeCount() != 1 {
		t.Fatalf("expected 1 edge, got %d", g.EdgeCount())
	}
	if colors["a"] == colors["b"] {
		t.Errorf("overlapping tasks should have different colors: a=%d b=%d", colors["a"], colors["b"])
	}

	groups := g.GetParallelGroups()
	if len(groups) != 2 {
		t.Errorf("expected 2 groups, got %d", len(groups))
	}
}

func TestCircularConflicts_ThreeOrMoreColors(t *testing.T) {
	t.Parallel()
	// A conflicts with B, B conflicts with C, C conflicts with A → needs 3 colors.
	g := NewConflictGraph()
	g.AddNode(TaskNode{ID: "a", ExpectedPaths: []string{"shared/x"}})
	g.AddNode(TaskNode{ID: "b", ExpectedPaths: []string{"shared/x", "shared/y"}})
	g.AddNode(TaskNode{ID: "c", ExpectedPaths: []string{"shared/y", "shared/z"}})
	// a-b overlap on shared/x, b-c overlap on shared/y.
	// Add explicit overlap for c-a via dependency to form a 3-clique.
	g.AddNode(TaskNode{ID: "a2", ExpectedPaths: nil}) // unused dummy to not conflict
	// Rebuild: use dependencies to create the triangle.
	g2 := NewConflictGraph()
	g2.AddNode(TaskNode{ID: "a", ExpectedPaths: []string{"shared/x"}, Dependencies: []string{"c"}})
	g2.AddNode(TaskNode{ID: "b", ExpectedPaths: []string{"shared/x", "shared/y"}})
	g2.AddNode(TaskNode{ID: "c", ExpectedPaths: []string{"shared/y"}})
	g2.BuildEdges()
	colors := g2.ColorGraph()

	// a-b conflict (path), b-c conflict (path), a-c conflict (dependency) → 3-clique.
	if g2.EdgeCount() != 3 {
		t.Fatalf("expected 3 edges for triangle, got %d", g2.EdgeCount())
	}
	colorSet := map[int]bool{}
	for _, c := range colors {
		colorSet[c] = true
	}
	if len(colorSet) < 3 {
		t.Errorf("3-clique should need at least 3 colors, got %d unique colors: %v", len(colorSet), colors)
	}
}

func TestDependency_DifferentColors(t *testing.T) {
	t.Parallel()
	g := NewConflictGraph()
	g.AddNode(TaskNode{ID: "a", ExpectedPaths: []string{"pkg/foo"}, Dependencies: []string{"b"}})
	g.AddNode(TaskNode{ID: "b", ExpectedPaths: []string{"pkg/bar"}})
	g.BuildEdges()
	colors := g.ColorGraph()

	if g.EdgeCount() != 1 {
		t.Fatalf("expected 1 edge from dependency, got %d", g.EdgeCount())
	}
	if colors["a"] == colors["b"] {
		t.Errorf("dependent tasks should have different colors: a=%d b=%d", colors["a"], colors["b"])
	}
}

func TestEmptyGraph(t *testing.T) {
	t.Parallel()
	g := NewConflictGraph()
	g.BuildEdges()
	colors := g.ColorGraph()

	if len(colors) != 0 {
		t.Errorf("expected empty colors, got %v", colors)
	}
	if g.NodeCount() != 0 {
		t.Errorf("expected 0 nodes, got %d", g.NodeCount())
	}
	if g.EdgeCount() != 0 {
		t.Errorf("expected 0 edges, got %d", g.EdgeCount())
	}
	groups := g.GetParallelGroups()
	if groups != nil {
		t.Errorf("expected nil groups, got %v", groups)
	}
}

func TestSingleNode(t *testing.T) {
	t.Parallel()
	g := NewConflictGraph()
	g.AddNode(TaskNode{ID: "only", ExpectedPaths: []string{"src/main.go"}})
	g.BuildEdges()
	colors := g.ColorGraph()

	if g.NodeCount() != 1 {
		t.Errorf("expected 1 node, got %d", g.NodeCount())
	}
	if colors["only"] != 0 {
		t.Errorf("single node should have color 0, got %d", colors["only"])
	}
	groups := g.GetParallelGroups()
	if len(groups) != 1 || len(groups[0]) != 1 || groups[0][0] != "only" {
		t.Errorf("expected [[only]], got %v", groups)
	}
}

func TestPathOverlapBoundary(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		pathsA  []string
		pathsB  []string
		overlap bool
	}{
		{
			name:    "exact match",
			pathsA:  []string{"internal/daemon"},
			pathsB:  []string{"internal/daemon"},
			overlap: true,
		},
		{
			name:    "trailing slash treated as same",
			pathsA:  []string{"internal/daemon"},
			pathsB:  []string{"internal/daemon/"},
			overlap: true,
		},
		{
			name:    "parent-child",
			pathsA:  []string{"internal/daemon"},
			pathsB:  []string{"internal/daemon/scheduler"},
			overlap: true,
		},
		{
			name:    "similar prefix not descendant",
			pathsA:  []string{"internal/daemon"},
			pathsB:  []string{"internal/daemonx"},
			overlap: false,
		},
		{
			name:    "sibling directories",
			pathsA:  []string{"internal/daemon"},
			pathsB:  []string{"internal/model"},
			overlap: false,
		},
		{
			name:    "empty pathsA",
			pathsA:  nil,
			pathsB:  []string{"internal/daemon"},
			overlap: false,
		},
		{
			name:    "empty pathsB",
			pathsA:  []string{"internal/daemon"},
			pathsB:  nil,
			overlap: false,
		},
		{
			name:    "both empty",
			pathsA:  nil,
			pathsB:  nil,
			overlap: false,
		},
		{
			name:    "empty string paths",
			pathsA:  []string{""},
			pathsB:  []string{"internal/daemon"},
			overlap: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := hasPathOverlap(tt.pathsA, tt.pathsB)
			if got != tt.overlap {
				t.Errorf("hasPathOverlap(%v, %v) = %v, want %v", tt.pathsA, tt.pathsB, got, tt.overlap)
			}
		})
	}
}

func TestLargeGraph_ColoringValid(t *testing.T) {
	t.Parallel()
	g := NewConflictGraph()
	// Create 20 nodes: nodes sharing the same "cluster" directory will conflict.
	for i := 0; i < 20; i++ {
		cluster := fmt.Sprintf("cluster%d", i%5) // 5 clusters of 4 nodes each
		g.AddNode(TaskNode{
			ID:            fmt.Sprintf("task%02d", i),
			ExpectedPaths: []string{fmt.Sprintf("src/%s/file%d.go", cluster, i)},
		})
	}
	g.BuildEdges()
	colors := g.ColorGraph()

	if g.NodeCount() != 20 {
		t.Fatalf("expected 20 nodes, got %d", g.NodeCount())
	}

	// Verify coloring validity: no two adjacent nodes share a color.
	for id, neighbors := range g.edges {
		for neighbor := range neighbors {
			if colors[id] == colors[neighbor] {
				t.Errorf("adjacent nodes %s and %s share color %d", id, neighbor, colors[id])
			}
		}
	}

	groups := g.GetParallelGroups()
	if len(groups) == 0 {
		t.Fatal("expected at least one group")
	}

	// Verify all nodes appear in groups.
	total := 0
	for _, group := range groups {
		total += len(group)
	}
	if total != 20 {
		t.Errorf("groups contain %d nodes, expected 20", total)
	}
}

func TestLargeGraph_WithPathOverlap(t *testing.T) {
	t.Parallel()
	// Nodes sharing a parent directory should conflict.
	g := NewConflictGraph()
	for i := 0; i < 20; i++ {
		cluster := fmt.Sprintf("src/cluster%d", i%5)
		g.AddNode(TaskNode{
			ID:            fmt.Sprintf("t%02d", i),
			ExpectedPaths: []string{cluster},
		})
	}
	g.BuildEdges()
	colors := g.ColorGraph()

	// Each cluster has 4 nodes all pointing to the same path → 4-clique per cluster.
	// The 5 clusters are independent → expect 4 colors.
	colorSet := map[int]bool{}
	for _, c := range colors {
		colorSet[c] = true
	}
	if len(colorSet) != 4 {
		t.Errorf("expected 4 colors for 5 independent 4-cliques, got %d", len(colorSet))
	}

	// Verify no adjacent pair shares a color.
	for id, neighbors := range g.edges {
		for neighbor := range neighbors {
			if colors[id] == colors[neighbor] {
				t.Errorf("adjacent nodes %s and %s share color %d", id, neighbor, colors[id])
			}
		}
	}
}

func TestGetParallelGroups_SortedOutput(t *testing.T) {
	t.Parallel()
	g := NewConflictGraph()
	g.AddNode(TaskNode{ID: "z", ExpectedPaths: []string{"shared"}})
	g.AddNode(TaskNode{ID: "a", ExpectedPaths: []string{"shared"}})
	g.AddNode(TaskNode{ID: "m", ExpectedPaths: []string{"other"}})
	g.BuildEdges()
	g.ColorGraph()
	groups := g.GetParallelGroups()

	// Verify IDs within each group are sorted.
	for _, group := range groups {
		for i := 1; i < len(group); i++ {
			if group[i-1] >= group[i] {
				t.Errorf("group not sorted: %v", group)
				break
			}
		}
	}
}
