package yaml

import (
	"errors"
	"fmt"
	"os"

	yamlv3 "gopkg.in/yaml.v3"
)

// ErrNoUpdate is a sentinel error returned by ReadModifyWrite's fn to indicate
// that no write is needed (e.g., idempotent check passed).
var ErrNoUpdate = errors.New("no file update needed")

// ReadModifyWrite reads a YAML file, applies a mutation function, and writes it back atomically.
// If the file does not exist, fn receives a zero-value T.
// Locking is the caller's responsibility.
//
// If fn returns ErrNoUpdate, the file is not written and nil is returned.
// If fn returns any other error, it is propagated to the caller.
func ReadModifyWrite[T any](path string, fn func(*T) error) error {
	var v T
	data, err := os.ReadFile(path) //nolint:gosec // path is constructed from a controlled application directory
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) > 0 {
		if err := yamlv3.Unmarshal(data, &v); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
	}
	if err := fn(&v); err != nil {
		if errors.Is(err, ErrNoUpdate) {
			return nil
		}
		return err
	}
	return AtomicWrite(path, &v)
}
