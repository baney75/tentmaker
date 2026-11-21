package mesh

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

var fuzzPaths = []string{"/v1/work", "/v1/claim", "/v1/release"}

// FuzzHandlerBodies posts arbitrary bodies to every endpoint that decodes
// one: no panic, the answer is always a JSON object, a body over 1 MiB is
// 413, and a body that isn't one valid JSON value is a 400 with an error.
func FuzzHandlerBodies(f *testing.F) {
	for _, s := range []string{
		`{"device":"alice-linux","free":[{"platform":{"os":"linux","arch":"x64"},"labels":["self-hosted"],"slots":1}]}`,
		`{"job_key":"github:alice/app:42","device":"alice-linux","ttl_s":60}`,
		`{"job_key":"k","device":"d"}`,
		`{} x`, `{}{}`, `[]`, `null`, `"x"`, `{"ttl_s":1e999}`, `{"device":5}`, ``, `{`, "\xff",
		`{"job_key":"` + string(bytes.Repeat([]byte("a"), 600)) + `","device":"d","ttl_s":5}`,
	} {
		for i := range fuzzPaths {
			f.Add([]byte(s), uint8(i), false, false)
		}
	}
	f.Add([]byte(`{"device":"x"}`), uint8(0), true, false)
	f.Add([]byte(`{"job_key":"k","device":"d","ttl_s":5}`), uint8(1), true, true)
	f.Fuzz(func(t *testing.T, body []byte, which uint8, big, chunked bool) {
		path := fuzzPaths[int(which)%len(fuzzPaths)]
		if big {
			body = append(body, bytes.Repeat([]byte(" "), maxBody)...)
		}
		n := New(Options{Device: "alice-mac", Pool: "home", Tailscale: newFakeTS(), Broker: &fakeBroker{secret: []byte("s")}})
		knowPeer(n, "n2", "100.64.0.2", "x")
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		req.RemoteAddr = "100.64.0.2:40000"
		req.Header.Set(poolHeader, "home")
		if chunked {
			req.ContentLength = -1 // unknown length: the MaxBytesReader path
		}
		rec := httptest.NewRecorder()
		n.handler().ServeHTTP(rec, req)

		var obj map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &obj); err != nil {
			t.Fatalf("%s: answer %d is not a JSON object: %q", path, rec.Code, rec.Body.String())
		}
		switch {
		case len(body) > maxBody:
			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("%s: %d-byte body got %d, want 413", path, len(body), rec.Code)
			}
		case !json.Valid(body):
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s: invalid JSON %q got %d, want 400: %s", path, body, rec.Code, rec.Body.String())
			}
		}
		if rec.Code != http.StatusOK {
			if e, _ := obj["error"].(string); e == "" {
				t.Fatalf("%s: %d without an error message: %s", path, rec.Code, rec.Body.String())
			}
		}
		switch rec.Code {
		case http.StatusOK, http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusForbidden:
		default:
			t.Fatalf("%s: unexpected status %d: %s", path, rec.Code, rec.Body.String())
		}
	})
}

// FuzzParseStatus: no panic; a status it accepts has a self with an IP and
// no peer without an ID or host name it made up.
func FuzzParseStatus(f *testing.F) {
	if b, err := os.ReadFile("testdata/status.json"); err == nil {
		f.Add(b)
	}
	for _, s := range []string{`{}`, `{"BackendState":"Running"}`, `{"BackendState":"Running","Self":{"TailscaleIPs":["100.64.0.1/32"]},"Peer":{"x":null}}`,
		`{"BackendState":"Running","Self":{"TailscaleIPs":["junk"]}}`, `null`, `[]`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		self, peers, err := ParseStatus(b)
		if err != nil {
			return
		}
		if len(self.IPs) == 0 || !self.Online {
			t.Fatalf("accepted a self with no IP or offline: %+v", self)
		}
		for _, ip := range self.IPs {
			if !ip.IsValid() {
				t.Fatalf("self has an invalid IP: %+v", self)
			}
		}
		for i := 1; i < len(peers); i++ {
			a, c := peers[i-1], peers[i]
			if a.HostName > c.HostName || (a.HostName == c.HostName && a.ID > c.ID) {
				t.Fatalf("peers not sorted: %q/%q before %q/%q", a.HostName, a.ID, c.HostName, c.ID)
			}
		}
	})
}

// FuzzParseWhoIs: no panic; an identity it accepts names a user.
func FuzzParseWhoIs(f *testing.F) {
	if b, err := os.ReadFile("testdata/whois.json"); err == nil {
		f.Add(b)
	}
	for _, s := range []string{`{}`, `{"Node":{},"UserProfile":{}}`, `{"Node":{"User":5},"UserProfile":{}}`, `{"Node":null,"UserProfile":{"ID":1}}`, `null`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		id, err := ParseWhoIs(b)
		if err != nil {
			return
		}
		if id.UserID == 0 && id.LoginName == "" {
			t.Fatalf("accepted an identity with no user: %+v", id)
		}
	})
}
