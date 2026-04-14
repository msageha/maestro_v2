package daemon

import (
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// errNoUpdate is a sentinel error returned by updateYAMLFile's fn to indicate
// that no write is needed (e.g., idempotent check passed).
var errNoUpdate = yamlutil.ErrNoUpdate

// updateYAMLFile reads a YAML file, applies a mutation function, and writes it back atomically.
// If the file does not exist, fn receives a zero-value T.
// Locking is the caller's responsibility.
//
// If fn returns errNoUpdate, the file is not written and nil is returned.
// If fn returns any other error, it is propagated to the caller.
func updateYAMLFile[T any](path string, fn func(*T) error) error {
	return yamlutil.ReadModifyWrite(path, fn)
}
