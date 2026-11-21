package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/baney75/tentmaker/internal/forge/github"
)

// verifyTimeout bounds the one API call that checks a new token.
const verifyTimeout = 30 * time.Second

// userURL is the "who am I" endpoint for a forge kind and its configured URL.
func userURL(kind, base string) (string, error) {
	base = strings.TrimRight(base, "/")
	switch kind {
	case "github":
		if base == "" {
			base = github.DefaultBaseURL
		}
		return base + "/user", nil
	case "forgejo":
		if base == "" {
			return "", errors.New("Forgejo needs --url")
		}
		if !strings.HasSuffix(base, "/api/v1") {
			base += "/api/v1"
		}
		return base + "/user", nil
	case "gitlab":
		if base == "" {
			base = "https://gitlab.com"
		}
		if !strings.Contains(base, "/api/v4") {
			base += "/api/v4"
		}
		return base + "/user", nil
	}
	return "", fmt.Errorf("unknown forge %q", kind)
}

// verifyToken makes one API call with token and returns the login it
// belongs to. The token goes only into the Authorization header; errors
// carry the HTTP status, never the token or the response body.
func verifyToken(ctx context.Context, hc *http.Client, kind, base, token string) (string, error) {
	u, err := userURL(kind, base)
	if err != nil {
		return "", err
	}
	if hc == nil {
		hc = httpClient()
	}
	ctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", errors.New("bad forge URL")
	}
	switch kind {
	case "gitlab":
		req.Header.Set("PRIVATE-TOKEN", token)
	case "forgejo":
		req.Header.Set("Authorization", "token "+token)
	default:
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/vnd.github+json")
	}
	req.Header.Set("User-Agent", "tentmaker")
	resp, err := hc.Do(req)
	if err != nil {
		var ue interface{ Unwrap() error }
		if errors.As(err, &ue) && ue.Unwrap() != nil {
			err = ue.Unwrap()
		}
		return "", fmt.Errorf("GET %s: %v", u, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return "", errors.New("the forge says the token is not valid (401)")
	case resp.StatusCode == http.StatusForbidden:
		return "", errors.New("the forge refused the token (403); check its permissions")
	case resp.StatusCode/100 != 2:
		return "", fmt.Errorf("GET %s: HTTP %d", u, resp.StatusCode)
	}
	var who struct {
		Login    string `json:"login"`
		Username string `json:"username"`
	}
	if err := json.Unmarshal(body, &who); err != nil {
		return "", fmt.Errorf("GET %s: the answer is not JSON", u)
	}
	return firstNonEmpty(who.Login, who.Username), nil
}
