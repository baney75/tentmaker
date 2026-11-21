package windows

import (
	"fmt"
	"net/netip"
	"strings"
)

// Windows Firewall exempts traffic to the host itself from its rules: a
// connection to 127.0.0.1, ::1 or any of the host's own unicast addresses
// is let through even when a block rule names that address (a live attack
// run proved it). So Tentmaker adds its own Windows Filtering Platform
// filters for that traffic, in a sublayer of its own that is weighed before
// Windows Firewall's.
//
// A job must not reach the host's own services, but it must reach its own:
// a test that starts a server on 127.0.0.1 or ::1 (httptest, a test
// database, a Gradle or Bazel daemon, a Playwright driver) and connects to
// it, as on GitHub's runners. The connect side can't tell the two apart (it
// doesn't know who listens), so the fence is on the listening side, at the
// ALE receive/accept layers (IPv4 and IPv6), for loopback traffic
// (FWP_CONDITION_FLAG_IS_LOOPBACK; traffic to any of the host's own
// addresses is loopback traffic) whose remote user, the one who connected,
// is the runner user (FWPM_CONDITION_ALE_REMOTE_USER_ID):
//
//   - permit it when the listening socket is the runner user's too
//     (FWPM_CONDITION_ALE_USER_ID): the job's own listeners,
//   - permit the source relay (TCP 127.0.0.1:RelayPort) and DNS (port 53)
//     on the configured DNS servers, then
//   - block everything else, with FWPM_FILTER_FLAG_CLEAR_ACTION_RIGHT so no
//     later sublayer can permit it again.
//
// Two more hard blocks, at the ALE connect layers and not limited to
// loopback traffic, take multicast and broadcast (224.0.0.0/4,
// 255.255.255.255, ff00::/8) from runner sockets: Windows Firewall rules
// don't, and a live run saw a job's datagram to 239.77.77.77 answered.
//
// Every object is persistent, so the filters are in force from boot,
// before the service starts. Other traffic to other hosts is not loopback
// and is left to the Windows Firewall rules (netrules.go).

// GUID is a Windows GUID, laid out like golang.org/x/sys/windows.GUID.
type GUID struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

func (g GUID) String() string {
	return fmt.Sprintf("%08x-%04x-%04x-%02x%02x-%x", g.Data1, g.Data2, g.Data3, g.Data4[0], g.Data4[1], g.Data4[2:])
}

const (
	wfpVersion = "wfp-v1"
	// WFPSublayerWeight puts Tentmaker's sublayer before Windows Firewall's.
	WFPSublayerWeight = 0xffff
	// wfpSlots is how many filter keys Tentmaker owns: filter i has key
	// wfpFilterKey(i). Setup deletes them all before it adds the ones it
	// wants, in one transaction.
	wfpSlots = 16
)

var (
	// WFPProvider and WFPSublayer are Tentmaker's own WFP objects.
	WFPProvider = GUID{0xc50423c3, 0x319b, 0x4ffd, [8]byte{0x9a, 0x81, 0x43, 0x41, 0x9b, 0x07, 0x9e, 0x43}}
	WFPSublayer = GUID{0x9e28beb7, 0x4b6d, 0x4f27, [8]byte{0xb2, 0x6b, 0x17, 0xe2, 0xd2, 0xfa, 0x8e, 0xa5}}
	wfpKeyBase  = GUID{0x942e7d95, 0x0bbd, 0x4946, [8]byte{0x92, 0xb9, 0xd2, 0xe2, 0x7c, 0xc8, 0x62, 0x00}}
)

// wfpFilterKey is the key of Tentmaker's filter in slot i.
func wfpFilterKey(i int) GUID {
	k := wfpKeyBase
	k.Data4[7] = byte(i)
	return k
}

// WFPKeys is every key Tentmaker may own.
func WFPKeys() []GUID {
	out := make([]GUID, wfpSlots)
	for i := range out {
		out[i] = wfpFilterKey(i)
	}
	return out
}

// WFPFilter is one filter at the ALE connect layers or, when Accept, at
// the ALE receive/accept layers. Every filter matches only traffic of user
// SID, and only loopback traffic (traffic to this host) unless AnyHost.
type WFPFilter struct {
	Key  GUID
	Name string
	// Accept puts the filter where a listening socket accepts a connection
	// or a first datagram; else it is where a socket connects or sends.
	Accept  bool
	V6      bool
	Permit  bool // else block, which no later sublayer may override
	AnyHost bool // traffic to any host, not only loopback traffic
	Weight  uint8
	// SID is the runner user. At connect it is the user of the socket that
	// connects (FWPM_CONDITION_ALE_USER_ID); at accept it is the remote
	// user, the one who connected (FWPM_CONDITION_ALE_REMOTE_USER_ID).
	SID string
	// ToRunner (Accept only): the listening socket's user is SID too.
	ToRunner bool
	Protocol uint8 // 6 TCP, 17 UDP, 0 any
	// The listening end: the remote end at connect, the local end at
	// accept. Port 0 is any port; no DstAddrs and no DstPrefixes is any
	// address, else any of them.
	DstPort     uint16
	DstAddrs    []netip.Addr
	DstPrefixes []netip.Prefix
}

// Multicast and broadcast destinations, blocked for jobs on every host.
var (
	MulticastV4 = []netip.Prefix{netip.MustParsePrefix("224.0.0.0/4"), netip.MustParsePrefix("255.255.255.255/32")}
	MulticastV6 = []netip.Prefix{netip.MustParsePrefix("ff00::/8")}
)

// Description is written on the filter. It spells out every condition, so
// comparing it with what is installed compares the filters.
func (f WFPFilter) Description() string {
	act := "block"
	if f.Permit {
		act = "permit"
	}
	fam := "v4"
	if f.V6 {
		fam = "v6"
	}
	scope := "loopback"
	if f.AnyHost {
		scope = "any host"
	}
	layer := "connect"
	if f.Accept {
		layer = "accept"
	}
	s := fmt.Sprintf("tentmaker %s user %s: %s %s %s %s weight %d", wfpVersion, f.SID, act, layer, fam, scope, f.Weight)
	if f.ToRunner {
		s += " listener the same user"
	}
	if f.Protocol != 0 {
		s += fmt.Sprintf(" proto %d", f.Protocol)
	}
	if f.DstPort != 0 {
		s += fmt.Sprintf(" port %d", f.DstPort)
	}
	var to []string
	for _, a := range f.DstAddrs {
		to = append(to, a.String())
	}
	for _, p := range f.DstPrefixes {
		to = append(to, p.String())
	}
	if len(to) > 0 {
		s += " to " + strings.Join(to, " ")
	}
	return s
}

// WFPFilters is every filter Tentmaker installs for the runner user sid.
func WFPFilters(sid string, resolvers []netip.Addr) []WFPFilter {
	var v4, v6 []netip.Addr
	for _, r := range resolvers {
		if r = r.Unmap(); r.Is4() {
			v4 = append(v4, r)
		} else if r.Zone() == "" {
			v6 = append(v6, r)
		}
	}
	var fs []WFPFilter
	for i, v6 := range []bool{false, true} {
		fs = append(fs, WFPFilter{Key: wfpFilterKey(i), Name: "Tentmaker: CI jobs reach their own listeners on this host",
			Accept: true, V6: v6, Permit: true, Weight: 15, SID: sid, ToRunner: true})
	}
	fs = append(fs, WFPFilter{Key: wfpFilterKey(2), Name: "Tentmaker: CI jobs reach the source archive relay",
		Accept: true, Permit: true, Weight: 14, SID: sid, Protocol: 6, DstPort: RelayPort, DstAddrs: []netip.Addr{Loopback}})
	for i, fam := range []struct {
		v6 bool
		rs []netip.Addr
	}{{false, v4}, {true, v6}} {
		if len(fam.rs) == 0 {
			continue
		}
		for j, proto := range []uint8{17, 6} {
			fs = append(fs, WFPFilter{Key: wfpFilterKey(3 + 2*i + j), Name: "Tentmaker: CI jobs reach DNS servers on this host",
				Accept: true, V6: fam.v6, Permit: true, Weight: 13, SID: sid, Protocol: proto, DstPort: 53, DstAddrs: fam.rs})
		}
	}
	for i, ps := range [][]netip.Prefix{MulticastV4, MulticastV6} {
		fs = append(fs, WFPFilter{Key: wfpFilterKey(7 + i), Name: "Tentmaker: CI jobs send no multicast or broadcast",
			V6: i == 1, AnyHost: true, Weight: 1, SID: sid, DstPrefixes: ps})
	}
	for i, v6 := range []bool{false, true} {
		fs = append(fs, WFPFilter{Key: wfpFilterKey(9 + i), Name: "Tentmaker: CI jobs never reach this host's services",
			Accept: true, V6: v6, Weight: 1, SID: sid})
	}
	return fs
}

// WFPState is one of Tentmaker's filters as WFP has it.
type WFPState struct {
	Key         GUID
	Description string
	Accept      bool // at an ALE receive/accept layer; false is a connect layer
	V6          bool // at the IPv6 layer; false is IPv4
	OtherLayer  bool // at none of the four ALE layers Tentmaker uses
	Permit      bool
	Block       bool
	Persistent  bool
	Hard        bool // FWPM_FILTER_FLAG_CLEAR_ACTION_RIGHT
	Sublayer    bool // in Tentmaker's sublayer
}

// checkWFP compares the filters in place with want. A difference is
// *errStaleNet when every filter in place was written for this user by
// this version (the DNS servers changed, or a filter was removed), so
// Start puts them back; anything else, including no filters at all, is a
// plain error.
func checkWFP(want []WFPFilter, have []WFPState) error {
	if len(have) == 0 {
		return fmt.Errorf("the WFP filters that keep CI jobs off this host are missing")
	}
	sid := ""
	if len(want) > 0 {
		sid = want[0].SID
	}
	prefix := fmt.Sprintf("tentmaker %s user %s: ", wfpVersion, sid)
	byKey := map[GUID]WFPState{}
	for _, h := range have {
		if !strings.HasPrefix(h.Description, prefix) {
			return fmt.Errorf("the WFP filter %s is for another user or version (%s)", h.Key, trunc(h.Description, 200))
		}
		byKey[h.Key] = h
	}
	var stale []string
	for _, w := range want {
		h, ok := byKey[w.Key]
		delete(byKey, w.Key)
		switch {
		case !ok:
			stale = append(stale, "missing "+w.Description())
		case h.Description != w.Description(), h.OtherLayer, h.Accept != w.Accept, h.V6 != w.V6, !h.Sublayer, !h.Persistent,
			w.Permit && !h.Permit, !w.Permit && (!h.Block || !h.Hard):
			stale = append(stale, fmt.Sprintf("%s is not %s", h.Key, w.Description()))
		}
	}
	for k, h := range byKey {
		stale = append(stale, fmt.Sprintf("extra %s %s", k, h.Description))
	}
	if len(stale) > 0 {
		return &errStaleNet{"WFP: " + strings.Join(stale, "; ")}
	}
	return nil
}
