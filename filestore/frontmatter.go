package filestore

import (
	"sort"
	"strings"
	"time"

	"github.com/ChristopherDavenport/agentmemory"
)

// An entry file is a frontmatter block between "---" lines followed by
// the content verbatim:
//
//	---
//	name: style
//	updated: 2026-09-20T10:00:00.123456789Z
//	description: How the user likes code written
//	type: feedback
//	---
//	<content>
//
// The block is a flat list of "key: value" lines, one value per line,
// which is the subset of YAML a person reads without help and this
// package parses without a library. name and updated are the store's;
// every other line is meta, written in key order. The content starts
// right after the closing line and is not touched, so its hash is the
// hash of the file's tail.

const delimiter = "---\n"

// encode renders an entry file.
func encode(e agentmemory.Entry) []byte {
	var b strings.Builder
	b.WriteString(delimiter)
	writeField(&b, "name", e.Name)
	writeField(&b, "updated", e.Updated.UTC().Format(time.RFC3339Nano))
	keys := make([]string, 0, len(e.Meta))
	for k := range e.Meta {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		writeField(&b, k, e.Meta[k])
	}
	b.WriteString(delimiter)
	b.WriteString(e.Content)
	return []byte(b.String())
}

func writeField(b *strings.Builder, key, value string) {
	b.WriteString(key)
	b.WriteByte(':')
	if value != "" {
		b.WriteByte(' ')
		b.WriteString(value)
	}
	b.WriteByte('\n')
}

// decoded is what an entry file holds.
type decoded struct {
	meta    map[string]string
	updated time.Time // zero when the file has no updated line
	content string
}

// decode parses an entry file. A file without a frontmatter block, as
// a person may write one, is all content. In the block, a key that is
// not kebab-case is ignored, as is the name line, since the file name
// is the name; values are trimmed of surrounding space.
func decode(data []byte) decoded {
	s := string(data)
	if !strings.HasPrefix(s, delimiter) {
		return decoded{content: s}
	}
	rest := s[len(delimiter):]
	var block string
	switch {
	case strings.HasPrefix(rest, delimiter):
		block, rest = "", rest[len(delimiter):]
	default:
		i := strings.Index(rest, "\n"+delimiter)
		if i < 0 {
			// An opening line with no closing one: a person's content
			// that happens to start with a rule.
			return decoded{content: s}
		}
		block, rest = rest[:i], rest[i+1+len(delimiter):]
	}
	d := decoded{content: rest}
	for line := range strings.SplitSeq(block, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch key {
		case "name":
		case "updated":
			if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
				d.updated = t
			}
		default:
			if !agentmemory.ValidName(key) {
				continue
			}
			if d.meta == nil {
				d.meta = map[string]string{}
			}
			d.meta[key] = value
		}
	}
	return d
}
