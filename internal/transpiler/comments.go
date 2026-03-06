package transpiler

// StripJSONataComments removes C-style block comments (/* ... */) from a JSONata
// expression. It does not strip comment-like sequences inside string literals;
// both single- and double-quoted strings are respected, with backslash escapes
// for the delimiting quote and for backslash itself.
func StripJSONataComments(s string) string {
	if len(s) == 0 {
		return s
	}
	var out []byte
	i := 0
	const (
		stateNormal = iota
		stateInString
		stateInComment
	)
	state := stateNormal
	var quote byte // ' or " when state == stateInString

	for i < len(s) {
		c := s[i]
		switch state {
		case stateNormal:
			if c == '\'' || c == '"' {
				state = stateInString
				quote = c
				out = append(out, c)
				i++
				continue
			}
			if c == '/' && i+1 < len(s) && s[i+1] == '*' {
				state = stateInComment
				i += 2
				continue
			}
			out = append(out, c)
			i++

		case stateInString:
			if c == '\\' && i+1 < len(s) {
				out = append(out, s[i:i+2]...)
				i += 2
				continue
			}
			if c == quote {
				state = stateNormal
				out = append(out, c)
				i++
				continue
			}
			out = append(out, c)
			i++

		case stateInComment:
			if c == '*' && i+1 < len(s) && s[i+1] == '/' {
				state = stateNormal
				i += 2
				continue
			}
			i++
		}
	}
	return string(out)
}
