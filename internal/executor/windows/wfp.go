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
// Windows Firewall's:
//
//   - at the ALE connect layers (IPv4 and IPv6), for sockets of the runner
//     user (FWPM_CONDITION_ALE_USER_ID) whose traffic Windows marks as
//     loopback (FWP_CONDITION_FLAG_IS_LOOPBACK; traffic to any of the
//     host's own addresses is loopback traffic),
//   - permit the source relay (TCP 127.0.0.1:RelayPort) and DNS (port 53)
//     to the configured DNS servers, then
//   - block everything else, with FWPM_FILTER_FLAG_CLEAR_ACTION_RIGHT so no
//     later sublayer can permit it again.
//
// Every object is persistent, so the filters are in force from boot,
// before the service starts. Traffic to other hosts is not loopback and is
// left to the Windows Firewall rules (netrules.go).

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

// WFPFilter is one filter at the ALE connect layer. Every filter matches
// only sockets of user SID and only loopback traffic (traffic to this host).
type WFPFilter struct {
	Key         GUID
	Name        string
	V6          bool
	Permit      bool // else block, which no later sublayer may override
	Weight      uint8
	SID         string
	Protocol    uint8        // 6 TCP, 17 UDP, 0 any
	RemotePort  uint16       // 0 any
	RemoteAddrs []netip.Addr // any of them; none means any address
}

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
	s := fmt.Sprintf("tentmaker %s user %s: %s %s loopback weight %d", wfpVersion, f.SID, act, fam, f.Weight)
	if f.Protocol != 0 {
		s += fmt.Sprintf(" proto %d", f.Protocol)
	}
	if f.RemotePort != 0 {
		s += fmt.Sprintf(" port %d", f.RemotePort)
	}
	if len(f.RemoteAddrs) > 0 {
		var as []string
		for _, a := range f.RemoteAddrs {
			as = append(as, a.String())
		}
		s += " to " + strings.Join(as, " ")
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
	fs := []WFPFilter{{Key: wfpFilterKey(0), Name: "Tentmaker: CI jobs reach the source archive relay",
		Permit: true, Weight: 15, SID: sid, Protocol: 6, RemotePort: RelayPort, RemoteAddrs: []netip.Addr{Loopback}}}
	for i, fam := range []struct {
		v6 bool
		rs []netip.Addr
	}{{false, v4}, {true, v6}} {
		if len(fam.rs) == 0 {
			continue
		}
		for j, proto := range []uint8{17, 6} {
			fs = append(fs, WFPFilter{Key: wfpFilterKey(1 + 2*i + j), Name: "Tentmaker: CI jobs reach DNS servers on this host",
				V6: fam.v6, Permit: true, Weight: 14, SID: sid, Protocol: proto, RemotePort: 53, RemoteAddrs: fam.rs})
		}
	}
	for i, v6 := range []bool{false, true} {
		fs = append(fs, WFPFilter{Key: wfpFilterKey(5 + i), Name: "Tentmaker: CI jobs never reach this host",
			V6: v6, Weight: 1, SID: sid})
	}
	return fs
}

// WFPState is one of Tentmaker's filters as WFP has it.
type WFPState struct {
	Key         GUID
	Description string
	V6          bool // at the IPv6 connect layer; false is IPv4
	OtherLayer  bool // at neither connect layer
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
		case h.Description != w.Description(), h.OtherLayer, h.V6 != w.V6, !h.Sublayer, !h.Persistent,
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
