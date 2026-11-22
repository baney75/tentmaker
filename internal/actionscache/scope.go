package actionscache

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
)

// Scope names. A push-like job's scope is its branch, as on GitHub. A pull
// request job's is the PR head branch under its own prefix, so no PR job
// ever writes where a push to that branch name would read.
const (
	branchPrefix = "refs/heads/"
	prPrefix     = "refs/pull/head/"
)

// RefScope is the scope a job of this event and head branch reads first and
// writes to; "" when the branch is unknown.
func RefScope(event, headBranch string) string {
	if headBranch == "" {
		return ""
	}
	if strings.HasPrefix(event, "pull_request") {
		return prPrefix + headBranch
	}
	return branchPrefix + headBranch
}

// BranchScope is the scope of a branch's push jobs (the default branch's,
// which every job of the repo reads).
func BranchScope(branch string) string {
	if branch == "" {
		return ""
	}
	return branchPrefix + branch
}

// Permission bits of the runtime token's ac claim.
const (
	readBit  = 1
	writeBit = 2
)

type tokenScope struct {
	Scope      string `json:"Scope"`
	Permission int    `json:"Permission"`
}

// tokenScopes reads the ac claim of the runtime token the toolkit sends.
// The signature is not checked, so the claim can only narrow what the
// binding grants, never widen it: a job that forges it gains nothing it
// was not already bound to.
func tokenScopes(r *http.Request) ([]tokenScope, bool) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return nil, false
	}
	parts := strings.Split(strings.TrimSpace(tok), ".")
	if len(parts) != 3 || len(parts[1]) > 64<<10 {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil, false
	}
	var claims struct {
		AC string `json:"ac"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.AC == "" {
		return nil, false
	}
	var scopes []tokenScope
	if json.Unmarshal([]byte(claims.AC), &scopes) != nil || len(scopes) == 0 {
		return nil, false
	}
	return scopes, true
}

// tokenGrants says whether the token's scopes grant bit on ref. A branch
// scope needs the same branch; a PR scope needs any refs/pull/ scope (the
// token names the PR's merge ref, which the broker doesn't know).
func tokenGrants(scopes []tokenScope, ref string, bit int) bool {
	for _, t := range scopes {
		if t.Permission&bit == 0 {
			continue
		}
		switch {
		case strings.HasPrefix(ref, prPrefix):
			if strings.HasPrefix(t.Scope, "refs/pull/") {
				return true
			}
		case t.Scope == ref:
			return true
		}
	}
	return false
}

// readScopes are the scopes a restore searches, in order: the job's own
// ref when its token agrees, then the default branch.
func (s *Service) readScopes(g *grant, r *http.Request) []string {
	var out []string
	if g.Ref != "" {
		if ts, ok := tokenScopes(r); ok && tokenGrants(ts, g.Ref, readBit) {
			out = append(out, g.Ref)
		} else {
			s.warnToken(g)
		}
	}
	if g.Default != "" && (len(out) == 0 || out[0] != g.Default) {
		out = append(out, g.Default)
	}
	return out
}

// mayWrite says whether the job may save to its own ref now.
func (s *Service) mayWrite(g *grant, r *http.Request) (bool, string) {
	if g.Access < Write {
		return false, "this Tentmaker job may read the cache but not write it"
	}
	if g.Ref == "" {
		return false, "this job's branch is unknown"
	}
	ts, ok := tokenScopes(r)
	if !ok || !tokenGrants(ts, g.Ref, writeBit) {
		s.warnToken(g)
		return false, "the job's runtime token does not grant writes to " + g.Ref
	}
	return true, ""
}

func (s *Service) warnToken(g *grant) {
	if g.tokenWarn.CompareAndSwap(false, true) {
		s.log.Warn("a job's runtime token does not match the ref it was granted for; its own ref is not cached",
			"job", g.Job, "repo", g.Repo.Repo, "ref", g.Ref)
	}
}
