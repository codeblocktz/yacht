package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// The Raw Editor is Railway's: every variable that is not a secret, as one
// .env file, edited as text. Saving it replaces the set — a line added is a
// variable added, a line gone is a variable removed — and stages like any
// other edit. Secrets stay out of it. A textarea is a value on screen in
// full, which is exactly what a secret is for not being, and leaving them
// out cannot remove one by accident: what is not in the text is not part of
// what the text replaces.

// EnvLine is one KEY=value from a .env file, in the order it was written.
type EnvLine struct {
	Key   string
	Value string
}

// EnvLineError is a .env file refused because of one line in it, so the
// Raw Editor can point at that line rather than leave somebody counting.
type EnvLineError struct {
	Line int
	Err  error
}

func (e *EnvLineError) Error() string { return fmt.Sprintf("line %d: %v", e.Line, e.Err) }
func (e *EnvLineError) Unwrap() error { return e.Err }

func lineErr(line int, format string, args ...any) error {
	return &EnvLineError{Line: line, Err: fmt.Errorf(format, args...)}
}

// numberedLine is an EnvLine with the line its key was written on.
type numberedLine struct {
	EnvLine
	line int
}

// ParseEnv reads a .env file the way the tools people paste from write one:
// blank lines and lines starting with # are skipped, "export " is allowed
// in front of a name, and a value is single-quoted (taken as written),
// double-quoted (with \n, \t, \" and \\ understood, and free to run over
// several lines), or bare, where it ends at the line or at a " #" comment
// and surrounding spaces are dropped. A key given twice is the last one.
func ParseEnv(text string) ([]EnvLine, error) {
	numbered, err := parseEnv(text)
	if err != nil {
		return nil, err
	}
	out := make([]EnvLine, len(numbered))
	for i, l := range numbered {
		out[i] = l.EnvLine
	}
	return out, nil
}

func parseEnv(text string) ([]numberedLine, error) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	var (
		out   []numberedLine
		index = map[string]int{}
	)
	rest := text
	line := 0
	for rest != "" {
		var raw string
		raw, rest, _ = strings.Cut(rest, "\n")
		line++
		start := line
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		trimmed = strings.TrimPrefix(trimmed, "export ")
		key, value, ok := strings.Cut(trimmed, "=")
		if !ok {
			return nil, lineErr(line, "%q has no = — write it as KEY=value", trimmed)
		}
		key = strings.TrimSpace(key)
		if err := (VariableInput{Key: key}).Validate(); err != nil {
			return nil, &EnvLineError{Line: line, Err: err}
		}
		value = strings.TrimLeft(value, " \t")

		switch {
		case strings.HasPrefix(value, "'"):
			body, after, closed := strings.Cut(value[1:], "'")
			for !closed && rest != "" {
				var next string
				next, rest, _ = strings.Cut(rest, "\n")
				line++
				body += "\n"
				var tail string
				var found bool
				if tail, after, found = strings.Cut(next, "'"); found {
					body += tail
					closed = true
				} else {
					body += next
				}
			}
			if !closed {
				return nil, lineErr(start, "%s's value opens a ' and never closes it", key)
			}
			if err := trailing(after); err != nil {
				return nil, lineErr(line, "%s: %w", key, err)
			}
			value = body
		case strings.HasPrefix(value, `"`):
			body, after, closed := unquote(value[1:])
			for !closed && rest != "" {
				var next string
				next, rest, _ = strings.Cut(rest, "\n")
				line++
				var more string
				more, after, closed = unquote(next)
				body += "\n" + more
			}
			if !closed {
				return nil, lineErr(start, `%s's value opens a " and never closes it`, key)
			}
			if err := trailing(after); err != nil {
				return nil, lineErr(line, "%s: %w", key, err)
			}
			value = body
		default:
			// A # is a comment only after a space, so a URL's fragment and a
			// colour like #fff stay part of the value.
			if i := strings.Index(value, " #"); i >= 0 {
				value = value[:i]
			}
			if i := strings.Index(value, "\t#"); i >= 0 {
				value = value[:i]
			}
			value = strings.TrimSpace(value)
		}

		if i, seen := index[key]; seen {
			out[i].Value, out[i].line = value, start
			continue
		}
		index[key] = len(out)
		out = append(out, numberedLine{EnvLine: EnvLine{Key: key, Value: value}, line: start})
	}
	return out, nil
}

// unquote reads a double-quoted value up to its closing quote, reporting
// what follows it and whether it closed on this line.
func unquote(s string) (body, after string, closed bool) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s):
			i++
			switch s[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case '"', '\\', '$':
				b.WriteByte(s[i])
			default:
				b.WriteByte('\\')
				b.WriteByte(s[i])
			}
		case c == '"':
			return b.String(), s[i+1:], true
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), "", false
}

// trailing allows only a comment after a closing quote.
func trailing(after string) error {
	after = strings.TrimSpace(after)
	if after == "" || strings.HasPrefix(after, "#") {
		return nil
	}
	return fmt.Errorf("unexpected %q after the closing quote", after)
}

// FormatEnv writes variables as a .env file ParseEnv reads back to the same
// values: bare where a value is safe bare, and double-quoted with escapes
// where it is not. Secrets are left out — see the Raw Editor note above.
func FormatEnv(vars []Variable) string {
	var b strings.Builder
	for _, v := range vars {
		if v.Secret {
			continue
		}
		b.WriteString(v.Key)
		b.WriteByte('=')
		b.WriteString(quoteEnv(v.Value))
		b.WriteByte('\n')
	}
	return b.String()
}

func quoteEnv(value string) string {
	if value == "" {
		return ""
	}
	bare := value == strings.TrimSpace(value) &&
		!strings.ContainsAny(value, "\n\r\t\"'\\") &&
		!strings.Contains(value, " #") &&
		!strings.HasPrefix(value, "#")
	if bare {
		return value
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`)
	return `"` + r.Replace(value) + `"`
}

// SetRawVariables replaces an app's variables that are not secrets with the
// ones in a .env file: each line set, each missing one removed. Secrets are
// left as they are; a line naming one is refused, because writing it here
// would make it plain text without anybody deciding to.
//
// All of it is staged together, or none of it is. With StageChanges off it
// deploys at once, as one deployment rather than one per line.
func (s *Service) SetRawVariables(ctx context.Context, ownerID, appName, raw string) error {
	lines, err := parseEnv(raw)
	if err != nil {
		return err
	}
	a, err := s.Get(ctx, ownerID, appName)
	if err != nil {
		return err
	}
	current, err := s.desiredVariablesFor(ctx, s.q, ownerID, a.ID)
	if err != nil {
		return err
	}
	secret := map[string]bool{}
	for _, v := range current {
		secret[v.Key] = v.Secret
	}
	given := map[string]bool{}
	for _, l := range lines {
		if secret[l.Key] {
			return lineErr(l.line, "%s is a secret — change it in the table, where its value stays hidden, "+
				"or remove it there first to make it plain", l.Key)
		}
		if err := s.checkReferences(ctx, ownerID, a, l.Value); err != nil {
			return lineErr(l.line, "%s: %w", l.Key, err)
		}
		given[l.Key] = true
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("app: begin raw edit: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := s.q.WithTx(tx)
	for _, l := range lines {
		if err := s.stageVariableQ(ctx, q, a, VariableInput{Key: l.Key, Value: l.Value}); err != nil {
			return err
		}
	}
	for _, v := range current {
		if v.Secret || given[v.Key] {
			continue
		}
		if err := s.stageRemoval(ctx, q, a, v.Key); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("app: commit raw edit: %w", err)
	}

	if s.opts.StageChanges {
		return nil
	}
	switch err := s.DeployStaged(ctx, ownerID, appName); {
	case errors.Is(err, ErrNothingStaged):
		return nil
	case errors.Is(err, ErrOperationInFlight):
		return ErrSavedWhileDeploying
	default:
		return err
	}
}
