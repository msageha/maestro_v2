package yaml

// SanitizeDoubleQuoteEscapes preprocesses raw YAML bytes to fix invalid
// backslash escape sequences in double-quoted strings. YAML 1.2 (used by
// gopkg.in/yaml.v3) only recognises specific escape characters after a
// backslash in double-quoted scalars. When Planner agents generate YAML via
// heredoc, content fields may contain sequences like \! or \: inside
// double-quoted strings, which are not valid YAML escape sequences and cause
// "found unknown escape character" parse errors.
//
// This function scans the input for double-quoted strings and doubles any
// backslash that precedes a character not in the YAML 1.2 valid escape set,
// turning it into a literal backslash. For correctly-formed YAML the function
// is a no-op.
func SanitizeDoubleQuoteEscapes(data []byte) []byte {
	if len(data) == 0 {
		return data
	}

	n := len(data)
	// Pre-allocate output with same capacity; expansions are rare.
	out := make([]byte, 0, n)

	i := 0
	for i < n {
		switch data[i] {
		case '"':
			if isValueStart(data, i) {
				out = append(out, '"')
				i++
				i = processDoubleQuoted(data, i, &out)
			} else {
				// Not a value start, but still skip to the closing quote
				// so that '#' inside is not misidentified as a comment.
				out = append(out, '"')
				i++
				i = skipNonValueDoubleQuoted(data, i, &out)
			}
		case '\'':
			// Skip single-quoted strings entirely (no escape processing).
			out = append(out, '\'')
			i++
			i = skipSingleQuoted(data, i, &out)
		case '#':
			// Comment: copy to end of line without processing.
			if i == 0 || data[i-1] == ' ' || data[i-1] == '\t' || data[i-1] == '\n' {
				for i < n && data[i] != '\n' {
					out = append(out, data[i])
					i++
				}
			} else {
				out = append(out, data[i])
				i++
			}
		default:
			out = append(out, data[i])
			i++
		}
	}
	return out
}

// isValueStart returns true when the double-quote at data[pos] is in a
// position where YAML expects a scalar value to begin—i.e. it starts a
// double-quoted scalar rather than appearing inside a plain scalar.
func isValueStart(data []byte, pos int) bool {
	if pos == 0 {
		return true
	}
	// Walk backwards past horizontal whitespace.
	j := pos - 1
	for j >= 0 && (data[j] == ' ' || data[j] == '\t') {
		j--
	}
	if j < 0 {
		return true
	}
	switch data[j] {
	case ':', '-', '\n', '\r', '[', '{', ',':
		return true
	}
	return false
}

// processDoubleQuoted scans from the byte immediately after the opening '"'
// and writes sanitised output. It returns the index after the closing '"'
// (or end of data).
func processDoubleQuoted(data []byte, i int, out *[]byte) int {
	n := len(data)
	for i < n {
		ch := data[i]
		if ch == '\\' && i+1 < n {
			next := data[i+1]
			if validYAMLEscape[next] {
				*out = append(*out, ch, next)
				i += 2
			} else {
				// Invalid escape → emit \\ so the backslash becomes literal.
				*out = append(*out, '\\', '\\')
				i++ // advance past the backslash only; next char is re-examined.
			}
		} else if ch == '"' {
			*out = append(*out, '"')
			i++
			return i
		} else {
			*out = append(*out, ch)
			i++
		}
	}
	return i
}

// skipNonValueDoubleQuoted copies bytes verbatim from inside a double-quoted
// region that was not recognised as a YAML value start. This prevents '#'
// characters inside the quotes from being misidentified as YAML comments.
// It handles backslash-escaped closing quotes (e.g. \") but does NOT perform
// the escape sanitisation that processDoubleQuoted does.
func skipNonValueDoubleQuoted(data []byte, i int, out *[]byte) int {
	n := len(data)
	for i < n {
		ch := data[i]
		if ch == '\\' && i+1 < n {
			// Copy escaped pair verbatim (handles \")
			*out = append(*out, ch, data[i+1])
			i += 2
		} else if ch == '"' {
			*out = append(*out, '"')
			i++
			return i
		} else {
			*out = append(*out, ch)
			i++
		}
	}
	return i
}

// skipSingleQuoted copies bytes from inside a single-quoted scalar verbatim.
// It returns the index after the closing '\” (or end of data).
func skipSingleQuoted(data []byte, i int, out *[]byte) int {
	n := len(data)
	for i < n {
		if data[i] == '\'' {
			*out = append(*out, '\'')
			i++
			// '' inside a single-quoted string is an escaped single quote.
			if i < n && data[i] == '\'' {
				*out = append(*out, '\'')
				i++
			} else {
				return i // end of single-quoted scalar
			}
		} else {
			*out = append(*out, data[i])
			i++
		}
	}
	return i
}

// validYAMLEscape is the set of characters that may follow a backslash in a
// YAML 1.2 double-quoted scalar (YAML spec §8.5.13 "Escaped Characters").
var validYAMLEscape = func() [256]bool {
	var m [256]bool
	for _, c := range []byte{
		'0', 'a', 'b', 't', '\t', 'n', 'v', 'f', 'r',
		'e', ' ', '"', '/', '\\', 'N', '_', 'L', 'P',
		'x', 'u', 'U',
		'\n', '\r', // line-break escapes
	} {
		m[c] = true
	}
	return m
}()
