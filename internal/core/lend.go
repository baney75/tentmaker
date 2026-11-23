package core

import (
	"context"
	"strings"
)

// Trust levels of a device in a pool (docs/DESIGN.md, Mesh: lent devices).
const (
	// TrustOwner is a device of the pool owner's own tailnet user (or a
	// mesh.members login): it runs every job the broker hands out.
	TrustOwner = "owner"
	// TrustTrusted is a lent device that runs only the invite's repos (all
	// when it lists none), and only jobs the broker vouches no fork can
	// reach, never a fork or pull_request_target job.
	TrustTrusted = "trusted"
	// TrustPublic is a lent device that runs only fork pull_request jobs of
	// public repos that opted in (lend_public), with no cache of any kind.
	TrustPublic = "public"
	// TrustRevoked is a guest the pool removed: it may still report the
	// jobs it runs, so they end cleanly, and gets no new work.
	TrustRevoked = "revoked"
)

// EventPullRequest and EventPullRequestTarget are GitHub run events the
// trust rules name.
const (
	EventPullRequest       = "pull_request"
	EventPullRequestTarget = "pull_request_target"
)

// Lend is what a lent device may run: its trust level and, for trusted, the
// repos (owner/name) it may run, all of them when empty.
type Lend struct {
	Trust string   `json:"trust"`
	Repos []string `json:"repos,omitempty"`
}

// ValidLendTrust reports whether t is a trust level an invite can carry.
func ValidLendTrust(t string) bool { return t == TrustTrusted || t == TrustPublic }

// AllowsRepo reports whether repo is in the lend's repo list (any repo when
// the list is empty). Names compare without case, as forges do.
func (l Lend) AllowsRepo(repo string) bool {
	if len(l.Repos) == 0 {
		return true
	}
	for _, r := range l.Repos {
		if strings.EqualFold(r, repo) {
			return true
		}
	}
	return false
}

// LendRefusal says why a device lent with l must not run offer off, or ""
// when it may. Both sides call it: the broker before it hands the offer to
// a lent device, and the lent device's worker when the offer arrives, so
// neither has to trust the other.
//   - owner: anything.
//   - trusted: a repo on the list, a job the broker vouched no fork can
//     reach (Cache set: a check job, or a runner of a private repo whose
//     fork PR workflows are off), never a fork or pull_request_target job.
//   - public: a runner (not a check) for a fork's pull_request job, with no
//     cache. The broker grants it only for a public repo that set
//     lend_public.
//   - revoked, or anything else: nothing.
func LendRefusal(l Lend, off WorkOffer) string {
	j := off.Job
	check := j.Event == KindCheck
	switch l.Trust {
	case TrustOwner:
		return ""
	case TrustTrusted:
		switch {
		case !l.AllowsRepo(j.Repo):
			return "repo " + j.Repo + " is not on this lent device's list"
		case j.FromFork:
			return "a trusted lent device runs no fork job"
		case j.Event == EventPullRequestTarget:
			return "a trusted lent device runs no pull_request_target job"
		case !off.Cache:
			return "a trusted lent device runs only jobs no fork can reach"
		}
		return ""
	case TrustPublic:
		switch {
		case check:
			return "a public lent device runs no check job"
		case !j.FromFork || j.Event != EventPullRequest:
			return "a public lent device runs only a fork's pull_request job"
		case off.Cache || off.Actions != nil:
			return "a public lent device gets no cache"
		}
		return ""
	case TrustRevoked:
		return "this device was removed from the pool"
	}
	return "unknown trust level " + l.Trust
}

// TakenRefusal says why a lent runner must stop after the forge reports the
// job it actually took (a JIT runner takes any queued job its labels fit),
// or "" when it may go on. event is the taken job's run event; fork says
// whether it came from a fork. The worker doesn't learn that (it passes
// false), so a public device's broker also stops a public runner that took
// a job not from a fork.
func TakenRefusal(trust, event string, fork bool) string {
	switch trust {
	case TrustOwner, "":
		return ""
	case TrustTrusted:
		if fork {
			return "a fork job reached a trusted lent device"
		}
		if event == EventPullRequestTarget {
			return "a pull_request_target job reached a trusted lent device"
		}
		return ""
	case TrustPublic:
		if event != EventPullRequest {
			return "a " + event + " job reached a public lent device"
		}
		return ""
	}
	return "a job reached a device with trust " + trust
}

// LentSource is a WorkSource on a pool this device joined as a lent device
// (tentmaker join). Lend is the trust the device accepted at join; nil
// means the source is the device's own pool, where it is the owner.
type LentSource interface {
	Lend() *Lend
}

type callerLendKey struct{}

// WithCallerLend returns ctx carrying l, the lend of a guest node the mesh
// admitted (from this pool's guest table, never from the request).
func WithCallerLend(ctx context.Context, l Lend) context.Context {
	return context.WithValue(ctx, callerLendKey{}, l)
}

// CallerLend is the lend WithCallerLend put in ctx; ok is false for an
// owner-trust caller (this device, one of the owner's, or a member).
func CallerLend(ctx context.Context) (Lend, bool) {
	l, ok := ctx.Value(callerLendKey{}).(Lend)
	return l, ok
}
