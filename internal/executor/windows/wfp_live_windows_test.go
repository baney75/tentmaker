//go:build live

package windows

// TestWFPLoopbackConditionsLive asks a real WFP engine which conditions
// match a TCP connection from this process to its own listener on
// 127.0.0.1. Each case adds one hard block in Tentmaker's sublayer, keyed to
// the listener's port, then dials. It needs an elevated shell and changes
// the host's filters, so it runs only with -tags live on a throwaway runner.
//
// It records why the loopback fence sits at the connect layer (DESIGN.md,
// Windows): at accept, ALE_REMOTE_USER_ID never matches loopback traffic,
// so no filter can let the runner reach its own listeners and nothing else
// (review r2 partS second-S2-F6, windows-latest, 2026-11-21).

import (
	"fmt"
	"net"
	"runtime"
	"testing"
	"time"
	"unsafe"

	win "golang.org/x/sys/windows"
)

// The accept layer and its fields, from fwpmu.h; only this probe uses them.
var (
	fwpmLayerALEAuthRecvAcceptV4 = win.GUID{Data1: 0xe1cd9fe7, Data2: 0xf4b5, Data3: 0x4273, Data4: [8]byte{0x96, 0xc0, 0x59, 0x2e, 0x48, 0x7b, 0x86, 0x50}}
	fwpmConditionALERemoteUserID = win.GUID{Data1: 0xf63073b7, Data2: 0x0189, Data3: 0x4ab0, Data4: [8]byte{0x95, 0xa4, 0x61, 0x23, 0xcb, 0xfa, 0xb8, 0x62}}
	fwpmConditionLocalPort       = win.GUID{Data1: 0x0c1ba1af, Data2: 0x5765, Data3: 0x453f, Data4: [8]byte{0xaf, 0x22, 0xa8, 0xf7, 0x91, 0xac, 0x77, 0x5b}}
)

type probeCond struct {
	field win.GUID
	sid   string // a security descriptor condition for this SID
	flags uint32 // a flags-all-set condition
	port  bool   // a port condition on the listener's port
}

func (s *wfpSession) addProbe(key GUID, layer win.GUID, port uint16, cs []probeCond) error {
	var pin runtime.Pinner
	defer pin.Unpin()
	var conds []fwpmFilterCondition0
	for _, c := range cs {
		switch {
		case c.sid != "":
			sd, err := win.SecurityDescriptorFromString("D:(A;;CC;;;" + c.sid + ")")
			if err != nil {
				return err
			}
			pin.Pin(sd)
			blob := &fwpByteBlob{Size: sd.Length(), Data: (*byte)(unsafe.Pointer(sd))}
			pin.Pin(blob)
			conds = append(conds, fwpmFilterCondition0{FieldKey: c.field, MatchType: fwpMatchEqual, Value: fwpValue0{Type: fwpSecurityDescriptor, Value: uintptr(unsafe.Pointer(blob))}})
		case c.flags != 0:
			conds = append(conds, fwpmFilterCondition0{FieldKey: c.field, MatchType: fwpMatchFlagsAllSet, Value: fwpValue0{Type: fwpUint32, Value: uintptr(c.flags)}})
		case c.port:
			conds = append(conds, fwpmFilterCondition0{FieldKey: c.field, MatchType: fwpMatchEqual, Value: fwpValue0{Type: fwpUint16, Value: uintptr(port)}})
		}
	}
	prov := win.GUID(WFPProvider)
	flt := &fwpmFilter0{
		FilterKey:           win.GUID(key),
		DisplayData:         fwpmDisplayData0{Name: u16("Tentmaker probe"), Description: u16("tentmaker live probe")},
		Flags:               fwpmFilterFlagClearActionRight,
		ProviderKey:         &prov,
		LayerKey:            layer,
		SubLayerKey:         win.GUID(WFPSublayer),
		Weight:              fwpValue0{Type: fwpUint8, Value: 1},
		NumFilterConditions: uint32(len(conds)),
		FilterCondition:     &conds[0],
		Action:              fwpmAction0{Type: fwpActionBlock},
	}
	var id uint64
	r := wfpCall(procFwpmFilterAdd0, uintptr(s.h), uintptr(unsafe.Pointer(flt)), 0, uintptr(unsafe.Pointer(&id)))
	runtime.KeepAlive(flt)
	runtime.KeepAlive(conds)
	if r != 0 {
		return wfpErr("add the probe filter", r)
	}
	return nil
}

// dialOwn listens on 127.0.0.1, dials it and reports whether bytes came back.
func dialOwn(t *testing.T, install func(port uint16) error) (bool, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Write([]byte("ok"))
			c.Close()
		}
	}()
	port := uint16(l.Addr().(*net.TCPAddr).Port)
	if err := install(port); err != nil {
		return false, err
	}
	c, err := net.DialTimeout("tcp", l.Addr().String(), 3*time.Second)
	if err != nil {
		return false, nil
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	b := make([]byte, 2)
	n, _ := c.Read(b)
	return n == 2, nil
}

func TestWFPLoopbackConditionsLive(t *testing.T) {
	tok := win.GetCurrentProcessToken()
	tu, err := tok.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	me := tu.User.Sid.String()
	t.Logf("this process runs as %s", me)
	loop := probeCond{field: fwpmConditionFlags, flags: fwpConditionFlagIsLoopback}
	cases := []struct {
		name  string
		layer win.GUID
		want  bool // the block should match
		conds []probeCond
	}{
		{"connect: user, loopback, remote port", fwpmLayerALEAuthConnectV4, true, []probeCond{
			{field: fwpmConditionALEUserID, sid: me}, loop, {field: fwpmConditionRemotePort, port: true}}},
		{"accept: loopback, local port", fwpmLayerALEAuthRecvAcceptV4, true, []probeCond{
			loop, {field: fwpmConditionLocalPort, port: true}}},
		{"accept: listener user, local port", fwpmLayerALEAuthRecvAcceptV4, true, []probeCond{
			{field: fwpmConditionALEUserID, sid: me}, {field: fwpmConditionLocalPort, port: true}}},
		// Microsoft's ALE docs: the remote user comes from IPsec
		// credentials. Loopback has none, so these never match.
		{"accept: remote user, local port", fwpmLayerALEAuthRecvAcceptV4, false, []probeCond{
			{field: fwpmConditionALERemoteUserID, sid: me}, {field: fwpmConditionLocalPort, port: true}}},
		{"accept: remote user Everyone, local port", fwpmLayerALEAuthRecvAcceptV4, false, []probeCond{
			{field: fwpmConditionALERemoteUserID, sid: "S-1-1-0"}, {field: fwpmConditionLocalPort, port: true}}},
	}
	s, err := wfpOpen()
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if err := s.tx(func() error {
		if err := s.addProvider(); err != nil {
			return err
		}
		return s.addSublayer()
	}); err != nil {
		t.Fatal(err)
	}
	key := wfpFilterKey(wfpSlots - 1)
	defer s.deleteFilter(key)
	var report []string
	for _, c := range cases {
		answered, err := dialOwn(t, func(port uint16) error {
			return s.tx(func() error {
				if err := s.deleteFilter(key); err != nil {
					return err
				}
				return s.addProbe(key, c.layer, port, c.conds)
			})
		})
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		verdict := "BLOCKED (the conditions matched)"
		if answered {
			verdict = "ANSWERED (the conditions did not match)"
		}
		report = append(report, fmt.Sprintf("PROBE %s: %s", c.name, verdict))
		if answered == c.want {
			t.Errorf("%s: %s", c.name, verdict)
		}
		if err := s.deleteFilter(key); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range report {
		t.Log(r)
	}
}
