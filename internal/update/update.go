// Package update finds, verifies and installs new Tentmaker releases.
//
// Channels:
//   - stable: GitHub releases tagged vX.Y.Z that are not prereleases.
//   - dev: one rolling prerelease tagged "dev". Its release name is the
//     version, X.Y.Z-dev.<UTC yyyymmddHHMM>.<sha7>.
//
// Every release has tentmaker_<os>_<arch>.tar.gz (.zip on Windows) and
// SHA256SUMS. Download refuses an archive whose sha256 doesn't match before
// it extracts anything. Apply runs the new binary's health check before it
// swaps, and keeps the old binary as <bin>.prev for Rollback.
package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// Channel names.
const (
	Stable = "stable"
	Dev    = "dev"
)

// DevTag is the git tag of the rolling dev prerelease.
const DevTag = "dev"

// SumsName is the checksum file every release carries.
const SumsName = "SHA256SUMS"

// Release is one published release on a channel.
type Release struct {
	Channel   string            `json:"channel"`
	Tag       string            `json:"tag"`
	Version   string            `json:"version"` // no leading "v"
	Published time.Time         `json:"published"`
	Assets    map[string]string `json:"assets"` // file name -> download URL
	URL       string            `json:"url"`    // release page
}

// ArchiveName is the release asset for goos/goarch.
func ArchiveName(goos, goarch string) string {
	if goos == "windows" {
		return "tentmaker_" + goos + "_" + goarch + ".zip"
	}
	return "tentmaker_" + goos + "_" + goarch + ".tar.gz"
}

// Options configures an Updater. Zero values get safe defaults.
type Options struct {
	APIBase string       // default https://api.github.com
	Repo    string       // default baney75/tentmaker
	HTTP    *http.Client // default: dial, TLS and header timeouts
	// APITimeout bounds each API call (default 30 s); DownloadTimeout bounds
	// each asset download (default 10 min).
	APITimeout      time.Duration
	DownloadTimeout time.Duration
	// HealthTimeout bounds `<new> version --json` (default 10 s).
	HealthTimeout time.Duration
	GOOS, GOARCH  string // default runtime values; tests set windows to test the swap
	UserAgent     string // default "tentmaker-updater"
	// MaxArchive caps the archive download (default 256 MiB).
	MaxArchive int64
}

// Updater talks to the release host. Safe for concurrent use.
type Updater struct{ o Options }

// New returns an Updater with defaults filled in.
func New(o Options) *Updater {
	if o.APIBase == "" {
		o.APIBase = "https://api.github.com"
	}
	o.APIBase = strings.TrimRight(o.APIBase, "/")
	if o.Repo == "" {
		o.Repo = "baney75/tentmaker"
	}
	if o.HTTP == nil {
		o.HTTP = defaultHTTP()
	}
	if o.APITimeout <= 0 {
		o.APITimeout = 30 * time.Second
	}
	if o.DownloadTimeout <= 0 {
		o.DownloadTimeout = 10 * time.Minute
	}
	if o.HealthTimeout <= 0 {
		o.HealthTimeout = 10 * time.Second
	}
	if o.GOOS == "" {
		o.GOOS = runtime.GOOS
	}
	if o.GOARCH == "" {
		o.GOARCH = runtime.GOARCH
	}
	if o.UserAgent == "" {
		o.UserAgent = "tentmaker-updater"
	}
	if o.MaxArchive <= 0 {
		o.MaxArchive = 256 << 20
	}
	return &Updater{o: o}
}

func defaultHTTP() *http.Client {
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       60 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	// No overall Timeout here: each call sets its own deadline on the context.
	return &http.Client{Transport: tr}
}

// Default is the Updater the package-level functions use.
var Default = New(Options{})

// Check finds the newest release on channel and reports whether it is newer
// than current.
func Check(ctx context.Context, channel, current string) (Release, bool, error) {
	return Default.Check(ctx, channel, current)
}

// Download fetches rel's archive for this OS into dir, verifies it and
// returns the extracted binary's path.
func Download(ctx context.Context, rel Release, dir string) (string, error) {
	return Default.Download(ctx, rel, dir)
}

// Apply health-checks newBin and swaps it in place of curBin.
func Apply(ctx context.Context, newBin, curBin, wantVersion string) error {
	return Default.Apply(ctx, newBin, curBin, wantVersion)
}

// Rollback puts <curBin>.prev back in place of curBin.
func Rollback(curBin string) error { return Default.Rollback(curBin) }

type ghAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

type ghRelease struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	HTMLURL     string    `json:"html_url"`
	Assets      []ghAsset `json:"assets"`
}

var stableTag = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

// Check finds the newest release on channel and reports whether it is newer
// than current. A current version that doesn't parse counts as older.
func (u *Updater) Check(ctx context.Context, channel, current string) (Release, bool, error) {
	var rel Release
	switch channel {
	case Stable:
		var list []ghRelease
		if err := u.getJSON(ctx, "/repos/"+u.o.Repo+"/releases?per_page=100", &list); err != nil {
			return Release{}, false, err
		}
		var best *ghRelease
		var bestV Version
		for i := range list {
			r := &list[i]
			if r.Draft || r.Prerelease || !stableTag.MatchString(r.TagName) {
				continue
			}
			v, err := ParseVersion(r.TagName)
			if err != nil {
				continue
			}
			if best == nil || v.Compare(bestV) > 0 {
				best, bestV = r, v
			}
		}
		if best == nil {
			return Release{}, false, fmt.Errorf("no stable release of %s yet; try --channel dev", u.o.Repo)
		}
		rel = toRelease(Stable, *best, bestV.String())
	case Dev:
		var r ghRelease
		if err := u.getJSON(ctx, "/repos/"+u.o.Repo+"/releases/tags/"+DevTag, &r); err != nil {
			return Release{}, false, err
		}
		name := strings.TrimPrefix(strings.TrimSpace(r.Name), "tentmaker ")
		v, err := ParseVersion(name)
		if err != nil {
			return Release{}, false, fmt.Errorf("dev release name %q is not a version: %v", r.Name, err)
		}
		rel = toRelease(Dev, r, v.String())
	default:
		return Release{}, false, fmt.Errorf("unknown channel %q: use %q or %q", channel, Stable, Dev)
	}
	newer, err := Newer(rel.Version, current)
	if err != nil {
		return Release{}, false, err
	}
	return rel, newer, nil
}

func toRelease(channel string, r ghRelease, version string) Release {
	rel := Release{
		Channel:   channel,
		Tag:       r.TagName,
		Version:   version,
		Published: r.PublishedAt,
		Assets:    make(map[string]string, len(r.Assets)),
		URL:       r.HTMLURL,
	}
	for _, a := range r.Assets {
		rel.Assets[a.Name] = a.URL
	}
	return rel
}

func (u *Updater) getJSON(ctx context.Context, path string, v any) error {
	ctx, cancel := context.WithTimeout(ctx, u.o.APITimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.o.APIBase+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", u.o.UserAgent)
	resp, err := u.o.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("can't reach the release server: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("reading %s: %w", redact(req.URL), err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%s: not found (no release published on this channel yet?)", redact(req.URL))
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
		return fmt.Errorf("%s: HTTP %d (GitHub rate limit; try again in an hour)", redact(req.URL), resp.StatusCode)
	case resp.StatusCode/100 != 2:
		return fmt.Errorf("%s: HTTP %d", redact(req.URL), resp.StatusCode)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("%s: bad JSON: %w", redact(req.URL), err)
	}
	return nil
}

// redact drops the query string and any user info so a signed URL never
// lands in an error message.
func redact(u *url.URL) string {
	c := *u
	c.RawQuery, c.User, c.Fragment = "", nil, ""
	return c.String()
}

var errNoAsset = errors.New("release has no asset")
