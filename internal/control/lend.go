package control

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

// LendHandler is a Handler that lends devices to a pool (docs/DESIGN.md,
// Mesh: lent devices). It is optional: a Handler without it serves none of
// these routes. Every POST needs the token; an invite code travels only in
// a request or answer body on the loopback API, never in a URL.
type LendHandler interface {
	// Invite makes a one-time code; the answer carries it once.
	Invite(ctx context.Context, req InviteRequest) (any, error)
	// Join lends this device to the pool whose code it is given.
	Join(ctx context.Context, code string) (any, error)
	// Leave drops this device out of the pool it joined.
	Leave(ctx context.Context) (any, error)
	// Devices lists the pool's devices, lent ones included.
	Devices(ctx context.Context) (any, error)
	// RemoveDevice revokes a lent device at once.
	RemoveDevice(ctx context.Context, name string) (any, error)
}

// InviteRequest is the body of POST /v1/invite.
type InviteRequest struct {
	Trust string   `json:"trust"`
	Repos []string `json:"repos,omitempty"`
	TTLS  int64    `json:"ttl_s,omitempty"` // 0 = the default
}

// JoinRequest is the body of POST /v1/join.
type JoinRequest struct {
	Code string `json:"code"`
}

// RemoveDeviceRequest is the body of POST /v1/devices/remove.
type RemoveDeviceRequest struct {
	Name string `json:"name"`
}

// JoinTimeout bounds a join: the host's WhoIs and its answer over the tailnet.
const JoinTimeout = 45 * time.Second

func (s *Server) lendRoutes(h LendHandler) {
	s.mux.HandleFunc("POST /v1/invite", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var req InviteRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&req); err != nil || req.TTLS < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be {\"trust\": \"trusted|public\", \"repos\": [...], \"ttl_s\": N}"})
			return
		}
		v, err := h.Invite(r.Context(), req)
		s.reply(w, v, err)
	}))
	s.mux.HandleFunc("POST /v1/join", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var req JoinRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil || req.Code == "" {
			// The body may hold a code: never echo it.
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be {\"code\": \"...\"}"})
			return
		}
		v, err := h.Join(r.Context(), req.Code)
		s.reply(w, v, err)
	}))
	s.mux.HandleFunc("POST /v1/leave", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := h.Leave(r.Context())
		s.reply(w, v, err)
	}))
	s.mux.HandleFunc("GET /v1/devices", func(w http.ResponseWriter, r *http.Request) {
		v, err := h.Devices(r.Context())
		s.reply(w, v, err)
	})
	s.mux.HandleFunc("POST /v1/devices/remove", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var req RemoveDeviceRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&req); err != nil || req.Name == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be {\"name\": \"device\"}"})
			return
		}
		v, err := h.RemoveDevice(r.Context(), req.Name)
		s.reply(w, v, err)
	}))
}

// ErrMeshOff is the lend routes' answer when the mesh is off.
var ErrMeshOff = errors.New("the mesh is off (mesh.enabled = false) or tailscale is not up")

// CallFor is Call with a time limit other than ClientTimeout.
func (c *Client) CallFor(ctx context.Context, timeout time.Duration, path string, body, out any) error {
	return c.doFor(ctx, timeout, http.MethodPost, path, body, out)
}
