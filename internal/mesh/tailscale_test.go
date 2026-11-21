package mesh

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseStatus(t *testing.T) {
	self, peers, err := ParseStatus(readFixture(t, "status.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := Peer{
		ID: "nSELF000CNTRL", HostName: "alice-mac", DNSName: "alice-mac.example-tailnet.ts.net",
		IPs:    []netip.Addr{netip.MustParseAddr("100.64.0.1"), netip.MustParseAddr("fd7a:115c:a1e0::1")},
		Online: true, OS: "macOS", UserID: 1001, LoginName: "alice@example.com",
	}
	if !reflect.DeepEqual(self, want) {
		t.Fatalf("self:\n got %+v\nwant %+v", self, want)
	}
	if len(peers) != 6 {
		t.Fatalf("got %d peers, want 6", len(peers))
	}
	by := map[string]Peer{}
	for _, p := range peers {
		by[p.HostName] = p
	}
	if p := by["alice-linux"]; !p.Online || p.OS != "linux" || p.UserID != 1001 || p.LoginName != "alice@example.com" ||
		p.DNSName != "alice-linux.example-tailnet.ts.net" || len(p.IPs) != 2 || p.IPs[0] != netip.MustParseAddr("100.64.0.2") || p.ID != "nLINUX00CNTRL" {
		t.Errorf("alice-linux: %+v", p)
	}
	if by["alice-old"].Online {
		t.Error("offline peer reported online")
	}
	if by["alice-expired"].Online {
		t.Error("expired peer reported online")
	}
	if p := by["bob-pc"]; p.LoginName != "bob@example.com" || p.UserID != 1002 {
		t.Errorf("bob-pc: %+v", p)
	}
	if p := by["ci-box"]; p.LoginName != TaggedLogin || len(p.Tags) != 1 {
		t.Errorf("ci-box: %+v", p)
	}
	if by["alice-win"].OS != "windows" {
		t.Errorf("alice-win: %+v", by["alice-win"])
	}
	// Sorted by host name.
	for i := 1; i < len(peers); i++ {
		if peers[i-1].HostName > peers[i].HostName {
			t.Fatalf("peers not sorted: %s before %s", peers[i-1].HostName, peers[i].HostName)
		}
	}
}

func TestParseStatusNotRunning(t *testing.T) {
	for _, in := range []string{
		`{"BackendState":"Stopped","Self":{"ID":"x","TailscaleIPs":["100.64.0.1"]}}`,
		`{"BackendState":"NeedsLogin"}`,
		`{"BackendState":"Running"}`,
		`{"BackendState":"Running","Self":{"ID":"x","TailscaleIPs":[]}}`,
		`not json`,
	} {
		if _, _, err := ParseStatus([]byte(in)); err == nil {
			t.Errorf("ParseStatus(%s): want error", in)
		}
	}
}

func TestParseWhoIs(t *testing.T) {
	id, err := ParseWhoIs(readFixture(t, "whois.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := Identity{
		ID: "nLINUX00CNTRL", Node: "alice-linux.example-tailnet.ts.net", HostName: "alice-linux",
		IPs:    []netip.Addr{netip.MustParseAddr("100.64.0.2"), netip.MustParseAddr("fd7a:115c:a1e0::2")},
		UserID: 1001, LoginName: "alice@example.com",
	}
	if !reflect.DeepEqual(id, want) {
		t.Fatalf("got %+v\nwant %+v", id, want)
	}
	for _, in := range []string{`{}`, `{"Node":{"Name":"x"}}`, `{"UserProfile":{"ID":1}}`, `{"Node":{},"UserProfile":{}}`, `[`} {
		if _, err := ParseWhoIs([]byte(in)); err == nil {
			t.Errorf("ParseWhoIs(%s): want error", in)
		}
	}
}

func TestFindTailscale(t *testing.T) {
	noPath := func(string) (string, error) { return "", errors.New("not found") }
	only := func(want string) func(string) bool { return func(p string) bool { return p == want } }
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

	if p, err := findTailscale("linux", func(string) (string, error) { return "/x/tailscale", nil }, only(""), env(nil)); err != nil || p != "/x/tailscale" {
		t.Errorf("PATH: %q %v", p, err)
	}
	mac := "/Applications/Tailscale.app/Contents/MacOS/Tailscale"
	if p, err := findTailscale("darwin", noPath, only(mac), env(nil)); err != nil || p != mac {
		t.Errorf("darwin app: %q %v", p, err)
	}
	if p, err := findTailscale("darwin", noPath, only("/opt/homebrew/bin/tailscale"), env(nil)); err != nil || p != "/opt/homebrew/bin/tailscale" {
		t.Errorf("darwin brew: %q %v", p, err)
	}
	win := `C:\Program Files\Tailscale\tailscale.exe`
	if p, err := findTailscale("windows", noPath, only(win), env(nil)); err != nil || p != win {
		t.Errorf("windows: %q %v", p, err)
	}
	if p, err := findTailscale("windows", noPath, only(`D:\Apps\Tailscale\tailscale.exe`), env(map[string]string{"ProgramFiles": `D:\Apps`})); err != nil || p != `D:\Apps\Tailscale\tailscale.exe` {
		t.Errorf("windows ProgramFiles: %q %v", p, err)
	}
	if p, err := findTailscale("linux", noPath, only("/usr/bin/tailscale"), env(nil)); err != nil || p != "/usr/bin/tailscale" {
		t.Errorf("linux: %q %v", p, err)
	}
	if _, err := findTailscale("linux", noPath, only(""), env(nil)); err == nil {
		t.Error("missing binary: want error")
	}
}

type fakeRunner struct {
	mu    sync.Mutex
	calls [][]string
	out   map[string][]byte // by first arg
	err   error
	dl    []time.Duration
}

func (f *fakeRunner) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string{name}, args...))
	if d, ok := ctx.Deadline(); ok {
		f.dl = append(f.dl, time.Until(d))
	} else {
		f.dl = append(f.dl, -1)
	}
	if f.err != nil {
		return f.out["error"], f.err
	}
	return f.out[args[0]], nil
}

func TestCLICommands(t *testing.T) {
	fr := &fakeRunner{out: map[string][]byte{"status": readFixture(t, "status.json"), "whois": readFixture(t, "whois.json")}}
	now := time.Unix(1_800_000_000, 0)
	c := newCLI(fr.run, func() (string, error) { return "/bin/tailscale", nil }, func() time.Time { return now })
	ctx := context.Background()

	self, err := c.Self(ctx)
	if err != nil || self.HostName != "alice-mac" {
		t.Fatalf("Self: %+v %v", self, err)
	}
	peers, err := c.Peers(ctx)
	if err != nil || len(peers) != 6 {
		t.Fatalf("Peers: %d %v", len(peers), err)
	}
	id, err := c.WhoIs(ctx, "100.64.0.2:50123")
	if err != nil || id.LoginName != "alice@example.com" {
		t.Fatalf("WhoIs: %+v %v", id, err)
	}
	want := [][]string{
		{"/bin/tailscale", "status", "--json"},
		{"/bin/tailscale", "status", "--json"},
		{"/bin/tailscale", "whois", "--json", "100.64.0.2:50123"},
	}
	if !reflect.DeepEqual(fr.calls, want) {
		t.Fatalf("argv:\n got %q\nwant %q", fr.calls, want)
	}
	for i, d := range fr.dl {
		if d <= 0 || d > cliTimeout {
			t.Errorf("call %d: deadline %v, want within %v", i, d, cliTimeout)
		}
	}

	// IPv6 keeps brackets; the cache is per IP, so a new port hits it.
	if _, err := c.WhoIs(ctx, "[fd7a:115c:a1e0::2]:7433"); err != nil {
		t.Fatal(err)
	}
	if got := fr.calls[len(fr.calls)-1]; got[3] != "[fd7a:115c:a1e0::2]:7433" {
		t.Errorf("v6 argv: %q", got)
	}
	n := len(fr.calls)
	if _, err := c.WhoIs(ctx, "100.64.0.2:9999"); err != nil {
		t.Fatal(err)
	}
	if len(fr.calls) != n {
		t.Error("WhoIs within 60 s should come from the cache")
	}
	now = now.Add(61 * time.Second)
	if _, err := c.WhoIs(ctx, "100.64.0.2:9999"); err != nil {
		t.Fatal(err)
	}
	if len(fr.calls) != n+1 {
		t.Error("WhoIs after 60 s should ask tailscale again")
	}
}

func TestCLIWhoIsFailuresNotCached(t *testing.T) {
	fr := &fakeRunner{err: errors.New("exit status 1: no match for IP")}
	c := newCLI(fr.run, func() (string, error) { return "/bin/tailscale", nil }, time.Now)
	for i := 0; i < 2; i++ {
		if _, err := c.WhoIs(context.Background(), "100.64.0.9:1"); err == nil {
			t.Fatal("want error")
		}
	}
	if len(fr.calls) != 2 {
		t.Errorf("failed WhoIs was cached: %d calls", len(fr.calls))
	}
	// Junk never reaches argv.
	for _, bad := range []string{"", "host:1", "100.64.0.1; rm -rf /", "-flag", "fe80::1%eth0"} {
		if _, err := c.WhoIs(context.Background(), bad); err == nil {
			t.Errorf("WhoIs(%q): want error", bad)
		}
	}
	if len(fr.calls) != 2 {
		t.Errorf("bad addresses reached the runner: %q", fr.calls)
	}
}

func TestCLIBinaryMissing(t *testing.T) {
	c := newCLI(func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("runner called without a binary")
		return nil, nil
	}, func() (string, error) { return "", errors.New("tailscale CLI not found") }, time.Now)
	if _, err := c.Self(context.Background()); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("got %v", err)
	}
}
