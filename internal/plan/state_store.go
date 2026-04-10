package plan

import "github.com/msageha/maestro_v2/internal/model"

// stateStore abstracts command state persistence operations, allowing
// the submit flow to be decoupled from direct filesystem I/O.
type stateStore interface {
	// LoadState reads and parses a command state file.
	LoadState(commandID string) (*model.CommandState, error)
	// SaveState atomically writes the command state to disk.
	SaveState(state *model.CommandState) error
	// DeleteState removes the state file for a command.
	DeleteState(commandID string) error
	// StateExists returns true if a state file exists for the given command.
	StateExists(commandID string) bool
}

