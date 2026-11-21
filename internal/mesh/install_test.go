package mesh

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func installTS() *fakeTS {
	ts := newFakeTS()
	ts.peers = []Peer{
		{ID: "n1", HostName: "alice-linux", DNSName: "alice-linux.example-tailnet.ts.net", OS: "linux", Online: true, UserID: 1001},
		{ID: "n2", HostName: "alice-win", DNSName: "alice-win.example-tailnet.ts.net", OS: "windows", Online: true, UserID: 1001},
	}
	return ts
}

const wantScript = `set -eu
exec 2>&1
f=$(mktemp)
trap 'rm -f "$f"' EXIT
curl -fsSL --max-time 300 https://raw.githubusercontent.com/baney75/tentmaker/main/scripts/install.sh -o "$f"
sh "$f" --channel 'dev'
if [ "$(id -u)" -eq 0 ]; then tentmaker setup --yes --json --pool 'home' --memory '4GiB' --boost-memory '12GiB' --repo 'alice/app' --repo 'alice/site.io'; else sudo -n tentmaker setup --yes --json --pool 'home' --memory '4GiB' --boost-memory '12GiB' --repo 'alice/app' --repo 'alice/site.io'; fi`

func TestRemoteInstallArgv(t *testing.T) {
	fr := &fakeRunner{out: map[string][]byte{}}
	find := func() (string, error) { return "/usr/local/bin/tailscale", nil }
	// Sizes are normalized through config.ParseSize ("4G" -> 4GiB).
	args := InstallArgs{Channel: "dev", Pool: "home", Memory: "4G", BoostMemory: "12GB", Repos: []string{"alice/app", "alice/site.io"}}
	if err := remoteInstall(context.Background(), installTS(), find, fr.run, "alice-linux", args); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"/usr/local/bin/tailscale", "ssh", "alice-linux", "--", "sh", "-c", shellQuote(wantScript)}}
	if !reflect.DeepEqual(fr.calls, want) {
		t.Fatalf("argv:\n got %q\nwant %q", fr.calls, want)
	}
	if fr.dl[0] <= 0 {
		t.Error("ssh call has no deadline")
	}
}

func TestRemoteInstallDefaults(t *testing.T) {
	fr := &fakeRunner{out: map[string][]byte{}}
	find := func() (string, error) { return "tailscale", nil }
	if err := remoteInstall(context.Background(), installTS(), find, fr.run, "root@100.64.0.2", InstallArgs{}); err != nil {
		t.Fatal(err)
	}
	script := fr.calls[0][6]
	if !strings.Contains(script, `--channel '\''stable'\''`) || !strings.Contains(script, "tentmaker setup --yes --json; else") {
		t.Fatalf("script: %s", script)
	}
	if fr.calls[0][2] != "root@100.64.0.2" {
		t.Fatalf("host: %q", fr.calls[0])
	}
}

func TestRemoteInstallValidation(t *testing.T) {
	find := func() (string, error) { return "tailscale", nil }
	bad := []struct {
		host string
		args InstallArgs
	}{
		{"alice-linux", InstallArgs{Channel: "beta"}},
		{"alice-linux", InstallArgs{Pool: "home; rm -rf /"}},
		{"alice-linux", InstallArgs{Pool: "$(id)"}},
		{"alice-linux", InstallArgs{Pool: "-x"}},
		{"alice-linux", InstallArgs{Memory: "4GiB'"}},
		{"alice-linux", InstallArgs{Memory: "lots"}},
		{"alice-linux", InstallArgs{BoostMemory: "12"}},
		{"alice-linux", InstallArgs{Memory: "0"}},
		{"alice-linux", InstallArgs{Repos: []string{"alice"}}},
		{"alice-linux", InstallArgs{Repos: []string{"alice/app;reboot"}}},
		{"alice-linux", InstallArgs{Repos: []string{"alice/../etc"}}},
		{"alice-linux", InstallArgs{Repos: []string{"a/b/c"}}},
		{"alice-linux; reboot", InstallArgs{}},
		{"-oProxyCommand=sh", InstallArgs{}},
		{"", InstallArgs{}},
		{"alice linux", InstallArgs{}},
	}
	for _, c := range bad {
		fr := &fakeRunner{}
		if err := remoteInstall(context.Background(), installTS(), find, fr.run, c.host, c.args); err == nil {
			t.Errorf("%q %+v: want error", c.host, c.args)
		}
		if len(fr.calls) != 0 {
			t.Errorf("%q %+v: ran %q", c.host, c.args, fr.calls)
		}
	}
}

func TestRemoteInstallWindows(t *testing.T) {
	find := func() (string, error) { return "tailscale", nil }
	for _, host := range []string{"alice-win", "alice-win.example-tailnet.ts.net", "ALICE-WIN", "user@alice-win"} {
		fr := &fakeRunner{}
		err := remoteInstall(context.Background(), installTS(), find, fr.run, host, InstallArgs{Channel: "dev", Memory: "4G"})
		if err == nil {
			t.Fatalf("%s: want an error for a Windows host", host)
		}
		msg := err.Error()
		if !strings.Contains(msg, "Windows") || !strings.Contains(msg, "irm https://raw.githubusercontent.com/baney75/tentmaker/main/scripts/install.ps1") ||
			!strings.Contains(msg, "-Channel dev") || !strings.Contains(msg, "tentmaker setup --yes --json --memory '4GiB'") {
			t.Errorf("%s: message lacks the PowerShell to run:\n%s", host, msg)
		}
		if len(fr.calls) != 0 {
			t.Errorf("%s: ran tailscale ssh on Windows", host)
		}
	}
	got := windowsCommand("stable", nil)
	if got != "irm https://raw.githubusercontent.com/baney75/tentmaker/main/scripts/install.ps1 | iex; tentmaker setup --yes --json" {
		t.Errorf("stable windows command: %s", got)
	}
}

func TestRemoteInstallFailureReturnsOutput(t *testing.T) {
	fr := &fakeRunner{out: map[string][]byte{"error": []byte("curl: (6) Could not resolve host\n")}, err: errors.New("exit status 6")}
	find := func() (string, error) { return "tailscale", nil }
	err := remoteInstall(context.Background(), installTS(), find, fr.run, "alice-linux", InstallArgs{})
	if err == nil || !strings.Contains(err.Error(), "Could not resolve host") || !strings.Contains(err.Error(), "exit status 6") {
		t.Fatalf("got %v", err)
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{"a": "'a'", "": "''", "it's": `'it'\''s'`} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}
