package validate

import "path/filepath"

// IsValidBaseName checks that name is a safe single-component file name:
// not empty, not "." or "..", and contains no directory separators.
func IsValidBaseName(name string) bool {
	return filepath.Base(name) == name && name != "." && name != ".."
}
