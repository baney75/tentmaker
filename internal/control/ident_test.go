package control

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"reflect"
	"testing"
)

func TestRunnerIDs(t *testing.T) {
	passwd := []byte("root:x:0:0::/root:/bin/sh\nalice:x:1000:1000::/home/alice:/bin/bash\ntentmaker-runner:x:990:990::/var/lib/tentmaker/runner:/usr/sbin/nologin\n")
	subuid := []byte("alice:100000:65536\ntentmaker-runner:165536:65536\n990:300000:10\n")
	got, err := RunnerIDs("tentmaker-runner", passwd, subuid)
	want := []IDRange{{990, 1}, {165536, 65536}, {300000, 10}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("ids %v %v, want %v", got, err, want)
	}
	if got, err := RunnerIDs("tentmaker-runner", []byte("root:x:0:0::/:/bin/sh\n"), subuid); err != nil || got != nil {
		t.Fatalf("no runner user: %v %v", got, err)
	}
	if _, err := RunnerIDs("tentmaker-runner", []byte("tentmaker-runner:x:abc:1::/:/bin/sh\n"), nil); err == nil {
		t.Fatal("a malformed uid was accepted")
	}
	if _, err := RunnerIDs("tentmaker-runner", passwd, []byte("tentmaker-runner:x:1\n")); err == nil {
		t.Fatal("a malformed subuid line was accepted")
	}
}

func TestClassifyUID(t *testing.T) {
	runner := []IDRange{{990, 1}, {165536, 65536}}
	for _, tc := range []struct {
		uid           uint32
		runner, admin bool
	}{{0, false, true}, {1000, false, false}, {990, true, false}, {165536, true, false}, {231071, true, false}, {231072, false, false}, {989, false, false}} {
		c, err := classifyUID(tc.uid, runner, nil)
		if err != nil || c.Runner != tc.runner || c.Admin != tc.admin {
			t.Errorf("uid %d: %+v %v, want runner %v admin %v", tc.uid, c, err, tc.runner, tc.admin)
		}
	}
	// Unreadable runner ids: root is served, nobody else.
	if c, err := classifyUID(0, nil, errors.New("EIO")); err != nil || !c.Admin {
		t.Errorf("root with unreadable ids: %+v %v", c, err)
	}
	if _, err := classifyUID(1000, nil, errors.New("EIO")); err == nil {
		t.Error("a user was served while the runner's ids were unreadable")
	}
}

func TestClassifySID(t *testing.T) {
	const runner = "S-1-5-21-1-2-3-1005"
	for _, tc := range []struct {
		sid           string
		elevated      bool
		runner, admin bool
	}{{"S-1-5-18", false, false, true}, {"S-1-5-21-1-2-3-1001", true, false, true}, {"S-1-5-21-1-2-3-1001", false, false, false},
		{runner, false, true, false}, {"s-1-5-21-1-2-3-1005", false, true, false}} {
		c, err := classifySID(tc.sid, tc.elevated, runner, nil)
		if err != nil || c.Runner != tc.runner || c.Admin != tc.admin {
			t.Errorf("%s elevated %v: %+v %v", tc.sid, tc.elevated, c, err)
		}
	}
	if _, err := classifySID("S-1-5-21-1-2-3-1001", false, "", errors.New("lookup failed")); err == nil {
		t.Error("a user was served while the runner's SID was unknown")
	}
	if c, err := classifySID("S-1-5-21-1-2-3-1001", false, "", nil); err != nil || c.Runner {
		t.Errorf("no runner user: %+v %v", c, err)
	}
}

// tableRow encodes one MIB_TCPROW_OWNER_PID as Windows lays it out.
func tableRow(local, remote netip.AddrPort, pid uint32) []byte {
	b := make([]byte, 24)
	binary.LittleEndian.PutUint32(b, 5) // ESTABLISHED
	put := func(off int, ap netip.AddrPort) {
		a := ap.Addr().As4()
		copy(b[off:], a[:])
		b[off+4], b[off+5] = byte(ap.Port()>>8), byte(ap.Port())
	}
	put(4, local)
	put(12, remote)
	binary.LittleEndian.PutUint32(b[20:], pid)
	return b
}

func TestOwnerPIDFromTCPTable(t *testing.T) {
	server := netip.MustParseAddrPort("127.0.0.1:7434")
	job := netip.MustParseAddrPort("127.0.0.1:50001")
	user := netip.MustParseAddrPort("127.0.0.1:50002")
	tbl := make([]byte, 4)
	binary.LittleEndian.PutUint32(tbl, 4)
	tbl = append(tbl, tableRow(job, server, 4242)...)
	tbl = append(tbl, tableRow(server, job, 1000)...) // the daemon's end
	tbl = append(tbl, tableRow(user, server, 77)...)
	tbl = append(tbl, tableRow(server, user, 1000)...)
	rows, err := parseTCPTable(tbl)
	if err != nil {
		t.Fatal(err)
	}
	if pid, err := ownerPID(rows, job, server); err != nil || pid != 4242 {
		t.Fatalf("job's connection: pid %d %v", pid, err)
	}
	mapped := netip.AddrPortFrom(netip.MustParseAddr("::ffff:127.0.0.1"), 50002)
	if pid, err := ownerPID(rows, mapped, server); err != nil || pid != 77 {
		t.Fatalf("user's connection: pid %d %v", pid, err)
	}
	if _, err := ownerPID(rows, netip.MustParseAddrPort("127.0.0.1:50003"), server); err == nil {
		t.Fatal("an unknown connection got an owner")
	}
	if _, err := parseTCPTable(tbl[:30]); err == nil {
		t.Fatal("a truncated table parsed")
	}
}
