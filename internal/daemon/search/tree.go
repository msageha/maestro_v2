// Package search provides tree-based search utilities for the daemon.
package search

import (
	"errors"
	"fmt"
	"math"
	"sync"
)

// NodeState represents the state of a search tree node.
type NodeState string

const (
	// NodeUnexpanded is the initial state of a newly created search node.
	NodeUnexpanded NodeState = "unexpanded"
	// NodeExpanded indicates the node has been visited and children generated.
	NodeExpanded NodeState = "expanded"
	// NodeTerminal indicates the node represents a final outcome.
	NodeTerminal NodeState = "terminal"
	// NodePruned indicates the node was discarded during search.
	NodePruned NodeState = "pruned"
)

// Node represents a single node in the MCTS search tree.
type Node struct {
	ID          string
	ParentID    string
	Children    []string
	Visits      int
	TotalReward float64
	AvgReward   float64
	State       NodeState
	Depth       int
	WorktreeRef string
	Metadata    map[string]string
}

// Tree manages the MCTS search tree with thread-safe operations.
type Tree struct {
	nodes          map[string]*Node
	root           string
	maxDepth       int
	maxBranching   int
	pruneThreshold float64
	mu             sync.RWMutex
}

// NewTree creates a new MCTS tree with the given constraints.
func NewTree(maxDepth, maxBranching int, pruneThreshold float64) *Tree {
	return &Tree{
		nodes:          make(map[string]*Node),
		maxDepth:       maxDepth,
		maxBranching:   maxBranching,
		pruneThreshold: pruneThreshold,
	}
}

// AddRoot adds the root node to the tree. Returns error if root already exists.
func (t *Tree) AddRoot(id string) *Node {
	t.mu.Lock()
	defer t.mu.Unlock()

	n := &Node{
		ID:       id,
		State:    NodeUnexpanded,
		Depth:    0,
		Children: []string{},
		Metadata: make(map[string]string),
	}
	t.nodes[id] = n
	t.root = id
	return n
}

// Expand adds child nodes under the given parent. Returns error if constraints are violated.
func (t *Tree) Expand(parentID string, childIDs []string) ([]*Node, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	parent, ok := t.nodes[parentID]
	if !ok {
		return nil, fmt.Errorf("parent node %q not found", parentID)
	}

	if len(parent.Children)+len(childIDs) > t.maxBranching {
		return nil, fmt.Errorf("expanding %q would exceed maxBranching %d (current %d + new %d)",
			parentID, t.maxBranching, len(parent.Children), len(childIDs))
	}

	childDepth := parent.Depth + 1
	if childDepth > t.maxDepth {
		return nil, fmt.Errorf("expanding %q would exceed maxDepth %d (child depth %d)",
			parentID, t.maxDepth, childDepth)
	}

	children := make([]*Node, 0, len(childIDs))
	for _, cid := range childIDs {
		child := &Node{
			ID:       cid,
			ParentID: parentID,
			State:    NodeUnexpanded,
			Depth:    childDepth,
			Children: []string{},
			Metadata: make(map[string]string),
		}
		t.nodes[cid] = child
		children = append(children, child)
		parent.Children = append(parent.Children, cid)
	}
	parent.State = NodeExpanded
	return children, nil
}

// Backpropagate propagates a reward from the given node up to the root.
func (t *Tree) Backpropagate(nodeID string, reward float64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	current := nodeID
	for current != "" {
		n, ok := t.nodes[current]
		if !ok {
			break
		}
		n.Visits++
		n.TotalReward += reward
		n.AvgReward = n.TotalReward / float64(n.Visits)
		current = n.ParentID
	}
}

// UCT computes the Upper Confidence Bound for Trees score for a node.
// Returns +Inf for unvisited nodes (exploration priority).
func (t *Tree) UCT(nodeID string, explorationCoeff float64) float64 {
	t.mu.RLock()
	defer t.mu.RUnlock()

	return t.uctLocked(nodeID, explorationCoeff)
}

func (t *Tree) uctLocked(nodeID string, explorationCoeff float64) float64 {
	n, ok := t.nodes[nodeID]
	if !ok {
		return 0
	}
	if n.Visits == 0 {
		return math.Inf(1)
	}

	parent, ok := t.nodes[n.ParentID]
	if !ok {
		return n.AvgReward
	}

	parentVisits := float64(parent.Visits)
	if parentVisits <= 0 {
		return n.AvgReward
	}

	return n.AvgReward + explorationCoeff*math.Sqrt(math.Log(parentVisits)/float64(n.Visits))
}

// SelectBest returns the child of parentID with the highest UCT score.
func (t *Tree) SelectBest(parentID string, explorationCoeff float64) (string, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	parent, ok := t.nodes[parentID]
	if !ok {
		return "", fmt.Errorf("parent node %q not found", parentID)
	}
	if len(parent.Children) == 0 {
		return "", errors.New("no children to select from")
	}

	bestID := ""
	bestScore := math.Inf(-1)
	for _, cid := range parent.Children {
		child, ok := t.nodes[cid]
		if !ok || child.State == NodePruned {
			continue
		}
		score := t.uctLocked(cid, explorationCoeff)
		if score > bestScore {
			bestScore = score
			bestID = cid
		}
	}

	if bestID == "" {
		return "", errors.New("no selectable children (all pruned)")
	}
	return bestID, nil
}

// Prune marks nodes with AvgReward below pruneThreshold as pruned, recursively.
func (t *Tree) Prune(nodeID string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.pruneLocked(nodeID)
}

func (t *Tree) pruneLocked(nodeID string) {
	n, ok := t.nodes[nodeID]
	if !ok {
		return
	}
	if n.Visits > 0 && n.AvgReward < t.pruneThreshold {
		t.markPrunedLocked(n)
	}
}

func (t *Tree) markPrunedLocked(n *Node) {
	n.State = NodePruned
	for _, cid := range n.Children {
		if child, ok := t.nodes[cid]; ok {
			t.markPrunedLocked(child)
		}
	}
}

// PruneBelow marks all leaf-level nodes with AvgReward below the given threshold as pruned.
// Non-leaf nodes are only pruned if all their children are already pruned.
func (t *Tree) PruneBelow(threshold float64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// First pass: mark individual nodes below threshold (leaf nodes only)
	for _, n := range t.nodes {
		if n.Visits > 0 && n.AvgReward < threshold && n.State != NodePruned && len(n.Children) == 0 {
			n.State = NodePruned
		}
	}

	// Second pass: propagate — prune parents whose children are all pruned
	changed := true
	for changed {
		changed = false
		for _, n := range t.nodes {
			if n.State == NodePruned || len(n.Children) == 0 {
				continue
			}
			allPruned := true
			for _, cid := range n.Children {
				if child, ok := t.nodes[cid]; ok && child.State != NodePruned {
					allPruned = false
					break
				}
			}
			if allPruned && n.Visits > 0 && n.AvgReward < threshold {
				n.State = NodePruned
				changed = true
			}
		}
	}
}

// GetNode returns the node with the given ID, or false if not found.
func (t *Tree) GetNode(id string) (*Node, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	n, ok := t.nodes[id]
	return n, ok
}

// NodeCount returns the total number of nodes in the tree.
func (t *Tree) NodeCount() int {
	t.mu.RLock()
	defer t.mu.RUnlock()

	return len(t.nodes)
}

// Leaves returns all leaf nodes (nodes with no children that are not pruned).
func (t *Tree) Leaves() []*Node {
	t.mu.RLock()
	defer t.mu.RUnlock()

	var leaves []*Node
	for _, n := range t.nodes {
		if len(n.Children) == 0 && n.State != NodePruned {
			leaves = append(leaves, n)
		}
	}
	return leaves
}
