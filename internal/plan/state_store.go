package plan

import "github.com/msageha/maestro_v2/internal/model"

// StateStore abstracts command state persistence operations, allowing
// the submit flow to be decoupled from direct filesystem I/O.
type StateStore interface {
	// LoadState reads and parses a command state file.
	LoadState(commandID string) (*model.CommandState, error)
	// SaveState atomically writes the command state to disk.
	SaveState(state *model.CommandState) error
	// DeleteState removes the state file for a command.
	DeleteState(commandID string) error
	// StateExists returns true if a state file exists for the given command.
	StateExists(commandID string) bool
	// LockCommand acquires the in-process mutex for a command's state.
	LockCommand(commandID string)
	// UnlockCommand releases the in-process mutex for a command's state.
	UnlockCommand(commandID string)
}
