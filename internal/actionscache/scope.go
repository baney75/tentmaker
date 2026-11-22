package actionscache

import "strings"

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

// Confirm narrows b to the job its runner took, which the broker read from
// the forge (the job whose runner is this one, and its run's event and head
// branch). A JIT runner takes whichever queued job matches its labels, so
// the job b was granted for proves nothing. Write survives only when the
// job is a push-like event on b's own ref; any other job keeps at most Read
// of its own scope and the default branch. Confirmation is for good: a
// confirmed binding is returned unchanged.
func (b Binding) Confirm(event, headBranch string) Binding {
	if b.Confirmed {
		return b
	}
	ref := RefScope(event, headBranch)
	if ref == "" || ref != b.Ref || strings.HasPrefix(event, "pull_request") {
		b.Ref = ref
		b.Access = min(b.Access, Read)
	}
	b.Confirmed = true
	return b
}

// readScopes are the scopes a restore searches, in order: the job's own
// ref once its job is confirmed, then the default branch, which every job
// of the repo may read.
func readScopes(g *grant) []string {
	var out []string
	if g.Confirmed && g.Ref != "" {
		out = append(out, g.Ref)
	}
	if g.Default != "" && (len(out) == 0 || out[0] != g.Default) {
		out = append(out, g.Default)
	}
	return out
}

// mayWrite says whether the job may save to its own ref now. The runtime
// token's claims play no part: its signature can't be checked here, so a
// job could write any claim it liked.
func mayWrite(g *grant) (bool, string) {
	switch {
	case g.Access < Write:
		return false, "this Tentmaker job may read the cache but not write it"
	case g.Ref == "":
		return false, "this job's branch is unknown"
	case !g.Confirmed:
		return false, "Tentmaker has not yet confirmed which job this runner took"
	}
	return true, ""
}
