package search

import (
	"fmt"
	"math"
	"sync"
	"testing"
)

func TestNewTree(t *testing.T) {
	t.Parallel()
	tree := NewTree(5, 3, 0.2)
	if tree.NodeCount() != 0 {
		t.Fatalf("expected 0 nodes, got %d", tree.NodeCount())
	}
}

func TestAddRootAndExpand(t *testing.T) {
	t.Parallel()
	tree := NewTree(5, 3, 0.2)
	root := tree.AddRoot("root")
	if root.ID != "root" {
		t.Fatalf("expected root ID 'root', got %q", root.ID)
	}
	if root.Depth != 0 {
		t.Fatalf("expected root depth 0, got %d", root.Depth)
	}
	if root.State != NodeUnexpanded {
		t.Fatalf("expected root state unexpanded, got %q", root.State)
	}
	if tree.NodeCount() != 1 {
		t.Fatalf("expected 1 node, got %d", tree.NodeCount())
	}

	children, err := tree.Expand("root", []string{"c1", "c2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(children) != 2 {
		t.Fatalf("expected 2 children, got %d", len(children))
	}
	if tree.NodeCount() != 3 {
		t.Fatalf("expected 3 nodes, got %d", tree.NodeCount())
	}

	// Verify parent state is expanded
	n, ok := tree.GetNode("root")
	if !ok {
		t.Fatal("root not found")
	}
	if n.State != NodeExpanded {
		t.Fatalf("expected root state expanded, got %q", n.State)
	}

	// Verify child properties
	for _, c := range children {
		if c.ParentID != "root" {
			t.Fatalf("expected parent 'root', got %q", c.ParentID)
		}
		if c.Depth != 1 {
			t.Fatalf("expected depth 1, got %d", c.Depth)
		}
	}
}

// TestAddRoot_DoesNotOverwriteExistingRoot is a regression test: AddRoot
// used to replace the root unconditionally, orphaning the existing subtree.
func TestAddRoot_DoesNotOverwriteExistingRoot(t *testing.T) {
	t.Parallel()
	tree := NewTree(5, 10, 0.2)

	first := tree.AddRoot("root-1")
	if first == nil || first.ID != "root-1" {
		t.Fatalf("expected first AddRoot to create root-1, got %+v", first)
	}
	if _, err := tree.Expand("root-1", []string{"child-1"}); err != nil {
		t.Fatalf("Expand: %v", err)
	}

	// Idempotent re-add of the same root returns the existing node.
	same := tree.AddRoot("root-1")
	if same.ID != "root-1" || len(same.Children) != 1 {
		t.Fatalf("re-adding same root must return existing node with children, got %+v", same)
	}

	// A different id must not replace the root.
	other := tree.AddRoot("root-2")
	if other.ID != "root-1" {
		t.Fatalf("AddRoot with different id must keep existing root, got %s", other.ID)
	}
	if _, exists := tree.GetNode("root-2"); exists {
		t.Fatal("root-2 must not be inserted when a root already exists")
	}
	if best, err := tree.SelectBest("root-1", 1.41); err != nil || best != "child-1" {
		t.Fatalf("existing subtree must remain intact, got best=%q err=%v", best, err)
	}
}

func TestBackpropagate(t *testing.T) {
	t.Parallel()
	tree := NewTree(5, 4, 0.0)
	tree.AddRoot("root")
	_, err := tree.Expand("root", []string{"c1", "c2"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = tree.Expand("c1", []string{"gc1"})
	if err != nil {
		t.Fatal(err)
	}

	// Backpropagate from grandchild
	tree.Backpropagate("gc1", 0.8)

	// gc1 should have visit=1, reward=0.8
	gc1, _ := tree.GetNode("gc1")
	if gc1.Visits != 1 || gc1.AvgReward != 0.8 {
		t.Fatalf("gc1: visits=%d avgReward=%f, want 1 and 0.8", gc1.Visits, gc1.AvgReward)
	}

	// c1 should also have visit=1, reward=0.8
	c1, _ := tree.GetNode("c1")
	if c1.Visits != 1 || c1.AvgReward != 0.8 {
		t.Fatalf("c1: visits=%d avgReward=%f, want 1 and 0.8", c1.Visits, c1.AvgReward)
	}

	// root should have visit=1, reward=0.8
	root, _ := tree.GetNode("root")
	if root.Visits != 1 || root.AvgReward != 0.8 {
		t.Fatalf("root: visits=%d avgReward=%f, want 1 and 0.8", root.Visits, root.AvgReward)
	}

	// Second backpropagate from c2
	tree.Backpropagate("c2", 0.4)

	root, _ = tree.GetNode("root")
	if root.Visits != 2 {
		t.Fatalf("root visits: got %d, want 2", root.Visits)
	}
	expectedAvg := (0.8 + 0.4) / 2.0
	if math.Abs(root.AvgReward-expectedAvg) > 1e-9 {
		t.Fatalf("root avgReward: got %f, want %f", root.AvgReward, expectedAvg)
	}
}

func TestUCT_UnvisitedInf(t *testing.T) {
	t.Parallel()
	tree := NewTree(5, 4, 0.0)
	tree.AddRoot("root")
	tree.Expand("root", []string{"c1", "c2"})

	// Give root some visits via backprop through c1
	tree.Backpropagate("c1", 0.5)

	// c2 is unvisited → should return +Inf
	score := tree.UCT("c2", 1.0)
	if !math.IsInf(score, 1) {
		t.Fatalf("expected +Inf for unvisited node, got %f", score)
	}

	// c1 is visited → should return finite value
	score = tree.UCT("c1", 1.0)
	if math.IsInf(score, 1) || math.IsNaN(score) {
		t.Fatalf("expected finite value for visited node, got %f", score)
	}
}

func TestSelectBest(t *testing.T) {
	t.Parallel()
	tree := NewTree(5, 4, 0.0)
	tree.AddRoot("root")
	tree.Expand("root", []string{"c1", "c2", "c3"})

	// Give different rewards
	tree.Backpropagate("c1", 0.9)
	tree.Backpropagate("c2", 0.1)
	tree.Backpropagate("c3", 0.5)

	// With explorationCoeff=0, pure exploitation: should pick c1 (highest avg)
	best, err := tree.SelectBest("root", 0.0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if best != "c1" {
		t.Fatalf("expected c1 as best, got %q", best)
	}
}

func TestSelectBest_PrefersUnvisited(t *testing.T) {
	t.Parallel()
	tree := NewTree(5, 4, 0.0)
	tree.AddRoot("root")
	tree.Expand("root", []string{"c1", "c2"})

	// Visit only c1
	tree.Backpropagate("c1", 0.9)

	// With exploration, unvisited c2 should be preferred (UCT = +Inf)
	best, err := tree.SelectBest("root", 1.414)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if best != "c2" {
		t.Fatalf("expected c2 (unvisited) as best, got %q", best)
	}
}

func TestPrune(t *testing.T) {
	t.Parallel()
	tree := NewTree(5, 4, 0.5)
	tree.AddRoot("root")
	tree.Expand("root", []string{"c1", "c2"})
	tree.Expand("c1", []string{"gc1", "gc2"})

	// Give c1 and its children low rewards
	tree.Backpropagate("gc1", 0.1)
	tree.Backpropagate("gc2", 0.2)

	// Give c2 a high reward
	tree.Backpropagate("c2", 0.9)

	// Prune c1 (avgReward = (0.1+0.2)/2 = 0.15, below threshold 0.5)
	tree.Prune("c1")

	c1, _ := tree.GetNode("c1")
	if c1.State != NodePruned {
		t.Fatalf("expected c1 to be pruned, got %q", c1.State)
	}

	// Children should also be pruned recursively
	gc1, _ := tree.GetNode("gc1")
	if gc1.State != NodePruned {
		t.Fatalf("expected gc1 to be pruned, got %q", gc1.State)
	}
	gc2, _ := tree.GetNode("gc2")
	if gc2.State != NodePruned {
		t.Fatalf("expected gc2 to be pruned, got %q", gc2.State)
	}

	// c2 should not be pruned
	c2, _ := tree.GetNode("c2")
	if c2.State == NodePruned {
		t.Fatal("c2 should not be pruned")
	}
}

func TestPruneBelow(t *testing.T) {
	t.Parallel()
	tree := NewTree(5, 4, 0.0)
	tree.AddRoot("root")
	tree.Expand("root", []string{"c1", "c2", "c3"})

	tree.Backpropagate("c1", 0.1)
	tree.Backpropagate("c2", 0.8)
	tree.Backpropagate("c3", 0.3)

	tree.PruneBelow(0.5)

	c1, _ := tree.GetNode("c1")
	if c1.State != NodePruned {
		t.Fatalf("expected c1 pruned, got %q", c1.State)
	}
	c2, _ := tree.GetNode("c2")
	if c2.State == NodePruned {
		t.Fatal("c2 should not be pruned")
	}
	c3, _ := tree.GetNode("c3")
	if c3.State != NodePruned {
		t.Fatalf("expected c3 pruned, got %q", c3.State)
	}
}

func TestExpand_MaxBranchingExceeded(t *testing.T) {
	t.Parallel()
	tree := NewTree(5, 2, 0.0)
	tree.AddRoot("root")

	_, err := tree.Expand("root", []string{"c1", "c2", "c3"})
	if err == nil {
		t.Fatal("expected error for exceeding maxBranching")
	}
}

func TestExpand_MaxDepthExceeded(t *testing.T) {
	t.Parallel()
	tree := NewTree(1, 4, 0.0) // maxDepth=1
	tree.AddRoot("root")
	tree.Expand("root", []string{"c1"})

	// depth=1 children trying to expand → depth=2 > maxDepth=1
	_, err := tree.Expand("c1", []string{"gc1"})
	if err == nil {
		t.Fatal("expected error for exceeding maxDepth")
	}
}

func TestExpand_ParentNotFound(t *testing.T) {
	t.Parallel()
	tree := NewTree(5, 4, 0.0)
	_, err := tree.Expand("nonexistent", []string{"c1"})
	if err == nil {
		t.Fatal("expected error for nonexistent parent")
	}
}

func TestLeaves(t *testing.T) {
	t.Parallel()
	tree := NewTree(5, 4, 0.0)
	tree.AddRoot("root")
	tree.Expand("root", []string{"c1", "c2"})
	tree.Expand("c1", []string{"gc1"})

	leaves := tree.Leaves()
	leafIDs := make(map[string]bool)
	for _, l := range leaves {
		leafIDs[l.ID] = true
	}

	if !leafIDs["c2"] {
		t.Fatal("c2 should be a leaf")
	}
	if !leafIDs["gc1"] {
		t.Fatal("gc1 should be a leaf")
	}
	if leafIDs["c1"] {
		t.Fatal("c1 should not be a leaf (has children)")
	}
	if leafIDs["root"] {
		t.Fatal("root should not be a leaf (has children)")
	}
}

func TestSelectBest_NoChildren(t *testing.T) {
	t.Parallel()
	tree := NewTree(5, 4, 0.0)
	tree.AddRoot("root")

	_, err := tree.SelectBest("root", 1.0)
	if err == nil {
		t.Fatal("expected error when selecting from node with no children")
	}
}

func TestConcurrentSafety(t *testing.T) {
	t.Parallel()
	tree := NewTree(10, 100, 0.0)
	tree.AddRoot("root")

	// Pre-expand children
	childIDs := make([]string, 50)
	for i := range childIDs {
		childIDs[i] = fmt.Sprintf("c%d", i)
	}
	_, err := tree.Expand("root", childIDs)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup

	// Concurrent backpropagations
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			tree.Backpropagate(fmt.Sprintf("c%d", idx), 0.5)
		}(i)
	}

	// Concurrent reads
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			tree.UCT(fmt.Sprintf("c%d", idx), 1.0)
			tree.GetNode(fmt.Sprintf("c%d", idx))
			tree.NodeCount()
			tree.Leaves()
		}(i)
	}

	// Concurrent SelectBest
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tree.SelectBest("root", 1.414)
		}()
	}

	wg.Wait()

	// All children should have been visited
	root, _ := tree.GetNode("root")
	if root.Visits != 50 {
		t.Fatalf("expected root visits=50, got %d", root.Visits)
	}
}

func TestPruneBelow_HighScoreParentNotPruned(t *testing.T) {
	t.Parallel()
	tree := NewTree(5, 4, 0.0)
	tree.AddRoot("root")
	tree.Expand("root", []string{"parent"})
	tree.Expand("parent", []string{"low_child", "high_child"})

	tree.Backpropagate("low_child", 0.1)
	tree.Backpropagate("high_child", 0.9)

	tree.PruneBelow(0.5)

	lc, _ := tree.GetNode("low_child")
	if lc.State != NodePruned {
		t.Fatalf("expected low_child pruned, got %q", lc.State)
	}
	hc, _ := tree.GetNode("high_child")
	if hc.State == NodePruned {
		t.Fatal("high_child should not be pruned")
	}
	p, _ := tree.GetNode("parent")
	if p.State == NodePruned {
		t.Fatal("parent with non-pruned children should not be pruned")
	}
}

func TestPruneBelow_ParentPrunedWhenAllChildrenPruned(t *testing.T) {
	t.Parallel()
	tree := NewTree(5, 4, 0.0)
	tree.AddRoot("root")
	tree.Expand("root", []string{"parent"})
	tree.Expand("parent", []string{"low1", "low2"})

	tree.Backpropagate("low1", 0.1)
	tree.Backpropagate("low2", 0.2)

	tree.PruneBelow(0.5)

	low1, _ := tree.GetNode("low1")
	if low1.State != NodePruned {
		t.Fatalf("expected low1 pruned, got %q", low1.State)
	}
	low2, _ := tree.GetNode("low2")
	if low2.State != NodePruned {
		t.Fatalf("expected low2 pruned, got %q", low2.State)
	}
	// Parent should be pruned via structural propagation (all children pruned)
	p, _ := tree.GetNode("parent")
	if p.State != NodePruned {
		t.Fatalf("parent with all children pruned should be pruned, got %q", p.State)
	}
}

func TestBackpropagate_MissingNodeContinuesPropagation(t *testing.T) {
	t.Parallel()
	tree := NewTree(5, 4, 0.0)
	tree.AddRoot("root")
	tree.Expand("root", []string{"A"})
	tree.Expand("A", []string{"B"})
	tree.Expand("B", []string{"C"})

	// Remove B from the nodes map to simulate a missing intermediate node.
	// B's parent (A) still has "B" in its Children list.
	tree.mu.Lock()
	delete(tree.nodes, "B")
	tree.mu.Unlock()

	// Backpropagate from C: C → B (missing, skip) → A → root
	tree.Backpropagate("C", 1.0)

	// C should be updated
	c, _ := tree.GetNode("C")
	if c.Visits != 1 || c.AvgReward != 1.0 {
		t.Fatalf("C: visits=%d avgReward=%f, want 1 and 1.0", c.Visits, c.AvgReward)
	}
	// A should be updated (propagation continued past missing B)
	a, _ := tree.GetNode("A")
	if a.Visits != 1 || a.AvgReward != 1.0 {
		t.Fatalf("A: visits=%d avgReward=%f, want 1 and 1.0", a.Visits, a.AvgReward)
	}
	// root should be updated
	root, _ := tree.GetNode("root")
	if root.Visits != 1 || root.AvgReward != 1.0 {
		t.Fatalf("root: visits=%d avgReward=%f, want 1 and 1.0", root.Visits, root.AvgReward)
	}
}

func TestBackpropagate_StartNodeMissing(t *testing.T) {
	t.Parallel()
	tree := NewTree(5, 4, 0.0)
	tree.AddRoot("root")
	tree.Expand("root", []string{"child"})

	// Backpropagate from a nonexistent node that is a child of "root"
	// Since "nonexistent" is not in any node's Children list,
	// reverse lookup returns "" and propagation stops gracefully.
	tree.Backpropagate("nonexistent", 1.0)

	root, _ := tree.GetNode("root")
	if root.Visits != 0 {
		t.Fatalf("root visits: got %d, want 0 (nonexistent start node)", root.Visits)
	}
}
