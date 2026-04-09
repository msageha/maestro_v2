package scheduler

import (
	"path"
	"sort"
	"strings"
)

// TaskNode represents a task in the conflict graph.
type TaskNode struct {
	ID            string
	ExpectedPaths []string
	BloomLevel    int
	Dependencies  []string // blocked_by task ID list
}

// ConflictGraph models pairwise conflicts between tasks as an undirected graph
// and supports greedy graph coloring to identify parallel execution groups.
type ConflictGraph struct {
	nodes  map[string]*TaskNode
	edges  map[string]map[string]bool // undirected: conflict relationship
	colors map[string]int             // coloring result
}

// NewConflictGraph creates an empty ConflictGraph.
func NewConflictGraph() *ConflictGraph {
	return &ConflictGraph{
		nodes:  make(map[string]*TaskNode),
		edges:  make(map[string]map[string]bool),
		colors: make(map[string]int),
	}
}

// AddNode registers a task node in the graph.
func (g *ConflictGraph) AddNode(node TaskNode) {
	g.nodes[node.ID] = &node
	if g.edges[node.ID] == nil {
		g.edges[node.ID] = make(map[string]bool)
	}
}

// BuildEdges examines all node pairs for path overlap and dependency relationships,
// adding undirected edges for each conflict found.
func (g *ConflictGraph) BuildEdges() {
	ids := make([]string, 0, len(g.nodes))
	for id := range g.nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for i := 0; i < len(ids); i++ {
		for j := i + 1; j < len(ids); j++ {
			a := g.nodes[ids[i]]
			b := g.nodes[ids[j]]

			conflict := hasPathOverlap(a.ExpectedPaths, b.ExpectedPaths)

			if !conflict {
				conflict = hasDependency(a, b)
			}

			if conflict {
				g.addEdge(ids[i], ids[j])
			}
		}
	}
}

// ColorGraph assigns colors using a greedy algorithm (largest-degree-first ordering).
// Returns a map from node ID to color number (0-based).
func (g *ConflictGraph) ColorGraph() map[string]int {
	ids := make([]string, 0, len(g.nodes))
	for id := range g.nodes {
		ids = append(ids, id)
	}

	// Sort by degree descending, then by ID for determinism.
	sort.Slice(ids, func(i, j int) bool {
		di := len(g.edges[ids[i]])
		dj := len(g.edges[ids[j]])
		if di != dj {
			return di > dj
		}
		return ids[i] < ids[j]
	})

	g.colors = make(map[string]int, len(ids))

	for _, id := range ids {
		used := make(map[int]bool)
		for neighbor := range g.edges[id] {
			if c, ok := g.colors[neighbor]; ok {
				used[c] = true
			}
		}
		// Assign smallest available color.
		color := 0
		for used[color] {
			color++
		}
		g.colors[id] = color
	}

	return g.colors
}

// GetParallelGroups returns groups of node IDs that share the same color.
// Each group can be executed in parallel. Groups are sorted by color number,
// and IDs within each group are sorted lexicographically.
func (g *ConflictGraph) GetParallelGroups() [][]string {
	if len(g.colors) == 0 {
		return nil
	}

	groupMap := make(map[int][]string)
	maxColor := 0
	for id, c := range g.colors {
		groupMap[c] = append(groupMap[c], id)
		if c > maxColor {
			maxColor = c
		}
	}

	groups := make([][]string, 0, len(groupMap))
	for c := 0; c <= maxColor; c++ {
		if ids, ok := groupMap[c]; ok {
			sort.Strings(ids)
			groups = append(groups, ids)
		}
	}
	return groups
}

// NodeCount returns the number of nodes in the graph.
func (g *ConflictGraph) NodeCount() int {
	return len(g.nodes)
}

// EdgeCount returns the number of undirected edges in the graph.
func (g *ConflictGraph) EdgeCount() int {
	count := 0
	for _, neighbors := range g.edges {
		count += len(neighbors)
	}
	return count / 2
}

func (g *ConflictGraph) addEdge(a, b string) {
	if g.edges[a] == nil {
		g.edges[a] = make(map[string]bool)
	}
	if g.edges[b] == nil {
		g.edges[b] = make(map[string]bool)
	}
	g.edges[a][b] = true
	g.edges[b][a] = true
}

// hasDependency reports whether a depends on b or b depends on a.
func hasDependency(a, b *TaskNode) bool {
	for _, dep := range a.Dependencies {
		if dep == b.ID {
			return true
		}
	}
	for _, dep := range b.Dependencies {
		if dep == a.ID {
			return true
		}
	}
	return false
}

// hasPathOverlap reports whether any path in pathsA overlaps with any path in pathsB.
// Overlap means one path equals or contains the other at a directory boundary.
// This is a package-local reimplementation to avoid import cycles with the daemon package.
func hasPathOverlap(pathsA, pathsB []string) bool {
	if len(pathsA) == 0 || len(pathsB) == 0 {
		return false
	}
	for _, a := range pathsA {
		for _, b := range pathsB {
			if pathOverlap(a, b) {
				return true
			}
		}
	}
	return false
}

// pathOverlap reports whether two individual paths overlap at a directory boundary.
func pathOverlap(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	ca := normalizePath(a)
	cb := normalizePath(b)
	if ca == "" || cb == "" {
		return false
	}
	if ca == cb {
		return true
	}
	return isDescendant(cb, ca) || isDescendant(ca, cb)
}

// normalizePath strips trailing slashes and cleans the path.
func normalizePath(p string) string {
	cleaned := path.Clean(strings.TrimSuffix(p, "/"))
	if cleaned == "." {
		return ""
	}
	return cleaned
}

// isDescendant reports whether child is a descendant of dir at a "/" boundary.
func isDescendant(child, dir string) bool {
	return strings.HasPrefix(child, dir+"/")
}
