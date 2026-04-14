package model

// mutationStrategy defines the strategy used for evolutionary mutation.
type mutationStrategy string

const (
	// MutationStrategyDiff applies mutations as diffs between versions.
	MutationStrategyDiff mutationStrategy = "diff"
	// MutationStrategyFull replaces the entire implementation as a mutation.
	MutationStrategyFull mutationStrategy = "full"
	// MutationStrategyCross creates mutations by combining multiple parent versions.
	MutationStrategyCross mutationStrategy = "cross"
)

// validateMutationStrategy reports whether s is a known mutation strategy.
func validateMutationStrategy(s string) bool {
	switch mutationStrategy(s) {
	case MutationStrategyDiff, MutationStrategyFull, MutationStrategyCross:
		return true
	}
	return false
}
