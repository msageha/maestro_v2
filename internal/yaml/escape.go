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
//
// Block scalars (| and > with their chomping/indentation variants) are copied
// verbatim: YAML performs no escape processing inside block scalar content, so
// a line like `"...\d..."` there is literal text, not a double-quoted scalar.
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
		case '|', '>':
			if end, ok := blockScalarEnd(data, i); ok {
				out = append(out, data[i:end]...)
				i = end
			} else {
				out = append(out, data[i])
				i++
			}
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

// blockScalarEnd reports whether the '|' or '>' at data[pos] begins a YAML
// block scalar header and, if so, returns the exclusive end index of the
// scalar's content so the caller can copy it verbatim.
//
// Detection follows the YAML 1.2 block scalar rules empirically verified
// against gopkg.in/yaml.v3:
//
//   - The indicator must sit at a value-start position (after "key: ",
//     after a "- " sequence dash, or alone on its line) and the rest of the
//     header line may contain only chomping/indentation indicators,
//     whitespace, and a comment.
//   - Content indentation is auto-detected from the first non-empty line
//     (or taken from an explicit indentation indicator). The first
//     non-empty line is content only when indented deeper than the node's
//     anchor column: the key start for "key: |", the last dash for "- |",
//     one column left of the indicator when it opens its own line.
//   - The scalar ends at the first non-empty line indented shallower than
//     the detected content indentation.
//
// Misdetection is biased toward over-extension: on malformed YAML the worst
// case is that a region is copied verbatim (the parse error surfaces
// unchanged), never that block scalar content is rewritten.
func blockScalarEnd(data []byte, pos int) (int, bool) {
	n := len(data)
	if !isBlockIndicatorContext(data, pos) {
		return 0, false
	}

	// Header: optional chomping (+/-) and indentation (1-9) indicators in
	// either order, then whitespace, then an optional comment, then EOL.
	j := pos + 1
	explicitIndent := -1
	for j < n && (data[j] == '+' || data[j] == '-' || (data[j] >= '1' && data[j] <= '9')) {
		if data[j] >= '1' && data[j] <= '9' {
			explicitIndent = int(data[j] - '0')
		}
		j++
	}
	for j < n && (data[j] == ' ' || data[j] == '\t') {
		j++
	}
	if j < n && data[j] == '#' {
		for j < n && data[j] != '\n' && data[j] != '\r' {
			j++
		}
	}
	if j < n && data[j] != '\n' && data[j] != '\r' {
		return 0, false
	}

	threshold := blockScalarThreshold(data, pos)

	// Advance past the header's line break.
	if j < n && data[j] == '\r' {
		j++
	}
	if j < n && data[j] == '\n' {
		j++
	}

	contentIndent := -1
	if explicitIndent >= 0 {
		contentIndent = threshold + explicitIndent
	}

	end := j
	for lineStart := j; lineStart < n; {
		p := lineStart
		indent := 0
		for p < n && data[p] == ' ' {
			p++
			indent++
		}
		empty := p >= n || data[p] == '\n' || data[p] == '\r'
		if !empty {
			if contentIndent < 0 {
				if indent <= threshold {
					return end, true // empty scalar; line belongs to the enclosing document
				}
				contentIndent = indent
			} else if indent < contentIndent {
				return end, true
			}
		}
		for p < n && data[p] != '\n' {
			p++
		}
		if p < n {
			p++ // consume '\n'
		}
		lineStart = p
		end = p
	}
	return end, true
}

// blockScalarThreshold computes the indentation column that following lines
// must exceed to count as content of the block scalar whose indicator is at
// data[pos]. See blockScalarEnd for the per-shape rules.
func blockScalarThreshold(data []byte, pos int) int {
	lineStart := pos
	for lineStart > 0 && data[lineStart-1] != '\n' {
		lineStart--
	}
	k := lineStart
	for k < pos && data[k] == ' ' {
		k++
	}
	if k == pos {
		// Indicator is the first token on its line ("key:\n  |\n  text"):
		// content may sit at the indicator's own column.
		return (pos - lineStart) - 1
	}
	lastDashCol := -1
	for k < pos && data[k] == '-' && (data[k+1] == ' ' || data[k+1] == '\t') {
		lastDashCol = k - lineStart
		k++
		for k < pos && (data[k] == ' ' || data[k] == '\t') {
			k++
		}
	}
	if k == pos && lastDashCol >= 0 {
		// "- |": content must be indented past the dash column.
		return lastDashCol
	}
	// "key: |" (possibly after a dash prefix): content must be indented
	// past the key's start column.
	return k - lineStart
}

// isBlockIndicatorContext reports whether the '|' or '>' at data[pos] is in a
// position where a block scalar header may begin: at buffer start, at line
// start, or separated by whitespace from a preceding ':' or '-'. Anchor
// (&name) and tag (!tag) tokens between the ':'/'-' and the indicator are
// skipped, so "key: &a |" is still recognised.
func isBlockIndicatorContext(data []byte, pos int) bool {
	j := pos - 1
	for {
		for j >= 0 && (data[j] == ' ' || data[j] == '\t') {
			j--
		}
		if j < 0 {
			return true
		}
		switch data[j] {
		case '\n', '\r':
			return true
		case ':', '-':
			// Require separating whitespace: "key:|" and "-|" are plain scalars.
			return j < pos-1
		}
		// Possibly an anchor or tag token preceding the indicator.
		for j >= 0 && data[j] != ' ' && data[j] != '\t' && data[j] != '\n' && data[j] != '\r' {
			j--
		}
		if data[j+1] != '&' && data[j+1] != '!' {
			return false
		}
	}
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
