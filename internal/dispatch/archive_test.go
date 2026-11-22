package dispatch

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/core"
	fakeforge "github.com/baney75/tentmaker/internal/forge/fake"
	"github.com/baney75/tentmaker/internal/forge/forgejo"
	"github.com/baney75/tentmaker/internal/forge/github"
	"github.com/baney75/tentmaker/internal/forge/gitlab"
)

// archiveVia is the fake forge with a real forge client's Archive, so the
// broker sees exactly the URL and header each real forge hands out.
type archiveVia struct {
	*fakeforge.Forge
	real core.StatusForge
}

func (a archiveVia) Archive(ctx context.Context, repo, sha string) (string, string, error) {
	return a.real.Archive(ctx, repo, sha)
}

const shortLived = "codeload-short-lived-token"

var srcTarball = []byte("\x1f\x8b fake tarball bytes for the relay")

// forgeServer serves archives the way each forge does: only with the
// forge token in its header (GitHub's redirect: only with the short-lived
// token in the URL).
func forgeServer(t *testing.T, kind, token string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasPrefix(p, "/codeload/"):
			if r.URL.Query().Get("token") != shortLived || r.Header.Get("Authorization") != "" {
				http.Error(w, "bad codeload token", http.StatusForbidden)
				return
			}
			w.Write(srcTarball)
			return
		case strings.Contains(p, "/tarball/") && kind == "github-redirect":
			http.Redirect(w, r, srv.URL+"/codeload/"+strings.TrimPrefix(p, "/repos/")+"?token="+shortLived, http.StatusFound)
			return
		}
		want := map[string]string{
			"github":  "Authorization: Bearer " + token,
			"forgejo": "Authorization: token " + token,
			"gitlab":  "PRIVATE-TOKEN: " + token,
		}[kind]
		name, value, _ := strings.Cut(want, ": ")
		if r.Header.Get(name) != value {
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}
		w.Write(srcTarball)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func realArchiveForge(t *testing.T, kind, token string) (core.StatusForge, []string) {
	t.Helper()
	srv := forgeServer(t, kind, token)
	tok := func(context.Context) (string, error) { return token, nil }
	secrets := []string{token}
	switch kind {
	case "github", "github-redirect":
		secrets = append(secrets, shortLived)
		return github.New(github.Options{BaseURL: srv.URL, Token: tok}), secrets
	case "forgejo":
		return forgejo.New(forgejo.Options{BaseURL: srv.URL, Token: tok, Flavor: "forgejo"}), secrets
	case "gitlab":
		return gitlab.New(gitlab.Options{BaseURL: srv.URL + "/api/v4", Token: tok}), secrets
	}
	t.Fatalf("unknown kind %s", kind)
	return nil, nil
}

// The bundle a worker receives, which travels into the sandbox, carries no
// forge token for any forge kind.
func TestCheckBundleCarriesNoForgeToken(t *testing.T) {
	for _, kind := range []string{"github", "github-redirect", "forgejo", "gitlab"} {
		t.Run(kind, func(t *testing.T) {
			token := "forge-token-" + strings.ReplaceAll(kind, "-", "") + "-SECRET"
			real, secrets := realArchiveForge(t, kind, token)
			r := newRig(t, checksRepo(repoSite))
			sf := archiveVia{Forge: r.forge, real: real}
			r.broker = NewBroker(BrokerOptions{
				Config:       r.cfg,
				Forges:       map[string]core.Forge{"github": r.forge},
				StatusForges: map[string]core.StatusForge{"github": sf},
				Planner:      r.planner.plan,
				StateDir:     r.bdir,
				Log:          r.log,
				Now:          r.clk.Now,
			})
			w := r.addWorker("alpha", 1)
			r.forge.SetFile(repoSite, sha1, ciWorkflow, []byte("test\n"))
			r.forge.SetCommits(repoSite, []core.Commit{{SHA: sha1, Ref: "main"}})
			r.tick()
			names := started(w.ex)
			if len(names) != 1 {
				t.Fatalf("starts = %v", names)
			}
			spec, _ := w.ex.Spec(names[0])
			for _, s := range secrets {
				if bytes.Contains(spec.Grant.Secret, []byte(s)) {
					t.Fatalf("the bundle the worker got carries a forge secret %q:\n%s", s, spec.Grant.Secret)
				}
			}
			b := bundleOf(t, spec)
			if b.Header != "" {
				t.Fatalf("the bundle carries a download header")
			}
			if !strings.HasPrefix(b.ArchiveURL, "http://127.0.0.1:") || !strings.Contains(b.ArchiveURL, "/v1/relay/") {
				t.Fatalf("the archive URL is not the loopback relay: %s", b.ArchiveURL)
			}
			// The relay streams the forge's tarball, fetched by the broker.
			id := b.ArchiveURL[strings.LastIndex(b.ArchiveURL, "/")+1:]
			var buf bytes.Buffer
			if err := w.w.Relay(bg, id, &buf); err != nil {
				t.Fatalf("relay: %v", err)
			}
			if !bytes.Equal(buf.Bytes(), srcTarball) {
				t.Fatalf("relay streamed %q", buf.Bytes())
			}
			logs := r.logs.String()
			for _, s := range secrets {
				if strings.Contains(logs, s) {
					t.Fatalf("a forge secret is in the logs")
				}
			}
		})
	}
}

// startOneCheck grants one check to worker alpha and returns its bundle as
// the broker sent it (before the worker's rewrite) and the worker's.
func startOneCheck(t *testing.T) (*rig, *wrig, *source, core.CheckBundle) {
	t.Helper()
	r := newRig(t, checksRepo(repoSite))
	w := r.addWorker("alpha", 2)
	src := &source{inner: r.broker}
	w.sources = []core.WorkSource{src}
	r.forge.SetFile(repoSite, sha1, ciWorkflow, []byte("test\n"))
	r.forge.SetCommits(repoSite, []core.Commit{{SHA: sha1, Ref: "main"}})
	r.tick()
	names := started(w.ex)
	if len(names) != 1 {
		t.Fatalf("starts = %v", names)
	}
	spec, _ := w.ex.Spec(names[0])
	return r, w, src, bundleOf(t, spec)
}

// brokerRef is the archive ref the broker handed out for the one check.
func brokerRef(t *testing.T, r *rig) string {
	t.Helper()
	r.broker.mu.Lock()
	defer r.broker.mu.Unlock()
	if len(r.broker.archives) != 1 {
		t.Fatalf("broker has %d archive records, want 1", len(r.broker.archives))
	}
	for ref := range r.broker.archives {
		return ref
	}
	return ""
}

func TestArchiveRefOnlyForItsDevice(t *testing.T) {
	r, _, _, _ := startOneCheck(t)
	ref := brokerRef(t, r)
	var buf bytes.Buffer
	if err := r.broker.FetchArchive(bg, ref, "mallory", &buf); !errors.Is(err, core.ErrNotFound) || buf.Len() != 0 {
		t.Fatalf("another device fetched the archive: err=%v bytes=%d", err, buf.Len())
	}
	if err := r.broker.FetchArchive(bg, strings.Repeat("ab", 32), "alpha", &buf); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("unknown ref: %v", err)
	}
	for i := 0; i < MaxArchiveFetches; i++ {
		buf.Reset()
		if err := r.broker.FetchArchive(bg, ref, "alpha", &buf); err != nil || buf.Len() == 0 {
			t.Fatalf("fetch %d: %v", i+1, err)
		}
	}
	if err := r.broker.FetchArchive(bg, ref, "alpha", &buf); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("fetch past the limit: %v", err)
	}
}

func TestArchiveRefDroppedWhenCheckEnds(t *testing.T) {
	r, w, _, b := startOneCheck(t)
	ref := brokerRef(t, r)
	id := relayID(t, b)
	name := started(w.ex)[0]
	w.ex.SetResult(name, core.CheckResult{State: "success", Description: "ok"})
	w.ex.Exit(name)
	r.run(2*time.Second, 2*time.Second)
	if got := r.forge.Status(repoSite, sha1, "tentmaker/test"); got != "success" {
		t.Fatalf("status = %q", got)
	}
	var buf bytes.Buffer
	if err := r.broker.FetchArchive(bg, ref, "alpha", &buf); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("broker still serves an ended check's archive: %v", err)
	}
	if err := w.w.Relay(bg, id, &buf); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("worker still relays an ended job's archive: %v", err)
	}
}

func TestRelayUnknownOrExpiredID(t *testing.T) {
	r, w, _, b := startOneCheck(t)
	var buf bytes.Buffer
	for _, id := range []string{strings.Repeat("0", 64), "nope", "../../etc/passwd", ""} {
		if err := w.w.Relay(bg, id, &buf); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("relay %q: %v", id, err)
		}
	}
	// A restarted worker forgets every relay.
	id := relayID(t, b)
	w.w = r.newWorker(w)
	if err := w.w.Relay(bg, id, &buf); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("a restarted worker relays: %v", err)
	}
}

func TestRelayExpiresWithTheJob(t *testing.T) {
	r, w, _, b := startOneCheck(t)
	id := relayID(t, b)
	r.clk.Add(r.cfg.Job.MaxDuration.D() + overrunGrace + time.Second)
	var buf bytes.Buffer
	if err := w.w.Relay(bg, id, &buf); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("relay past the job's life: %v", err)
	}
}

// A bundle with a download header, as an older broker sends, never reaches
// a sandbox.
func TestWorkerRefusesBundleWithCredentials(t *testing.T) {
	r := newRig(t)
	w := r.addWorker("alpha", 2)
	secret := []byte(`{"repo":"o/r","sha":"` + sha1 + `","archive_url":"https://forge.example/a.tar.gz","header":"Authorization: token SECRET","job":{"name":"t"}}`)
	off := core.WorkOffer{Job: core.Job{Repo: "o/r", Event: CheckEvent}, Platform: linuxX64,
		Grant: core.Grant{RunnerName: "tm-alpha-0123abcd", Secret: secret}}
	if w.w.start(bg, &source{inner: r.broker}, off) {
		t.Fatal("started a check whose bundle carries a forge header")
	}
	if n := len(started(w.ex)); n != 0 {
		t.Fatalf("%d starts", n)
	}
}

// A device name is only a claim: the broker binds each grant to the tailnet
// node that asked for it. A node that later announces the same name (the
// first dropped out of discovery) can't report the grant's result, fetch
// its archive or get work as that device while the grant is on record, a
// restart included; the first node still can (review r2 partS sol-S2-F2).
func TestGrantsBoundToTheirTailnetNode(t *testing.T) {
	r := newRig(t, checksRepo(repoSite))
	w := r.addWorker("beta", 2)
	src := &source{inner: r.broker, node: "nBETA"}
	w.sources = []core.WorkSource{src}
	r.forge.SetFile(repoSite, sha1, ciWorkflow, []byte("test\n"))
	r.forge.SetCommits(repoSite, []core.Commit{{SHA: sha1, Ref: "main"}})
	r.tick()
	names := started(w.ex)
	if len(names) != 1 {
		t.Fatalf("starts = %v", names)
	}
	name, ref := names[0], brokerRef(t, r)
	grant := func() *grantRec {
		r.broker.mu.Lock()
		defer r.broker.mu.Unlock()
		return r.broker.grants[name]
	}
	if g := grant(); g == nil || g.Node != "nBETA" {
		t.Fatalf("grant = %+v, want node nBETA", g)
	}

	impostor := core.WithCallerNode(bg, "nOTHER")
	report := core.WorkRequest{Device: "beta", Free: 2, Platforms: []core.Platform{linuxX64},
		Results: []core.CheckResult{{Runner: name, State: "failure", Description: "forged"}}}
	if _, err := r.broker.Work(impostor, report); !errors.Is(err, core.ErrOtherNode) {
		t.Fatalf("another node's report as beta: %v", err)
	}
	if got := r.forge.Status(repoSite, sha1, "tentmaker/test"); got != "pending" {
		t.Fatalf("status after the forged report = %q, want pending", got)
	}
	if grant() == nil {
		t.Fatal("the forged report removed the grant")
	}
	var buf bytes.Buffer
	for _, ctx := range []context.Context{impostor, bg} {
		if err := r.broker.FetchArchive(ctx, ref, "beta", &buf); !errors.Is(err, core.ErrNotFound) || buf.Len() != 0 {
			t.Fatalf("archive for node %q: err=%v bytes=%d", core.CallerNode(ctx), err, buf.Len())
		}
	}
	if err := src.FetchArchive(bg, ref, "beta", &buf); err != nil || buf.Len() == 0 {
		t.Fatalf("archive for the granted node: err=%v bytes=%d", err, buf.Len())
	}

	// The binding is saved with the grant.
	r.broker = r.newBroker(r.cfg, r.bdir, nil)
	src.inner = r.broker
	if _, err := r.broker.Work(impostor, report); !errors.Is(err, core.ErrOtherNode) {
		t.Fatalf("after a restart, another node's report as beta: %v", err)
	}

	// The granted node reports its own result.
	w.ex.SetResult(name, core.CheckResult{State: "success", Description: "ok"})
	w.ex.Exit(name)
	r.run(2*time.Second, 2*time.Second)
	if got := r.forge.Status(repoSite, sha1, "tentmaker/test"); got != "success" {
		t.Fatalf("status = %q, want success", got)
	}
	// With none of beta's grants on record, the name is free again.
	if _, err := r.broker.Work(impostor, core.WorkRequest{Device: "beta"}); err != nil {
		t.Fatalf("a new node as beta once its grants ended: %v", err)
	}
}
