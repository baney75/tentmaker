package checks

import (
	"fmt"
	"strings"
)

// matrixCombos expands strategy.matrix the way GitHub does: the cartesian
// product of the plain keys, minus exclude, plus include. Each combination
// is a mapping in key order.
func matrixCombos(m *val) ([]*val, error) {
	if m.isScalar() {
		if strings.Contains(m.s, "${{") {
			return nil, fmt.Errorf("can't evaluate the matrix %s", m.s)
		}
		return nil, fmt.Errorf("matrix must be a mapping")
	}
	if !m.isMap() {
		return nil, fmt.Errorf("matrix must be a mapping")
	}
	combos := []*val{newMap()}
	var plain []string
	for _, k := range m.keys {
		if k == "include" || k == "exclude" {
			continue
		}
		plain = append(plain, k)
		vs := m.m[k]
		if vs.isScalar() && strings.Contains(vs.s, "${{") {
			return nil, fmt.Errorf("can't evaluate matrix.%s %s", k, vs.s)
		}
		if !vs.isSeq() {
			return nil, fmt.Errorf("matrix.%s must be a list", k)
		}
		var next []*val
		for _, c := range combos {
			for _, x := range vs.seq {
				n := c.clone()
				n.set(k, x)
				next = append(next, n)
			}
		}
		combos = next
	}
	if len(plain) == 0 {
		combos = nil
	}
	if ex := m.get("exclude"); ex != nil {
		if !ex.isSeq() {
			return nil, fmt.Errorf("matrix.exclude must be a list")
		}
		var kept []*val
		for _, c := range combos {
			drop := false
			for _, e := range ex.seq {
				if e.isMap() && matches(c, e, e.keys) {
					drop = true
					break
				}
			}
			if !drop {
				kept = append(kept, c)
			}
		}
		combos = kept
	}
	if inc := m.get("include"); inc != nil {
		if !inc.isSeq() {
			return nil, fmt.Errorf("matrix.include must be a list")
		}
		base := len(combos)
		for _, e := range inc.seq {
			if !e.isMap() {
				return nil, fmt.Errorf("matrix.include entries must be mappings")
			}
			var origKeys []string
			for _, k := range e.keys {
				if contains(plain, k) {
					origKeys = append(origKeys, k)
				}
			}
			hit := false
			for i := 0; i < base; i++ {
				if matches(combos[i], e, origKeys) {
					hit = true
					for _, k := range e.keys {
						if !contains(plain, k) {
							combos[i].set(k, e.m[k])
						}
					}
				}
			}
			if !hit {
				combos = append(combos, e.clone())
			}
		}
	}
	return combos, nil
}

func matches(c, e *val, keys []string) bool {
	for _, k := range keys {
		x := c.get(k)
		if x == nil || x.text() != e.m[k].text() {
			return false
		}
	}
	return true
}

func newMap() *val { return &val{kind: mappingKind, m: map[string]*val{}} }

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
