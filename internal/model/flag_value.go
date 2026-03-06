package model

import "strings"

// StringSlice implements [flag.Value] for flags that can be specified multiple times.
// Each call to Set appends to the slice, allowing repeated flags like --foo a --foo b.
type StringSlice []string

// String returns the comma-joined representation of the slice.
func (s *StringSlice) String() string {
	if s == nil {
		return ""
	}
	return strings.Join(*s, ",")
}

// Set appends a value to the slice.
func (s *StringSlice) Set(v string) error {
	*s = append(*s, v)
	return nil
}
