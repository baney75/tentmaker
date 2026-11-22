package control

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"testing"

	"github.com/baney75/tentmaker/internal/core"
)

type prHandler struct {
	*fakeHandler
	got []core.PullRequest
}

func (h *prHandler) OpenPR(ctx context.Context, pr core.PullRequest) (core.PullRequestResult, error) {
	h.got = append(h.got, pr)
	return core.PullRequestResult{Number: 3, URL: "https://github.com/o/r/pull/3", Created: true}, nil
}

// POST /v1/pr needs the control token; with it the daemon opens the pull
// request and answers with its number, never a forge token.
func TestOpenPRNeedsTheToken(t *testing.T) {
	dir := t.TempDir()
	tok, err := NewToken(TokenPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h := &prHandler{fakeHandler: &fakeHandler{}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go NewServer(h, tok, func(net.Conn) (Caller, error) { return Caller{Who: "root", Admin: true}, nil }, nil).Serve(ctx, ln)

	pr := core.PullRequest{Repo: "o/r", Head: "tentmaker/migrate", Title: "t", Body: "b"}
	var res core.PullRequestResult
	noTok := &Client{Addr: ln.Addr().String(), TokenPath: filepath.Join(t.TempDir(), "none")}
	if err := noTok.Call(ctx, "/v1/pr", pr, &res); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("without the token file: %v", err)
	}
	c := &Client{Addr: ln.Addr().String(), TokenPath: TokenPath(dir)}
	if err := c.Call(ctx, "/v1/pr", pr, &res); err != nil {
		t.Fatal(err)
	}
	if res.Number != 3 || len(h.got) != 1 || h.got[0].Head != "tentmaker/migrate" {
		t.Fatalf("res %+v got %+v", res, h.got)
	}
	var ce *Error
	if err := c.Call(ctx, "/v1/pr", map[string]string{"repo": "o/r"}, &res); !errors.As(err, &ce) || ce.Status != 400 {
		t.Fatalf("no head: %v", err)
	}

	// A Handler without OpenPR serves no /v1/pr.
	_, plain, _ := start(t)
	if err := plain.Call(ctx, "/v1/pr", pr, &res); !errors.As(err, &ce) || ce.Status != 404 {
		t.Fatalf("plain handler: %v", err)
	}
}
