package config

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Pos is a 1-based position in the source file.
type Pos struct{ Line, Column int }

// SourceMap maps logical paths ("services.api.port", "smokeTests[1].command")
// to positions in the YAML source, so semantic errors can point at the exact
// line even though validation runs on decoded Go values.
type SourceMap struct {
	pos map[string]Pos
}

func newSourceMap(doc *yaml.Node) *SourceMap {
	s := &SourceMap{pos: map[string]Pos{"": {Line: 1, Column: 1}}}
	s.walk(doc, "")
	return s
}

func (s *SourceMap) walk(n *yaml.Node, path string) {
	switch n.Kind {
	case yaml.DocumentNode:
		for _, c := range n.Content {
			s.walk(c, path)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			p := k.Value
			if path != "" {
				p = path + "." + k.Value
			}
			s.pos[p] = Pos{k.Line, k.Column}
			s.walk(v, p)
		}
	case yaml.SequenceNode:
		for i, c := range n.Content {
			p := fmt.Sprintf("%s[%d]", path, i)
			s.pos[p] = Pos{c.Line, c.Column}
			s.walk(c, p)
		}
	}
	// Alias nodes are intentionally not followed: it keeps the walk finite for
	// self-referential anchors. Lookups fall back to the nearest known parent.
}

// Lookup returns the position of path, or of its nearest ancestor that exists
// in the source (for example when the key itself is missing).
func (s *SourceMap) Lookup(path string) Pos {
	if s == nil {
		return Pos{}
	}
	for {
		if p, ok := s.pos[path]; ok {
			return p
		}
		i := strings.LastIndexAny(path, ".[")
		if i < 0 {
			return s.pos[""]
		}
		path = path[:i]
	}
}

// mappingValue returns the value node for key in a mapping node, or nil.
func mappingValue(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}
