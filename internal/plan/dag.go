package plan

import (
	"fmt"
	"strings"
)

func ValidateTaskDAG(names []string, blockedBy map[string][]string) ([]string, error) {
	return validateDAG(names, blockedBy)
}

func ValidatePhaseDAG(phaseNames []string, dependsOn map[string][]string) ([]string, error) {
	return validateDAG(phaseNames, dependsOn)
}

// validateDAG uses Kahn's algorithm for topological sort.
// On cycle detection, uses DFS to find and report the cycle path.
func validateDAG(nodeNames []string, edges map[string][]string) ([]string, error) {
	if len(nodeNames) == 0 {
		return nil, nil
	}

	nodeSet := make(map[string]bool, len(nodeNames))
	for _, n := range nodeNames {
		nodeSet[n] = true
	}

	// Build in-degree map and forward adjacency (dependency → dependent)
	inDegree := make(map[string]int, len(nodeNames))
	forward := make(map[string][]string)
	for _, n := range nodeNames {
		inDegree[n] = 0
	}

	for node, deps := range edges {
		for _, dep := range deps {
			if !nodeSet[dep] {
				continue // skip unknown refs (caught by other validation)
			}
			inDegree[node]++
			forward[dep] = append(forward[dep], node)
		}
	}

	// Kahn's algorithm
	var queue []string
	for _, n := range nodeNames {
		if inDegree[n] == 0 {
			queue = append(queue, n)
		}
	}

	var sorted []string
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		sorted = append(sorted, node)

		for _, dependent := range forward[node] {
			inDegree[dependent]--
			if inDegree[dependent] == 0 {
				queue = append(queue, dependent)
			}
		}
	}

	if len(sorted) == len(nodeNames) {
		return sorted, nil
	}

	// Cycle detected — find cycle path via DFS
	cyclePath := findCyclePath(nodeNames, edges, inDegree)
	return nil, fmt.Errorf("circular dependency detected: %s", strings.Join(cyclePath, " -> "))
}

// findCyclePath finds a cycle path among nodes with non-zero in-degree.
func findCyclePath(nodeNames []string, edges map[string][]string, inDegree map[string]int) []string {
	const (
		white = 0 // unvisited
		gray  = 1 // in current path
		black = 2 // finished
	)

	color := make(map[string]int)
	parent := make(map[string]string)

	var cyclePath []string

	var dfs func(node string) bool
	dfs = func(node string) bool {
		color[node] = gray
		for _, dep := range edges[node] {
			if color[dep] == gray {
				// Found cycle: reconstruct path
				cyclePath = []string{dep}
				current := node
				for current != dep {
					cyclePath = append(cyclePath, current)
					current = parent[current]
				}
				cyclePath = append(cyclePath, dep)
				// Reverse to get forward order
				for i, j := 0, len(cyclePath)-1; i < j; i, j = i+1, j-1 {
					cyclePath[i], cyclePath[j] = cyclePath[j], cyclePath[i]
				}
				return true
			}
			if color[dep] == white {
				parent[dep] = node
				if dfs(dep) {
					return true
				}
			}
		}
		color[node] = black
		return false
	}

	// Start DFS from nodes still in the cycle (non-zero in-degree)
	for _, n := range nodeNames {
		if inDegree[n] > 0 && color[n] == white {
			if dfs(n) {
				return cyclePath
			}
		}
	}

	return []string{"(cycle detected)"}
}

func ValidateNoSelfReference(blockedBy map[string][]string) *ValidationErrors {
	errs := &ValidationErrors{}
	for name, deps := range blockedBy {
		for _, dep := range deps {
			if dep == name {
				errs.Add(fmt.Sprintf("blocked_by[%s]", name), "self-reference is not allowed")
			}
		}
	}
	if errs.HasErrors() {
		return errs
	}
	return nil
}

func ValidateSamePhaseRefs(blockedBy map[string][]string, validNames map[string]bool) *ValidationErrors {
	errs := &ValidationErrors{}
	for name, deps := range blockedBy {
		for i, dep := range deps {
			if !validNames[dep] {
				errs.Add(fmt.Sprintf("%s.blocked_by[%d]", name, i),
					fmt.Sprintf("references unknown name %q", dep))
			}
		}
	}
	if errs.HasErrors() {
		return errs
	}
	return nil
}
