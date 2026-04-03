package validate

// IsValidIdentifier checks that a name is a safe directory component
// (no path separators or null bytes, not empty, not "." or "..").
func IsValidIdentifier(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	for _, r := range name {
		if r == '/' || r == '\\' || r == '\x00' {
			return false
		}
	}
	return true
}
