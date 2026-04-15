package plan

import (
	"fmt"
	"strings"

	"github.com/msageha/maestro_v2/internal/model"
)

// ValidateTaskDAG validates that task dependencies form a directed acyclic graph
// and returns a topological ordering of task names.
func ValidateTaskDAG(names []string, blockedBy map[string][]string) ([]string, error) {
	return validateDAG(names, blockedBy)
}

// ValidatePhaseDAG validates that phase dependencies form a directed acyclic graph
// and returns a topological ordering of phase names.
func ValidatePhaseDAG(phaseNames []string, dependsOn map[string][]string) ([]string, error) {
	return validateDAG(phaseNames, dependsOn)
}

// validateDAG uses Kahn's algorithm for topological sort.
// On cycle detection, uses DFS to find and report the cycle path.
func validateDAG(nodeNames []string, edges map[string][]string) ([]string, error) {
	if len(nodeNames) == 0 {
		return []string{}, nil
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

// ValidateNoSelfReference checks that no task lists itself in its own blocked_by dependencies.
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

// ValidateSamePhaseRefs checks that all blocked_by references point to names within the given valid name set.
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

// ValidateTaskDAGAfterMutation validates the task DAG after a state mutation
// (e.g., rewriteDependencies during retry or cascade recovery). It checks
// for cycles in the dependency graph and cross-phase dependency violations.
func ValidateTaskDAGAfterMutation(state *model.CommandState) error {
	allNames := make([]string, 0, len(state.TaskStates))
	for k := range state.TaskStates {
		allNames = append(allNames, k)
	}
	if _, err := ValidateTaskDAG(allNames, state.TaskDependencies); err != nil {
		return fmt.Errorf("post-mutation DAG validation: %w", err)
	}
	if err := validateCrossPhaseRefs(state); err != nil {
		return fmt.Errorf("cross-phase dependency violation: %w", err)
	}
	return nil
}

// validateCrossPhaseRefs checks that no task depends on a task in a phase
// that the task's own phase does not (transitively) depend on.
// Tasks within the same phase or tasks not assigned to any phase are skipped.
func validateCrossPhaseRefs(state *model.CommandState) error {
	if len(state.Phases) <= 1 {
		return nil
	}

	// Build task→phaseIdx map.
	taskPhase := make(map[string]int, len(state.TaskStates))
	for i, phase := range state.Phases {
		for _, tid := range phase.TaskIDs {
			taskPhase[tid] = i
		}
	}

	// Build phase name/ID → index lookup.
	phaseIdx := make(map[string]int, len(state.Phases)*2)
	for i, p := range state.Phases {
		if p.Name != "" {
			phaseIdx[p.Name] = i
		}
		if p.PhaseID != "" {
			phaseIdx[p.PhaseID] = i
		}
	}

	// Compute transitive reachability: reachable[i] contains all phase
	// indices that phase i transitively depends on.
	reachable := make([]map[int]bool, len(state.Phases))
	for i, p := range state.Phases {
		reachable[i] = make(map[int]bool)
		for _, dep := range p.DependsOnPhases {
			if idx, ok := phaseIdx[dep]; ok {
				reachable[i][idx] = true
			}
		}
	}
	changed := true
	for changed {
		changed = false
		for i := range reachable {
			for j := range reachable[i] {
				for k := range reachable[j] {
					if !reachable[i][k] {
						reachable[i][k] = true
						changed = true
					}
				}
			}
		}
	}

	// Validate: each task dependency's phase must be the same phase or
	// a phase reachable (transitively depended on) from the task's phase.
	for taskID, deps := range state.TaskDependencies {
		tPI, tInPhase := taskPhase[taskID]
		if !tInPhase {
			continue
		}
		for _, dep := range deps {
			dPI, dInPhase := taskPhase[dep]
			if !dInPhase || dPI == tPI {
				continue
			}
			if !reachable[tPI][dPI] {
				return fmt.Errorf("task %s (phase %q) depends on task %s (phase %q) without phase dependency",
					taskID, state.Phases[tPI].Name, dep, state.Phases[dPI].Name)
			}
		}
	}
	return nil
}
