package internal

import (
	"fmt"
	"strings"
)

// SplitCommand splits a command string into arguments the way a POSIX shell
// would tokenise it: whitespace separates arguments, single quotes preserve
// everything literally, double quotes allow backslash-escaping of `"` and `\`,
// and a backslash outside quotes escapes the next character. No globbing,
// variable expansion or redirection is performed — the result is passed to
// ECS as the container's command override, exec-style.
func SplitCommand(s string) ([]string, error) {
	var (
		args    []string
		cur     strings.Builder
		inArg   bool
		quote   rune // 0, '\'' or '"'
		escaped bool
	)

	for _, r := range s {
		switch {
		case escaped:
			// Inside double quotes only `"` and `\` are escapable; any other
			// backslash is kept, as in a shell.
			if quote == '"' && r != '"' && r != '\\' {
				cur.WriteRune('\\')
			}
			cur.WriteRune(r)
			escaped = false
			inArg = true

		case r == '\\' && quote != '\'':
			escaped = true
			inArg = true

		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}

		case r == '\'' || r == '"':
			quote = r
			inArg = true // "" is a valid, empty argument

		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			if inArg {
				args = append(args, cur.String())
				cur.Reset()
				inArg = false
			}

		default:
			cur.WriteRune(r)
			inArg = true
		}
	}

	if escaped {
		return nil, fmt.Errorf("command ends with an unfinished backslash escape: %q", s)
	}
	if quote != 0 {
		return nil, fmt.Errorf("command has an unterminated %c quote: %q", quote, s)
	}
	if inArg {
		args = append(args, cur.String())
	}
	if len(args) == 0 {
		return nil, fmt.Errorf("command is empty")
	}
	return args, nil
}

// quoteCommand renders arguments back into a single shell-like string for
// display, quoting any argument that contains whitespace or quotes.
func quoteCommand(args []string) string {
	parts := make([]string, len(args))
	for i, a := range args {
		if a == "" || strings.ContainsAny(a, " \t\n\"'\\") {
			parts[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		} else {
			parts[i] = a
		}
	}
	return strings.Join(parts, " ")
}
