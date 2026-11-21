package dispatch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/httpsafe"
)

// The archive relay. A check's source archive is fetched by the broker that
// granted it, with the forge token, and streamed to the granted worker; the
// worker streams it on to the job over a loopback URL. The forge token, the
// forge's archive URL and any short-lived URL token stay in the broker.
const (
	// ArchiveTimeout bounds one archive fetch end to end.
	ArchiveTimeout = 10 * time.Minute
	// MaxArchiveBytes caps one archive.
	MaxArchiveBytes int64 = 2 << 30
	// MaxArchiveFetches is how often one check's archive may be fetched.
	MaxArchiveFetches = 3
	// DefaultRelayAddr is the daemon's relay listener, which serves GET
	// /v1/relay/<id> and nothing else (control.DefaultRelayAddr). The job
	// fence allows it; the control API refuses CI jobs by their account.
	DefaultRelayAddr = "127.0.0.1:7435"
)

// refPattern is the shape of an archive ref and a relay id: 32 random
// bytes, hex.
var refPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidRef reports whether s has the shape of an archive ref or relay id.
func ValidRef(s string) bool { return refPattern.MatchString(s) }

func newRef() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// archiveRec is one active check's archive, in memory only.
type archiveRec struct {
	runner  string
	device  string
	node    string // the tailnet node the check was granted to (grantRec.Node)
	forge   string
	repo    string
	sha     string
	fetches int
}

var errFetchLimit = fmt.Errorf("%w: this check's archive was fetched %d times already", core.ErrNotFound, MaxArchiveFetches)

// FetchArchive implements core.ArchiveSource. It serves ref only to the
// device its check was granted to, while that check is active, at most
// MaxArchiveFetches times, and fetches from the forge itself.
func (b *Broker) FetchArchive(ctx context.Context, ref, device string, w io.Writer) error {
	b.mu.Lock()
	b.pruneArchivesLocked()
	a := b.archives[ref]
	if a == nil {
		b.mu.Unlock()
		return core.ErrNotFound
	}
	if a.device != device {
		b.mu.Unlock()
		b.log.Warn("archive asked for by a device the check was not granted to; refused", "device", device, "runner", a.runner)
		return core.ErrNotFound
	}
	if a.node != core.CallerNode(ctx) {
		b.mu.Unlock()
		b.log.Warn("archive asked for by a tailnet node the check was not granted to; refused", "device", device, "runner", a.runner)
		return core.ErrNotFound
	}
	if a.fetches >= MaxArchiveFetches {
		b.mu.Unlock()
		return errFetchLimit
	}
	a.fetches++
	rec := *a
	b.mu.Unlock()

	sf := b.o.StatusForges[rec.forge]
	if sf == nil {
		return core.ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, ArchiveTimeout)
	defer cancel()
	u, header, err := sf.Archive(ctx, rec.repo, rec.sha)
	if err != nil {
		b.noteErr(rec.repo, "archive", err)
		return errors.New("the forge gave no source archive")
	}
	client := b.o.HTTP
	if client == nil {
		client = archiveClient()
	}
	start := b.now()
	n, err := downloadArchive(ctx, client, u, header, w)
	if err != nil {
		b.noteErr(rec.repo, "archive download", err)
		return fmt.Errorf("source archive: %w", err)
	}
	b.log.Info("source archive relayed", "runner", rec.runner, "device", rec.device, "repo", rec.repo,
		"sha", rec.sha, "bytes", n, "seconds", int(b.now().Sub(start).Seconds()))
	return nil
}

// addArchiveLocked records the archive of a check just granted and returns
// its ref.
func (b *Broker) addArchiveLocked(rec *grantRec) (string, error) {
	ref, err := newRef()
	if err != nil {
		return "", err
	}
	b.archives[ref] = &archiveRec{runner: rec.Name, device: rec.Device, node: rec.Node, forge: rec.Forge, repo: rec.Repo, sha: rec.SHA}
	return ref, nil
}

// pruneArchivesLocked drops the archives of checks that ended.
func (b *Broker) pruneArchivesLocked() {
	for ref, a := range b.archives {
		g := b.grants[a.runner]
		if g == nil || g.Kind != core.KindCheck || g.Device != a.device || g.Ended {
			delete(b.archives, ref)
		}
	}
}

// archiveClient has no overall timeout (a big archive takes a while); the
// caller's context bounds the fetch.
func archiveClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	t.TLSHandshakeTimeout = 30 * time.Second
	t.ResponseHeaderTimeout = time.Minute
	return &http.Client{Transport: t}
}

// downloadArchive streams the archive at rawURL to w, following redirects
// (GitHub's goes to codeload) and sending header only to the archive's own
// host. Errors never carry the URL or the header.
func downloadArchive(ctx context.Context, client *http.Client, rawURL, header string, w io.Writer) (int64, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return 0, errors.New("the forge's archive URL is not an http(s) URL")
	}
	var hName, hValue string
	if header != "" {
		n, v, ok := strings.Cut(header, ":")
		n = strings.TrimSpace(n)
		if !ok || n == "" || strings.ContainsAny(n, " \t\r\n") || strings.ContainsAny(v, "\r\n") {
			return 0, errors.New("the forge's archive header is malformed")
		}
		hName, hValue = n, strings.TrimSpace(v)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, errors.New("can't build the archive request")
	}
	if hName != "" {
		req.Header.Set(hName, hValue)
	}
	c := *client
	c.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		if hName != "" && !httpsafe.SameOrigin(r.URL, u) {
			r.Header.Del(hName)
		}
		return nil
	}
	resp, err := c.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("the forge answered HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > MaxArchiveBytes {
		return 0, errors.New("the archive is larger than 2 GiB")
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, MaxArchiveBytes+1))
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return n, err
	}
	if n > MaxArchiveBytes {
		return n, errors.New("the archive is larger than 2 GiB")
	}
	return n, nil
}

// ---------------------------------------------------------------- worker side

// relayBundle turns a check bundle's ArchiveRef into a loopback relay URL
// bound to (source, ref, runner) and returns the rewritten bundle. A bundle
// with a download header or without a ref came from a broker that hands
// out forge credentials, and is refused rather than passed to a sandbox.
func (w *Worker) relayBundle(src core.WorkSource, runner string, secret []byte, now time.Time) ([]byte, error) {
	var b core.CheckBundle
	if err := json.Unmarshal(secret, &b); err != nil {
		return nil, errors.New("the check bundle is not valid JSON")
	}
	if b.Header != "" || b.ArchiveRef == "" {
		return nil, errors.New("the broker sent the source archive's location instead of a ref (an older broker); update it, since the job would see its credentials")
	}
	if !ValidRef(b.ArchiveRef) {
		return nil, errors.New("the check bundle's archive ref is malformed")
	}
	as, ok := src.(core.ArchiveSource)
	if !ok {
		return nil, errors.New("this broker can't serve source archives")
	}
	addr := w.o.RelayAddr()
	if addr == "" {
		return nil, errors.New("the control API is not serving, so the job can't get its source")
	}
	id, err := newRef()
	if err != nil {
		return nil, err
	}
	life := w.o.Config.Job.MaxDuration.D()
	if life <= 0 {
		life = 3 * time.Hour
	}
	e := &relayEntry{source: as, ref: b.ArchiveRef, runner: runner, expires: now.Add(life + overrunGrace)}
	b.ArchiveURL = "http://" + addr + "/v1/relay/" + id
	b.Header, b.ArchiveRef = "", ""
	out, err := json.Marshal(b)
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	w.relays[id] = e
	w.mu.Unlock()
	return out, nil
}

func (w *Worker) dropRelaysLocked(runner string) {
	for id, e := range w.relays {
		if e.runner == runner {
			delete(w.relays, id)
		}
	}
}

// Relay streams the source archive of the running check job that owns id.
// An unknown or expired id is core.ErrNotFound.
func (w *Worker) Relay(ctx context.Context, id string, out io.Writer) error {
	if !ValidRef(id) {
		return core.ErrNotFound
	}
	now := w.now()
	w.mu.Lock()
	e := w.relays[id]
	if e != nil && now.After(e.expires) {
		delete(w.relays, id)
		e = nil
	}
	w.mu.Unlock()
	if e == nil {
		return core.ErrNotFound
	}
	err := e.source.FetchArchive(ctx, e.ref, w.device, out)
	if err != nil && !errors.Is(err, core.ErrNotFound) {
		w.noteErr("relay", "source archive for "+e.runner, err)
	}
	return err
}
