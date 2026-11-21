package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The test binary doubles as a fake tentmaker for the health check.
func TestMain(m *testing.M) {
	switch os.Getenv("TENTMAKER_UPDATE_FAKE") {
	case "ok":
		fmt.Printf("{\"version\":%q}\n", os.Getenv("TENTMAKER_UPDATE_FAKE_VERSION"))
		os.Exit(0)
	case "hang":
		time.Sleep(time.Minute)
		os.Exit(0)
	case "fail":
		os.Exit(3)
	}
	os.Exit(m.Run())
}

func TestParseAndCompare(t *testing.T) {
	order := []string{
		"0.0.0-dev",
		"0.1.0-dev.202611200400.abc1234",
		"0.1.0-dev.202611210400.0000000",
		"0.1.0-dev.202611210400.abc1234",
		"0.1.0",
		"0.1.1-dev.202601010000.aaaaaaa",
		"0.1.1",
		"0.2.0",
		"0.10.0",
		"1.0.0",
	}
	for i := range order {
		for j := range order {
			a, err := ParseVersion(order[i])
			if err != nil {
				t.Fatal(err)
			}
			b, err := ParseVersion(order[j])
			if err != nil {
				t.Fatal(err)
			}
			want := cmpInt(int64(i), int64(j))
			if got := a.Compare(b); got != want {
				t.Errorf("Compare(%s, %s) = %d, want %d", order[i], order[j], got, want)
			}
		}
	}
	for _, bad := range []string{"", "1", "1.2", "1.2.x", "1.2.3-", "1.2.3-a..b", "a.b.c",
		"0.0.0-0\x83\x9e +0", "1.2.3-a b", "1.2.3-a_b", "1.2.3+", "1.2.3+a..b", "1.2.3+a b"} {
		if _, err := ParseVersion(bad); err == nil {
			t.Errorf("ParseVersion(%q) accepted", bad)
		}
	}
	v, err := ParseVersion("v1.2.3+build.5")
	if err != nil || v.String() != "1.2.3" {
		t.Errorf("v1.2.3+build.5 = %v, %v", v, err)
	}
	if n, _ := Newer("0.1.0", "hand-build"); !n {
		t.Error("an unparsable current version should count as older")
	}
}

type fakeRelease struct {
	TagName     string              `json:"tag_name"`
	Name        string              `json:"name"`
	Draft       bool                `json:"draft"`
	Prerelease  bool                `json:"prerelease"`
	PublishedAt time.Time           `json:"published_at"`
	Assets      []map[string]string `json:"assets"`
}

func apiServer(t *testing.T, list []fakeRelease, dev *fakeRelease) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			t.Error("request without a User-Agent")
		}
		switch r.URL.Path {
		case "/repos/o/r/releases":
			json.NewEncoder(w).Encode(list)
		case "/repos/o/r/releases/tags/dev":
			if dev == nil {
				http.NotFound(w, r)
				return
			}
			json.NewEncoder(w).Encode(dev)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCheckStable(t *testing.T) {
	list := []fakeRelease{
		{TagName: "dev", Name: "0.11.0-dev.202611210400.abc1234", Prerelease: true},
		{TagName: "v0.9.1"},
		{TagName: "v0.10.0"},
		{TagName: "v0.12.0", Draft: true},
		{TagName: "v0.13.0", Prerelease: true},
		{TagName: "v0.11.0-rc.1"},
		{TagName: "nightly"},
		{TagName: "v0.2.0"},
	}
	srv := apiServer(t, list, nil)
	u := New(Options{APIBase: srv.URL, Repo: "o/r"})
	ctx := context.Background()
	rel, newer, err := u.Check(ctx, Stable, "0.9.1")
	if err != nil {
		t.Fatal(err)
	}
	if rel.Version != "0.10.0" || rel.Tag != "v0.10.0" || !newer || rel.Channel != Stable {
		t.Fatalf("got %+v newer=%v", rel, newer)
	}
	if _, newer, _ := u.Check(ctx, Stable, "0.10.0"); newer {
		t.Error("same version reported as newer")
	}
	if _, newer, _ := u.Check(ctx, Stable, "0.10.1-dev.202611210400.abc1234"); newer {
		t.Error("stable 0.10.0 reported newer than dev 0.10.1-dev")
	}
	if _, _, err := u.Check(ctx, "beta", "0.1.0"); err == nil {
		t.Error("unknown channel accepted")
	}
	empty := New(Options{APIBase: apiServer(t, nil, nil).URL, Repo: "o/r"})
	if _, _, err := empty.Check(ctx, Stable, "0.1.0"); err == nil || !strings.Contains(err.Error(), "no stable release") {
		t.Errorf("no releases: err = %v", err)
	}
	if _, _, err := empty.Check(ctx, Dev, "0.1.0"); err == nil {
		t.Error("missing dev release: no error")
	}
}

func TestCheckDev(t *testing.T) {
	dev := &fakeRelease{TagName: "dev", Name: "0.1.1-dev.202611210400.abc1234", Prerelease: true,
		Assets: []map[string]string{{"name": SumsName, "browser_download_url": "http://x/SHA256SUMS"}}}
	u := New(Options{APIBase: apiServer(t, nil, dev).URL, Repo: "o/r"})
	ctx := context.Background()
	rel, newer, err := u.Check(ctx, Dev, "0.1.1-dev.202611200400.fff0000")
	if err != nil {
		t.Fatal(err)
	}
	if !newer || rel.Version != dev.Name || rel.Tag != "dev" || rel.Assets[SumsName] != "http://x/SHA256SUMS" {
		t.Fatalf("got %+v newer=%v", rel, newer)
	}
	if _, newer, _ := u.Check(ctx, Dev, dev.Name); newer {
		t.Error("same dev build reported newer")
	}
	dev.Name = "not a version"
	if _, _, err := u.Check(ctx, Dev, "0.1.0"); err == nil {
		t.Error("bad dev release name accepted")
	}
}

func TestCheckTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()
	u := New(Options{APIBase: srv.URL, Repo: "o/r", APITimeout: 100 * time.Millisecond})
	start := time.Now()
	if _, _, err := u.Check(context.Background(), Stable, "0.1.0"); err == nil {
		t.Fatal("hung server: no error")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("API timeout not enforced")
	}
}

type entry struct {
	name    string
	body    string
	symlink string
}

func tarGz(t *testing.T, entries []entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: 0o755, Size: int64(len(e.body)), Typeflag: tar.TypeReg}
		if e.symlink != "" {
			hdr = &tar.Header{Name: e.name, Mode: 0o777, Typeflag: tar.TypeSymlink, Linkname: e.symlink}
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if e.symlink == "" {
			tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func zipBytes(t *testing.T, entries []entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		fh := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		fh.SetMode(0o755)
		body := e.body
		if e.symlink != "" {
			fh.SetMode(os.ModeSymlink | 0o777)
			body = e.symlink
		}
		w, err := zw.CreateHeader(fh)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	zw.Close()
	return buf.Bytes()
}

// releaseServer serves one archive and a SHA256SUMS whose entry is sumOf.
func releaseServer(t *testing.T, name string, archive, sumOf []byte) Release {
	t.Helper()
	h := sha256.Sum256(sumOf)
	sums := hex.EncodeToString(h[:]) + "  " + name + "\n" + strings.Repeat("0", 64) + "  other.tar.gz\n"
	mux := http.NewServeMux()
	mux.HandleFunc("/dl/"+name, func(w http.ResponseWriter, r *http.Request) { w.Write(archive) })
	mux.HandleFunc("/dl/"+SumsName, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(sums)) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return Release{Tag: "v1.2.3", Version: "1.2.3", Assets: map[string]string{
		name:     srv.URL + "/dl/" + name,
		SumsName: srv.URL + "/dl/" + SumsName,
	}}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

func TestDownloadGood(t *testing.T) {
	archive := tarGz(t, []entry{{name: "tentmaker", body: "BINARY"}, {name: "LICENSE", body: "MIT"}, {name: "README.md", body: "# hi"}})
	rel := releaseServer(t, "tentmaker_linux_amd64.tar.gz", archive, archive)
	u := New(Options{GOOS: "linux", GOARCH: "amd64"})
	dir := t.TempDir()
	bin, err := u.Download(context.Background(), rel, dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(bin)
	if err != nil || string(b) != "BINARY" {
		t.Fatalf("binary = %q, %v", b, err)
	}
	if filepath.Base(bin) != "tentmaker" || filepath.Dir(filepath.Dir(bin)) != dir {
		t.Errorf("binary path %s", bin)
	}
	if names := dirNames(t, dir); len(names) != 1 {
		t.Errorf("leftovers in download dir: %v", names)
	}
}

func TestDownloadZip(t *testing.T) {
	archive := zipBytes(t, []entry{{name: "tentmaker.exe", body: "WINBIN"}, {name: "LICENSE", body: "MIT"}})
	rel := releaseServer(t, "tentmaker_windows_arm64.zip", archive, archive)
	u := New(Options{GOOS: "windows", GOARCH: "arm64"})
	bin, err := u.Download(context.Background(), rel, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(bin); string(b) != "WINBIN" || filepath.Base(bin) != "tentmaker.exe" {
		t.Fatalf("got %s = %q", bin, b)
	}
}

func TestDownloadTamperedRefuses(t *testing.T) {
	good := tarGz(t, []entry{{name: "tentmaker", body: "BINARY"}})
	evil := tarGz(t, []entry{{name: "tentmaker", body: "EVIL"}})
	rel := releaseServer(t, "tentmaker_linux_amd64.tar.gz", evil, good)
	dir := t.TempDir()
	_, err := New(Options{GOOS: "linux", GOARCH: "amd64"}).Download(context.Background(), rel, dir)
	if err == nil || !strings.Contains(err.Error(), "failed verification") {
		t.Fatalf("tampered archive: err = %v", err)
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Errorf("tampered archive left files behind: %v", names)
	}
}

func TestDownloadUnsafeEntries(t *testing.T) {
	cases := map[string][]entry{
		"traversal": {{name: "tentmaker", body: "B"}, {name: "../escaped", body: "X"}},
		"nested":    {{name: "a/../../escaped", body: "X"}},
		"absolute":  {{name: "/tmp/escaped", body: "X"}},
		"symlink":   {{name: "tentmaker", symlink: "/bin/sh"}},
	}
	for name, ents := range cases {
		for _, goos := range []string{"linux", "windows"} {
			t.Run(name+"_"+goos, func(t *testing.T) {
				var archive []byte
				asset := ArchiveName(goos, "amd64")
				if goos == "windows" {
					archive = zipBytes(t, ents)
				} else {
					archive = tarGz(t, ents)
				}
				rel := releaseServer(t, asset, archive, archive)
				parent := t.TempDir()
				dir := filepath.Join(parent, "dl")
				_, err := New(Options{GOOS: goos, GOARCH: "amd64"}).Download(context.Background(), rel, dir)
				if err == nil {
					t.Fatal("unsafe archive accepted")
				}
				if _, err := os.Lstat(filepath.Join(parent, "escaped")); err == nil {
					t.Fatal("traversal entry escaped the target dir")
				}
				if names := dirNames(t, dir); len(names) != 0 {
					t.Errorf("rejected archive left files behind: %v", names)
				}
			})
		}
	}
}

func TestDownloadMissingAssets(t *testing.T) {
	u := New(Options{GOOS: "linux", GOARCH: "amd64"})
	ctx := context.Background()
	if _, err := u.Download(ctx, Release{Assets: map[string]string{SumsName: "http://x"}}, t.TempDir()); err == nil {
		t.Error("missing archive accepted")
	}
	if _, err := u.Download(ctx, Release{Assets: map[string]string{"tentmaker_linux_amd64.tar.gz": "http://x"}}, t.TempDir()); err == nil ||
		!strings.Contains(err.Error(), "unverified") {
		t.Errorf("missing SHA256SUMS: err = %v", err)
	}
	if _, err := lookupSum([]byte("abc  tentmaker_linux_amd64.tar.gz\n"), "tentmaker_linux_amd64.tar.gz"); err == nil {
		t.Error("malformed sum accepted")
	}
}

// setupBins puts a fake current binary in a temp dir and returns its path
// and the path of a runnable new binary (a copy of this test binary).
func setupBins(t *testing.T, curName string) (cur, newBin string, self []byte) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	self, err = os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	newBin = filepath.Join(t.TempDir(), "downloaded-tentmaker.exe")
	if err := os.WriteFile(newBin, self, 0o755); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" && !strings.HasSuffix(curName, ".exe") {
		curName += ".exe" // Windows runs only .exe files
	}
	cur = filepath.Join(dir, curName)
	if err := os.WriteFile(cur, []byte("OLD BINARY"), 0o755); err != nil {
		t.Fatal(err)
	}
	return cur, newBin, self
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestApplyAndRollback(t *testing.T) {
	t.Setenv("TENTMAKER_UPDATE_FAKE", "ok")
	t.Setenv("TENTMAKER_UPDATE_FAKE_VERSION", "1.2.3")
	cur, newBin, self := setupBins(t, "tentmaker")
	u := New(Options{GOOS: "linux"})
	if err := u.Apply(context.Background(), newBin, cur, "v1.2.3"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mustRead(t, cur), self) {
		t.Fatal("new binary not in place")
	}
	if string(mustRead(t, cur+PrevSuffix)) != "OLD BINARY" {
		t.Fatal("previous binary not kept")
	}
	if _, err := os.Stat(stagedPath(cur)); err == nil {
		t.Error("staged file left behind")
	}
	if err := u.Rollback(cur); err != nil {
		t.Fatal(err)
	}
	if string(mustRead(t, cur)) != "OLD BINARY" {
		t.Fatal("rollback did not restore the previous binary")
	}
}

func TestApplyWindowsSwap(t *testing.T) {
	t.Setenv("TENTMAKER_UPDATE_FAKE", "ok")
	t.Setenv("TENTMAKER_UPDATE_FAKE_VERSION", "2.0.0")
	cur, newBin, self := setupBins(t, "tentmaker.exe")
	// A stale .old from an earlier update gets replaced.
	os.WriteFile(cur+OldSuffix, []byte("STALE"), 0o755)
	u := New(Options{GOOS: "windows"})
	if err := u.Apply(context.Background(), newBin, cur, "2.0.0"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mustRead(t, cur), self) {
		t.Fatal("new binary not in place")
	}
	if string(mustRead(t, cur+OldSuffix)) != "OLD BINARY" {
		t.Fatal("running exe not renamed aside to .old")
	}
	if string(mustRead(t, cur+PrevSuffix)) != "OLD BINARY" {
		t.Fatal("previous binary not kept as .prev")
	}
	if err := u.Rollback(cur); err != nil {
		t.Fatal(err)
	}
	if string(mustRead(t, cur)) != "OLD BINARY" {
		t.Fatal("rollback did not restore the previous binary")
	}
}

func TestApplyFailedHealthCheckDoesNotSwap(t *testing.T) {
	cases := map[string]struct {
		mode, version string
		timeout       time.Duration
	}{
		"wrong version": {"ok", "9.9.9", 0},
		"exits nonzero": {"fail", "", 0},
		"hangs":         {"hang", "", 300 * time.Millisecond},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv("TENTMAKER_UPDATE_FAKE", tc.mode)
			t.Setenv("TENTMAKER_UPDATE_FAKE_VERSION", tc.version)
			cur, newBin, _ := setupBins(t, "tentmaker")
			u := New(Options{GOOS: "linux", HealthTimeout: tc.timeout})
			start := time.Now()
			err := u.Apply(context.Background(), newBin, cur, "1.2.3")
			if err == nil || !strings.Contains(err.Error(), "health check") {
				t.Fatalf("err = %v", err)
			}
			if tc.timeout > 0 && time.Since(start) > 5*time.Second {
				t.Error("health check timeout not enforced")
			}
			if string(mustRead(t, cur)) != "OLD BINARY" {
				t.Fatal("binary swapped despite a failed health check")
			}
			for _, p := range []string{cur + PrevSuffix, stagedPath(cur)} {
				if _, err := os.Stat(p); err == nil {
					t.Errorf("%s left behind", filepath.Base(p))
				}
			}
		})
	}
}

func TestRollbackWithoutPrev(t *testing.T) {
	cur := filepath.Join(t.TempDir(), "tentmaker")
	os.WriteFile(cur, []byte("X"), 0o755)
	if err := New(Options{}).Rollback(cur); err == nil {
		t.Fatal("rollback without .prev succeeded")
	}
}

func TestParseVersionOutput(t *testing.T) {
	for in, want := range map[string]string{
		`{"version":"1.2.3","commit":"abc"}`: "1.2.3",
		"tentmaker 0.0.0-dev\n":              "0.0.0-dev",
	} {
		if got, err := parseVersionOutput([]byte(in)); err != nil || got != want {
			t.Errorf("%q -> %q, %v", in, got, err)
		}
	}
	if _, err := parseVersionOutput([]byte("garbage output here")); err == nil {
		t.Error("garbage accepted")
	}
}

// TENTMAKER_UPDATE_API points the updater at a fake release server, the
// way TENTMAKER_DOWNLOAD_BASE does for the installers.
func TestFromEnvUsesTheTestReleaseServer(t *testing.T) {
	dev := &fakeRelease{TagName: "dev", Name: "0.0.1-scn.B", Prerelease: true,
		Assets: []map[string]string{{"name": SumsName, "browser_download_url": "http://x/SHA256SUMS"}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/baney75/tentmaker/releases/tags/dev" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(dev)
	}))
	t.Cleanup(srv.Close)
	u := FromEnv(func(k string) string {
		if k == EnvAPIBase {
			return srv.URL
		}
		return ""
	})
	rel, newer, err := u.Check(context.Background(), Dev, "0.0.1-scn.A")
	if err != nil || !newer || rel.Version != "0.0.1-scn.B" {
		t.Fatalf("got %+v newer=%v err=%v", rel, newer, err)
	}
	if def := FromEnv(func(string) string { return "" }); def.o.APIBase != "https://api.github.com" || def.o.Repo != "baney75/tentmaker" {
		t.Fatalf("without the hook: %s %s", def.o.APIBase, def.o.Repo)
	}
}
