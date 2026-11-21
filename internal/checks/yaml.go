package checks

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// val is a YAML value with mapping order kept. Scalars keep the text as
// written (so "3.10" stays "3.10"); aliases and merge keys are resolved.
type val struct {
	kind   yaml.Kind // ScalarNode, MappingNode or SequenceNode
	s      string    // scalar text
	tag    string    // scalar tag, such as "!!null" or "!!bool"
	keys   []string  // mapping keys in order
	m      map[string]*val
	seq    []*val
	line   int
	quoted bool
}

func parseYAML(src []byte) (*val, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, err
	}
	if doc.Kind == 0 {
		return &val{kind: yaml.MappingNode, m: map[string]*val{}}, nil
	}
	return convert(&doc, 0)
}

func convert(n *yaml.Node, depth int) (*val, error) {
	if depth > 64 {
		return nil, fmt.Errorf("YAML nests too deeply near line %d", n.Line)
	}
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) == 0 {
			return &val{kind: yaml.MappingNode, m: map[string]*val{}}, nil
		}
		return convert(n.Content[0], depth+1)
	case yaml.AliasNode:
		return convert(n.Alias, depth+1)
	case yaml.ScalarNode:
		return &val{kind: yaml.ScalarNode, s: n.Value, tag: n.ShortTag(), line: n.Line,
			quoted: n.Style&(yaml.SingleQuotedStyle|yaml.DoubleQuotedStyle) != 0}, nil
	case yaml.SequenceNode:
		v := &val{kind: yaml.SequenceNode, line: n.Line}
		for _, c := range n.Content {
			cv, err := convert(c, depth+1)
			if err != nil {
				return nil, err
			}
			v.seq = append(v.seq, cv)
		}
		return v, nil
	case yaml.MappingNode:
		v := &val{kind: yaml.MappingNode, m: map[string]*val{}, line: n.Line}
		var merges []*val
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, c := n.Content[i], n.Content[i+1]
			cv, err := convert(c, depth+1)
			if err != nil {
				return nil, err
			}
			if k.Kind == yaml.ScalarNode && k.ShortTag() == "!!merge" {
				if cv.kind == yaml.SequenceNode {
					merges = append(merges, cv.seq...)
				} else {
					merges = append(merges, cv)
				}
				continue
			}
			v.set(k.Value, cv)
		}
		// Merge keys fill in only what the mapping doesn't set itself;
		// earlier merge sources win over later ones.
		for _, src := range merges {
			if src.kind != yaml.MappingNode {
				return nil, fmt.Errorf("line %d: << must merge a mapping", n.Line)
			}
			for _, k := range src.keys {
				if _, ok := v.m[k]; !ok {
					v.set(k, src.m[k])
				}
			}
		}
		return v, nil
	}
	return nil, fmt.Errorf("line %d: unsupported YAML node", n.Line)
}

func (v *val) set(k string, c *val) {
	if _, ok := v.m[k]; !ok {
		v.keys = append(v.keys, k)
	}
	v.m[k] = c
}

// get returns the value at key, or nil.
func (v *val) get(key string) *val {
	if v == nil || v.kind != yaml.MappingNode {
		return nil
	}
	return v.m[key]
}

func (v *val) has(key string) bool { return v.get(key) != nil }

func (v *val) isMap() bool    { return v != nil && v.kind == yaml.MappingNode }
func (v *val) isSeq() bool    { return v != nil && v.kind == yaml.SequenceNode }
func (v *val) isScalar() bool { return v != nil && v.kind == yaml.ScalarNode }
func (v *val) isNull() bool   { return v == nil || (v.kind == yaml.ScalarNode && v.tag == "!!null") }

// str returns a scalar's text ("" for null or non-scalars).
func (v *val) str() string {
	if v == nil || v.kind != yaml.ScalarNode || v.tag == "!!null" {
		return ""
	}
	return v.s
}

// strings returns a scalar as a one-item list or a sequence of scalars.
// ok is false when an item isn't a scalar.
func (v *val) strings() (out []string, ok bool) {
	switch {
	case v.isNull():
		return nil, true
	case v.isScalar():
		return []string{v.s}, true
	case v.isSeq():
		for _, c := range v.seq {
			if !c.isScalar() {
				return nil, false
			}
			out = append(out, c.str())
		}
		return out, true
	}
	return nil, false
}

// text renders a value as compact text for names and comparisons.
func (v *val) text() string {
	switch {
	case v.isNull():
		return ""
	case v.isScalar():
		return v.s
	case v.isSeq():
		parts := make([]string, len(v.seq))
		for i, c := range v.seq {
			parts[i] = c.text()
		}
		return "[" + strings.Join(parts, ", ") + "]"
	default:
		keys := append([]string(nil), v.keys...)
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = k + ": " + v.m[k].text()
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
}

// clone deep-copies v.
func (v *val) clone() *val {
	if v == nil {
		return nil
	}
	c := *v
	if v.m != nil {
		c.keys = append([]string(nil), v.keys...)
		c.m = make(map[string]*val, len(v.m))
		for k, x := range v.m {
			c.m[k] = x.clone()
		}
	}
	if v.seq != nil {
		c.seq = make([]*val, len(v.seq))
		for i, x := range v.seq {
			c.seq[i] = x.clone()
		}
	}
	return &c
}

// deepMerge returns base with over merged in the way GitLab's extends does:
// mappings merge key by key, anything else in over replaces base.
func deepMerge(base, over *val) *val {
	if !base.isMap() || !over.isMap() {
		return over.clone()
	}
	out := base.clone()
	for _, k := range over.keys {
		if b, ok := out.m[k]; ok {
			out.set(k, deepMerge(b, over.m[k]))
		} else {
			out.set(k, over.m[k].clone())
		}
	}
	return out
}

func isTrue(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "yes", "on":
		return true
	}
	return false
}

const mappingKind = yaml.MappingNode
