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
