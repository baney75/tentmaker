package control

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/runs"
)

// RunsHandler serves run history. A Handler that also implements it gets
// these routes:
//
//	GET  /v1/runs?repo=&sha=&run_id=&failed=1&since=&limit=  open, like status
//	GET  /v1/runs/log?id=&max=                                open, like status
//
// The log is a redacted tail, open to local users so an agent's MCP server
// can read it without root; CI jobs are refused like every route.
//
//	POST /v1/rerun {"repo", "run_id"}                          token
//	POST /v1/alerts/test                                       token
type RunsHandler interface {
	Runs(ctx context.Context, q runs.Query) (runs.Answer, error)
	RunLog(ctx context.Context, id string, max int64) (runs.LogAnswer, error)
	Rerun(ctx context.Context, req RerunRequest) (RerunAnswer, error)
	AlertsTest(ctx context.Context) (any, error)
}

// RerunRequest is the body of POST /v1/rerun.
type RerunRequest struct {
	Repo  string `json:"repo"`
	RunID int64  `json:"run_id"`
}

// RerunAnswer says which broker asked the forge.
type RerunAnswer struct {
	Repo   string `json:"repo"`
	RunID  int64  `json:"run_id"`
	Broker string `json:"broker"`
}

func (s *Server) addRuns(h RunsHandler) {
	s.mux.HandleFunc("GET /v1/runs", func(w http.ResponseWriter, r *http.Request) {
		q, err := runs.ParseQuery(r.URL.Query())
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		v, err := h.Runs(r.Context(), q)
		s.replyErr(w, v, err)
	})
	s.mux.HandleFunc("GET /v1/runs/log", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		if !runs.ValidID(id) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad job id"})
			return
		}
		max := int64(runs.LogTailBytes)
		if v, err := strconv.ParseInt(r.URL.Query().Get("max"), 10, 64); err == nil && v > 0 && v < max {
			max = v
		}
		v, err := h.RunLog(r.Context(), id, max)
		s.replyErr(w, v, err)
	})
	s.mux.HandleFunc("POST /v1/rerun", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var req RerunRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&req); err != nil || req.Repo == "" || req.RunID <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": `body must be {"repo": "owner/name", "run_id": N}`})
			return
		}
		v, err := h.Rerun(r.Context(), req)
		s.replyErr(w, v, err)
	}))
	s.mux.HandleFunc("POST /v1/alerts/test", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := h.AlertsTest(r.Context())
		s.replyErr(w, v, err)
	}))
}

// replyErr is reply with core.ErrNotFound as 404.
func (s *Server) replyErr(w http.ResponseWriter, v any, err error) {
	if errors.Is(err, core.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	s.reply(w, v, err)
}

// PostFor sends body to path with the token and decodes the answer.
func (c *Client) PostFor(ctx context.Context, path string, body, out any) error {
	return c.do(ctx, http.MethodPost, path, body, out)
}
