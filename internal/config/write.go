package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Writing one prompt back to the config file.
//
// This is the only path that MODIFIES the config, and it exists because the App
// Home tab edits prompts (§7e). Everything about it is shaped by two facts
// about the file it is editing.
//
// The first is that the file holds ${ENV} REFERENCES and the loaded Config
// holds their expanded values. Marshalling the in-memory struct back over the
// file would therefore write live bot tokens into it, in plain text, as a side
// effect of somebody rewording a prompt. So nothing is marshalled: the raw
// bytes are read, one span of them is replaced, and everything else is passed
// through untouched.
//
// The second is that the file is heavily commented, on purpose, and a config
// you cannot read is a config you cannot fix. A yaml.Node round-trip keeps the
// comments but silently drops the blank lines between sections, which reflows a
// carefully laid-out file a little further every time a prompt is edited. So
// the parser is used to LOCATE the value and the edit is textual: the touched
// lines change and the rest of the file is byte-identical.

// SetPrompt rewrites one prompt in the config file and in this Config.
//
// An empty text RESETS the prompt: the key is deleted rather than filled with
// the default's own words. That keeps "never overridden" distinguishable from
// "overridden to whatever the default happened to say when I pressed reset" —
// and means a later change to the default reaches an install that never had an
// opinion.
//
// The file is re-read here rather than remembered from load. The daemon holds a
// Config for days; the file underneath it may have been edited by hand or by
// `riggs install` in the meantime, and rewriting a remembered copy would undo
// that silently.
func (c *Config) SetPrompt(id PromptID, text string) error {
	spec, ok := LookupPrompt(id)
	if !ok {
		return fmt.Errorf("config: %q is not an editable prompt", id)
	}
	if c.Path == "" || c.Path == NoFilePath {
		return fmt.Errorf("config: there is no config file to write to (run `riggs install`)")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	data, err := os.ReadFile(c.Path)
	if err != nil {
		return fmt.Errorf("config: reading %s: %w", c.Path, err)
	}
	updated, err := setScalar(data, spec.Path, strings.TrimSpace(text))
	if err != nil {
		return fmt.Errorf("config: editing %s: %w", c.Path, err)
	}
	// Parsed before it is kept, so a bug in the surgery above is caught here
	// rather than at the next start-up — by which time the daemon has exited
	// and the file it cannot read is the only copy.
	var check Config
	if err := yaml.Unmarshal(updated, &check); err != nil {
		return fmt.Errorf("config: the edit would make %s unparseable: %w", c.Path, err)
	}
	if err := writeAtomic(c.Path, updated); err != nil {
		return err
	}
	// Only now. The in-memory value must never claim an edit the file did not
	// take: the daemon would act on a prompt that vanishes at the next restart.
	spec.set(c, strings.TrimSpace(text))
	return nil
}

// writeAtomic replaces path's contents through a temporary file in the same
// directory.
//
// Same directory because rename is only atomic within a filesystem, and 0600
// because the file may hold literal tokens — set on the temporary file BEFORE
// anything is written into it, not after, so there is no window in which a
// world-readable file holds them.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".riggs-config-*")
	if err != nil {
		return fmt.Errorf("config: creating a temporary file in %s: %w", dir, err)
	}
	name := tmp.Name()
	defer os.Remove(name) // a no-op once the rename has succeeded

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("config: securing %s: %w", name, err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("config: writing %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("config: closing %s: %w", name, err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("config: replacing %s: %w", path, err)
	}
	return nil
}

// setScalar replaces — or inserts, or deletes — the scalar at path in a YAML
// document, changing nothing else about the bytes.
//
// path is a sequence of mapping keys, two deep in every current use
// ("review-request", "prompt"). An empty value deletes the key.
func setScalar(data []byte, path []string, value string) ([]byte, error) {
	if len(path) != 2 {
		return nil, fmt.Errorf("only a two-level path is supported, got %v", path)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	lines := splitLines(data)

	sectionKey, sectionValue, keyNode := locate(&doc, path)
	switch {
	case keyNode != nil:
		// The key is there. Replace everything it spans — which for a block
		// scalar is several lines — with one line, or with nothing at all.
		indent := keyNode.Column - 1
		first := keyNode.Line - 1
		last := blockEnd(lines, first, indent)
		if value == "" {
			return joinLines(cut(lines, first, last)), nil
		}
		return joinLines(splice(lines, first, last,
			renderScalar(indent, path[1], value, keyNode.LineComment))), nil

	case sectionValue != nil && sectionValue.Kind == yaml.MappingNode:
		// The section exists but not the key. Nothing to delete.
		if value == "" {
			return data, nil
		}
		// Inserted as the section's FIRST key rather than appended after its
		// last. Appending would have to decide whether a trailing comment block
		// belongs to this section or heads the next one, and that is not
		// decidable from the text; inserting at the top never has to ask.
		return joinLines(insert(lines, sectionKey.Line,
			renderScalar(sectionIndent(sectionKey, lines), path[1], value, ""))), nil

	case sectionKey != nil:
		// The section is declared with nothing under it (`ai:` and a newline),
		// which parses as null and has no mapping to insert into. Its own line
		// is rewritten to carry the key, rather than a second `ai:` being
		// appended — a duplicate mapping key makes the whole file unloadable.
		if value == "" {
			return data, nil
		}
		indent := sectionKey.Column - 1
		first := sectionKey.Line - 1
		last := blockEnd(lines, first, indent)
		return joinLines(splice(lines, first, last,
			strings.Repeat(" ", indent)+path[0]+":",
			renderScalar(indent+2, path[1], value, ""))), nil

	default:
		if value == "" {
			return data, nil
		}
		// Neither. Append the whole section, after a blank line so it does not
		// run into whatever the file ended with.
		out := append([]string{}, lines...)
		for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
			out = out[:len(out)-1]
		}
		out = append(out, "", path[0]+":", renderScalar(2, path[1], value, ""))
		return joinLines(out), nil
	}
}

// locate finds the section's key and value nodes and, within the section, the
// target key. Any of the three may be nil, which is how the caller tells
// "replace" from "insert" from "rewrite the empty section" from "append".
func locate(doc *yaml.Node, path []string) (sectionKey, sectionValue, key *yaml.Node) {
	if len(doc.Content) == 0 {
		return nil, nil, nil
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, nil, nil
	}
	sectionKey, sectionValue = mappingEntry(root, path[0])
	if sectionValue == nil || sectionValue.Kind != yaml.MappingNode {
		return sectionKey, sectionValue, nil
	}
	k, _ := mappingEntry(sectionValue, path[1])
	return sectionKey, sectionValue, k
}

// mappingEntry returns the key and value nodes for name in a mapping.
func mappingEntry(m *yaml.Node, name string) (key, value *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == name {
			return m.Content[i], m.Content[i+1]
		}
	}
	return nil, nil
}

// blockEnd reports the last line index the entry starting at first occupies.
//
// An entry owns its own line plus every following line that is blank or
// indented deeper than its key — which is exactly the extent of a block scalar,
// a nested mapping or a wrapped plain scalar, without this having to tell them
// apart. Trailing blank lines are given back, because they separate this entry
// from the next rather than belonging to it.
func blockEnd(lines []string, first, indent int) int {
	last := first
	for i := first + 1; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "" {
			continue
		}
		if indentOf(line) <= indent {
			break
		}
		last = i
	}
	return last
}

// sectionIndent is the indentation its keys are written at, taken from the
// first one already there and defaulting to two spaces.
func sectionIndent(sectionKey *yaml.Node, lines []string) int {
	next := sectionKey.Line // 0-based index of the line after the key
	if next < len(lines) && strings.TrimSpace(lines[next]) != "" {
		if n := indentOf(lines[next]); n > sectionKey.Column-1 {
			return n
		}
	}
	return sectionKey.Column - 1 + 2
}

// renderScalar writes one `key: "value"` line.
//
// Always double-quoted and always on one line, whatever the value contains. A
// prompt is prose: it can hold a colon, a leading `{`, a `#`, or a newline from
// a multi-line modal input, and every one of those changes what a plain scalar
// means. Quoting unconditionally means the writer never has to be right about
// which of them needs it.
func renderScalar(indent int, key, value, lineComment string) string {
	line := strings.Repeat(" ", indent) + key + ": " + quote(value)
	if c := strings.TrimSpace(lineComment); c != "" {
		// Kept: it is the admin's note about this setting, and losing it
		// because the setting was edited is a poor trade.
		if !strings.HasPrefix(c, "#") {
			c = "# " + c
		}
		line += " " + c
	}
	return line
}

// quote renders s as a YAML double-quoted scalar.
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// indentOf counts the leading spaces on a line.
func indentOf(line string) int {
	return len(line) - len(strings.TrimLeft(line, " "))
}

// insert puts line at index at.
func insert(lines []string, at int, line string) []string {
	return splice(lines, at, at-1, line)
}

// splice replaces lines[first:last+1] with replacement. last < first inserts
// without removing anything.
func splice(lines []string, first, last int, replacement ...string) []string {
	out := append([]string{}, lines[:first]...)
	out = append(out, replacement...)
	if last+1 < len(lines) {
		return append(out, lines[last+1:]...)
	}
	return out
}

// cut removes lines[first:last+1].
func cut(lines []string, first, last int) []string {
	return splice(lines, first, last)
}

// splitLines breaks the document into lines, dropping the trailing empty
// element a final newline produces so joinLines can put exactly one back.
func splitLines(data []byte) []string {
	s := string(data)
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// joinLines reassembles the document, newline-terminated.
func joinLines(lines []string) []byte {
	if len(lines) == 0 {
		return nil
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}
