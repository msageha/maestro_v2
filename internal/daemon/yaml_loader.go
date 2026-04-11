package daemon

import (
	"fmt"
	"os"
	"path/filepath"

	yamlv3 "gopkg.in/yaml.v3"
)

// loadYAMLFile reads a YAML file at path and unmarshals it into T.
// If the file does not exist and allowMissing is true, a zero-value T is
// returned without error. The raw bytes are also returned (nil when the file
// is missing) so callers can use them for size checks or quarantine.
//
// Read errors are returned unwrapped so that callers can use os.IsNotExist
// directly. Parse errors are wrapped with the filename for diagnostics.
func loadYAMLFile[T any](path string, allowMissing bool) (T, []byte, error) {
	var result T
	data, err := os.ReadFile(path) //nolint:gosec // path is constructed from a controlled application directory
	if err != nil {
		if os.IsNotExist(err) && allowMissing {
			return result, nil, nil
		}
		return result, nil, err
	}
	if err := yamlv3.Unmarshal(data, &result); err != nil {
		return result, data, fmt.Errorf("parse %s: %w", filepath.Base(path), err)
	}
	return result, data, nil
}
