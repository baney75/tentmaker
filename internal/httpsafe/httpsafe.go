// Package httpsafe keeps credentials on the origin they were meant for.
//
// Go's http.Client copies a request's headers to every redirect. It drops
// Authorization and Cookie when the redirect leaves the original host (but
// keeps them for its subdomains and on a scheme change), and it copies any
// other header, such as GitLab's PRIVATE-TOKEN, everywhere. A forge or a
// proxy in front of it that answers with a redirect to another origin
// would receive the token.
package httpsafe

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// CredentialHeaders are the request headers that carry a secret.
var CredentialHeaders = []string{
	"Authorization",
	"Proxy-Authorization",
	"Private-Token",
	"Job-Token",
	"Cookie",
}

// ErrCrossOrigin is returned (wrapped in a *url.Error by the client) when a
// request that carries a credential is redirected to another origin.
var ErrCrossOrigin = errors.New("refused a redirect to another origin for a request that carries a credential")

// maxRedirects matches net/http's default.
const maxRedirects = 10

// Origin is scheme://host:port with the default port filled in and the
// host lowercased.
func Origin(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" {
		switch scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}
	return scheme + "://" + net.JoinHostPort(host, port)
}

// SameOrigin reports whether a and b have the same origin.
func SameOrigin(a, b *url.URL) bool { return Origin(a) == Origin(b) }

// HasCredential reports whether h carries any credential header.
func HasCredential(h http.Header) bool {
	for _, k := range CredentialHeaders {
		if h.Get(k) != "" {
			return true
		}
	}
	return false
}

// CheckRedirect refuses a redirect to another origin when the original
// request carried a credential header, and stops after 10 redirects. A
// request without credentials follows redirects as usual.
func CheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return errors.New("stopped after 10 redirects")
	}
	if len(via) == 0 {
		return nil
	}
	first := via[0]
	if HasCredential(first.Header) && !SameOrigin(first.URL, req.URL) {
		return ErrCrossOrigin
	}
	return nil
}

// Guard returns a shallow copy of c (or of a new client when c is nil)
// whose redirects pass CheckRedirect first and then c's own policy.
func Guard(c *http.Client) *http.Client {
	var cp http.Client
	if c != nil {
		cp = *c
	}
	prev := cp.CheckRedirect
	cp.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if err := CheckRedirect(req, via); err != nil {
			return err
		}
		if prev != nil {
			return prev(req, via)
		}
		return nil
	}
	return &cp
}
