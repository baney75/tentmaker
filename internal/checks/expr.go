package checks

import (
	"fmt"
	"regexp"
	"strings"
)

// Placeholders Plan leaves in the job for RunBundle to fill, since only the
// bundle knows the commit.
const (
	shaExpr = "${{ github.sha }}"
	refExpr = "${{ github.ref_name }}"
)

// exprCtx resolves the few ${{ }} expressions checks mode understands.
type exprCtx struct {
	matrix   *val   // the single matrix combination, nil when there is no matrix
	runnerOS string // "Linux", "macOS", "Windows"; empty while runs-on is unresolved
	forgejo  bool   // Forgejo also spells the github context "forge"
}

var (
	matrixDot = regexp.MustCompile(`^matrix((?:\.[A-Za-z_][A-Za-z0-9_-]*)+)$`)
	matrixIdx = regexp.MustCompile(`^matrix\[\s*'([^']+)'\s*\]$`)
)

// resolve substitutes every ${{ }} in s. It returns an error naming the
// first expression it can't evaluate.
func (c exprCtx) resolve(s string) (string, error) {
	if !strings.Contains(s, "${{") {
		return s, nil
	}
	var b strings.Builder
	rest := s
	for {
		i := strings.Index(rest, "${{")
		if i < 0 {
			b.WriteString(rest)
			return b.String(), nil
		}
		j := strings.Index(rest[i:], "}}")
		if j < 0 {
			return "", fmt.Errorf("unclosed ${{ in %q", firstLine(s))
		}
		inner := strings.TrimSpace(rest[i+3 : i+j])
		out, err := c.eval(inner)
		if err != nil {
			return "", err
		}
		b.WriteString(rest[:i])
		b.WriteString(out)
		rest = rest[i+j+2:]
	}
}

func (c exprCtx) eval(e string) (string, error) {
	switch e {
	case "github.sha":
		return shaExpr, nil
	case "github.ref_name":
		return refExpr, nil
	case "forge.sha":
		if c.forgejo {
			return shaExpr, nil
		}
	case "forge.ref_name":
		if c.forgejo {
			return refExpr, nil
		}
	case "runner.os":
		if c.runnerOS != "" {
			return c.runnerOS, nil
		}
	}
	var path []string
	if m := matrixDot.FindStringSubmatch(e); m != nil {
		path = strings.Split(m[1][1:], ".")
	} else if m := matrixIdx.FindStringSubmatch(e); m != nil {
		path = []string{m[1]}
	}
	if path != nil && c.matrix != nil {
		v := c.matrix
		for _, p := range path {
			v = v.get(p)
		}
		if v.isScalar() {
			return v.str(), nil
		}
	}
	return "", fmt.Errorf("can't evaluate ${{ %s }}", e)
}

func firstLine(s string) string {
	s = strings.TrimLeft(s, "\r\n")
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
