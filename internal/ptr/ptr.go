package ptr

// Bool returns a pointer to the given bool value.
func Bool(v bool) *bool { return &v }

// String returns a pointer to the given string value.
func String(v string) *string { return &v }

// Int returns a pointer to the given int value.
func Int(v int) *int { return &v }

// Float64 returns a pointer to the given float64 value.
func Float64(v float64) *float64 { return &v }
