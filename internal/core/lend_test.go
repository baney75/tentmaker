package core

import "testing"

func TestLendRefusal(t *testing.T) {
	trusted := Lend{Trust: TrustTrusted, Repos: []string{"alice/app"}}
	all := Lend{Trust: TrustTrusted}
	public := Lend{Trust: TrustPublic}
	job := func(repo, event string, fork bool) Job { return Job{Repo: repo, Event: event, FromFork: fork} }
	for _, c := range []struct {
		name   string
		l      Lend
		off    WorkOffer
		refuse bool
	}{
		{"owner anything", Lend{Trust: TrustOwner}, WorkOffer{Job: job("x/y", "pull_request", true)}, false},
		{"trusted listed vouched", trusted, WorkOffer{Job: job("Alice/App", "push", false), Cache: true}, false},
		{"trusted unlisted", trusted, WorkOffer{Job: job("alice/site", "push", false), Cache: true}, true},
		{"trusted empty list means all", all, WorkOffer{Job: job("alice/site", "push", false), Cache: true}, false},
		{"trusted fork", trusted, WorkOffer{Job: job("alice/app", "pull_request", true), Cache: true}, true},
		{"trusted pull_request_target", trusted, WorkOffer{Job: job("alice/app", EventPullRequestTarget, false), Cache: true}, true},
		{"trusted not vouched", trusted, WorkOffer{Job: job("alice/app", "push", false)}, true},
		{"public fork pr", public, WorkOffer{Job: job("alice/pub", EventPullRequest, true)}, false},
		{"public push", public, WorkOffer{Job: job("alice/pub", "push", false)}, true},
		{"public fork prt", public, WorkOffer{Job: job("alice/pub", EventPullRequestTarget, true)}, true},
		{"public with cache", public, WorkOffer{Job: job("alice/pub", EventPullRequest, true), Cache: true}, true},
		{"public with actions cache", public, WorkOffer{Job: job("alice/pub", EventPullRequest, true), Actions: &ActionsOffer{}}, true},
		{"public check job", public, WorkOffer{Job: job("alice/pub", KindCheck, true)}, true},
		{"revoked", Lend{Trust: TrustRevoked}, WorkOffer{Job: job("alice/app", "push", false), Cache: true}, true},
		{"unknown trust", Lend{Trust: "root"}, WorkOffer{Job: job("alice/app", "push", false), Cache: true}, true},
	} {
		if got := LendRefusal(c.l, c.off) != ""; got != c.refuse {
			t.Errorf("%s: refused %v, want %v", c.name, got, c.refuse)
		}
	}
}
