//go:build windows

package windows

import (
	"encoding/binary"
	"fmt"
	"runtime"
	"unsafe"

	win "golang.org/x/sys/windows"
)

// The Windows Filtering Platform calls behind SetWFP, WFPInstalled and
// RemoveWFP (wfp.go). Constants, GUIDs and struct layouts are from the
// Windows SDK's fwpmu.h, fwpmtypes.h and fwptypes.h; wfp_layout_windows.go
// checks the layouts at compile time.

var (
	fwpuclnt                  = win.NewLazySystemDLL("fwpuclnt.dll")
	procFwpmEngineOpen0       = fwpuclnt.NewProc("FwpmEngineOpen0")
	procFwpmEngineClose0      = fwpuclnt.NewProc("FwpmEngineClose0")
	procFwpmTransactionBegin0 = fwpuclnt.NewProc("FwpmTransactionBegin0")
	procFwpmTransactionCommit = fwpuclnt.NewProc("FwpmTransactionCommit0")
	procFwpmTransactionAbort0 = fwpuclnt.NewProc("FwpmTransactionAbort0")
	procFwpmProviderAdd0      = fwpuclnt.NewProc("FwpmProviderAdd0")
	procFwpmProviderDelete0   = fwpuclnt.NewProc("FwpmProviderDeleteByKey0")
	procFwpmSubLayerAdd0      = fwpuclnt.NewProc("FwpmSubLayerAdd0")
	procFwpmSubLayerDelete0   = fwpuclnt.NewProc("FwpmSubLayerDeleteByKey0")
	procFwpmFilterAdd0        = fwpuclnt.NewProc("FwpmFilterAdd0")
	procFwpmFilterDelete0     = fwpuclnt.NewProc("FwpmFilterDeleteByKey0")
	procFwpmFilterGetByKey0   = fwpuclnt.NewProc("FwpmFilterGetByKey0")
	procFwpmFreeMemory0       = fwpuclnt.NewProc("FwpmFreeMemory0")
)

const (
	rpcCAuthnDefault = 0xffffffff

	fwpUint8              = 1
	fwpUint16             = 2
	fwpUint32             = 3
	fwpByteArray16        = 11
	fwpSecurityDescriptor = 14

	fwpMatchEqual       = 0
	fwpMatchFlagsAllSet = 6

	fwpConditionFlagIsLoopback = 0x1

	fwpActionBlock  = 0x1001
	fwpActionPermit = 0x1002

	fwpmFilterFlagPersistent       = 0x1
	fwpmFilterFlagClearActionRight = 0x8
	fwpmProviderFlagPersistent     = 0x1
	fwpmSublayerFlagPersistent     = 0x1

	fwpEFilterNotFound   = 0x80320003
	fwpEProviderNotFound = 0x80320005
	fwpESublayerNotFound = 0x80320007
	fwpEAlreadyExists    = 0x80320009

	// wfpTxnTimeout bounds how long a transaction waits for the engine.
	wfpTxnTimeoutMS = 30000
)

var (
	fwpmLayerALEAuthConnectV4 = win.GUID{Data1: 0xc38d57d1, Data2: 0x05a7, Data3: 0x4c33, Data4: [8]byte{0x90, 0x4f, 0x7f, 0xbc, 0xee, 0xe6, 0x0e, 0x82}}
	fwpmLayerALEAuthConnectV6 = win.GUID{Data1: 0x4a72393b, Data2: 0x319f, Data3: 0x44bc, Data4: [8]byte{0x84, 0xc3, 0xba, 0x54, 0xdc, 0xb3, 0xb6, 0xb4}}
	fwpmConditionALEUserID    = win.GUID{Data1: 0xaf043a0a, Data2: 0xb34d, Data3: 0x4f86, Data4: [8]byte{0x97, 0x9c, 0xc9, 0x03, 0x71, 0xaf, 0x6e, 0x66}}
	fwpmConditionFlags        = win.GUID{Data1: 0x632ce23b, Data2: 0x5167, Data3: 0x435c, Data4: [8]byte{0x86, 0xd7, 0xe9, 0x03, 0x68, 0x4a, 0xa8, 0x0c}}
	fwpmConditionRemoteAddr   = win.GUID{Data1: 0xb235ae9a, Data2: 0x1d64, Data3: 0x49b8, Data4: [8]byte{0xa4, 0x4c, 0x5f, 0xf3, 0xd9, 0x09, 0x50, 0x45}}
	fwpmConditionRemotePort   = win.GUID{Data1: 0xc35a604d, Data2: 0xd22b, Data3: 0x4e1a, Data4: [8]byte{0x91, 0xb4, 0x68, 0xf6, 0x74, 0xee, 0x67, 0x4b}}
	fwpmConditionIPProtocol   = win.GUID{Data1: 0x3971ef2b, Data2: 0x623e, Data3: 0x4f9a, Data4: [8]byte{0x8c, 0xb1, 0x6e, 0x79, 0xb8, 0x06, 0xb9, 0xa7}}
)

// FWP_VALUE0 and FWP_CONDITION_VALUE0: a type and a pointer-sized union.
type fwpValue0 struct {
	Type  uint32
	Value uintptr
}

type fwpmDisplayData0 struct {
	Name        *uint16
	Description *uint16
}

type fwpByteBlob struct {
	Size uint32
	Data *byte
}

type fwpmFilterCondition0 struct {
	FieldKey  win.GUID
	MatchType uint32
	Value     fwpValue0
}

type fwpmAction0 struct {
	Type uint32
	Key  win.GUID
}

type fwpmFilter0 struct {
	FilterKey           win.GUID
	DisplayData         fwpmDisplayData0
	Flags               uint32
	ProviderKey         *win.GUID
	ProviderData        fwpByteBlob
	LayerKey            win.GUID
	SubLayerKey         win.GUID
	Weight              fwpValue0
	NumFilterConditions uint32
	FilterCondition     *fwpmFilterCondition0
	Action              fwpmAction0
	ProviderContextKey  [2]uint64 // a union with rawContext; 8-aligned
	Reserved            *win.GUID
	FilterID            uint64
	EffectiveWeight     fwpValue0
}

type fwpmSession0 struct {
	SessionKey           win.GUID
	DisplayData          fwpmDisplayData0
	Flags                uint32
	TxnWaitTimeoutInMSec uint32
	ProcessID            uint32
	SID                  *win.SID
	Username             *uint16
	KernelMode           int32
}

type fwpmProvider0 struct {
	ProviderKey  win.GUID
	DisplayData  fwpmDisplayData0
	Flags        uint32
	ProviderData fwpByteBlob
	ServiceName  *uint16
}

type fwpmSublayer0 struct {
	SubLayerKey  win.GUID
	DisplayData  fwpmDisplayData0
	Flags        uint32
	ProviderKey  *win.GUID
	ProviderData fwpByteBlob
	Weight       uint16
}

func wfpErr(what string, r uintptr) error {
	return fmt.Errorf("%s: %w (0x%08x)", what, win.Errno(r), uint32(r))
}

// wfpSession is an open engine handle.
type wfpSession struct{ h win.Handle }

func wfpOpen() (*wfpSession, error) {
	if err := fwpuclnt.Load(); err != nil {
		return nil, fmt.Errorf("load fwpuclnt.dll: %w", err)
	}
	for _, p := range []*win.LazyProc{procFwpmEngineOpen0, procFwpmEngineClose0, procFwpmTransactionBegin0, procFwpmTransactionCommit,
		procFwpmTransactionAbort0, procFwpmProviderAdd0, procFwpmProviderDelete0, procFwpmSubLayerAdd0, procFwpmSubLayerDelete0,
		procFwpmFilterAdd0, procFwpmFilterDelete0, procFwpmFilterGetByKey0, procFwpmFreeMemory0} {
		if err := p.Find(); err != nil {
			return nil, err
		}
	}
	s := &fwpmSession0{TxnWaitTimeoutInMSec: wfpTxnTimeoutMS}
	var h win.Handle
	r, _, _ := procFwpmEngineOpen0.Call(0, rpcCAuthnDefault, 0, uintptr(unsafe.Pointer(s)), uintptr(unsafe.Pointer(&h)))
	if r != 0 {
		return nil, wfpErr("open the WFP engine", r)
	}
	return &wfpSession{h}, nil
}

func (s *wfpSession) close() { procFwpmEngineClose0.Call(uintptr(s.h)) }

// tx runs f in one transaction: all of it takes effect, or none.
func (s *wfpSession) tx(f func() error) error {
	if r, _, _ := procFwpmTransactionBegin0.Call(uintptr(s.h), 0); r != 0 {
		return wfpErr("begin a WFP transaction", r)
	}
	if err := f(); err != nil {
		procFwpmTransactionAbort0.Call(uintptr(s.h))
		return err
	}
	if r, _, _ := procFwpmTransactionCommit.Call(uintptr(s.h)); r != 0 {
		return wfpErr("commit the WFP transaction", r)
	}
	return nil
}

func (s *wfpSession) addProvider() error {
	key := win.GUID(WFPProvider)
	p := &fwpmProvider0{ProviderKey: key, Flags: fwpmProviderFlagPersistent,
		DisplayData: fwpmDisplayData0{Name: u16("Tentmaker"), Description: u16("Tentmaker keeps CI jobs off this host.")}}
	r, _, _ := procFwpmProviderAdd0.Call(uintptr(s.h), uintptr(unsafe.Pointer(p)), 0)
	runtime.KeepAlive(p)
	if r != 0 && r != fwpEAlreadyExists {
		return wfpErr("add the Tentmaker WFP provider", r)
	}
	return nil
}

func (s *wfpSession) addSublayer() error {
	prov := win.GUID(WFPProvider)
	l := &fwpmSublayer0{SubLayerKey: win.GUID(WFPSublayer), Flags: fwpmSublayerFlagPersistent, ProviderKey: &prov, Weight: WFPSublayerWeight,
		DisplayData: fwpmDisplayData0{Name: u16("Tentmaker"), Description: u16("Tentmaker's filters for CI jobs, weighed before Windows Firewall's.")}}
	r, _, _ := procFwpmSubLayerAdd0.Call(uintptr(s.h), uintptr(unsafe.Pointer(l)), 0)
	runtime.KeepAlive(l)
	if r != 0 && r != fwpEAlreadyExists {
		return wfpErr("add the Tentmaker WFP sublayer", r)
	}
	return nil
}

func (s *wfpSession) deleteFilter(k GUID) error {
	key := win.GUID(k)
	r, _, _ := procFwpmFilterDelete0.Call(uintptr(s.h), uintptr(unsafe.Pointer(&key)))
	if r != 0 && r != fwpEFilterNotFound {
		return wfpErr("delete WFP filter "+k.String(), r)
	}
	return nil
}

// addFilter adds f. Everything the filter points to through a uintptr
// union is pinned, so it stays put and alive until the call returns.
func (s *wfpSession) addFilter(f WFPFilter) error {
	var pin runtime.Pinner
	defer pin.Unpin()
	sd, err := win.SecurityDescriptorFromString("D:(A;;CC;;;" + f.SID + ")")
	if err != nil {
		return fmt.Errorf("the security descriptor for %s: %w", f.SID, err)
	}
	pin.Pin(sd)
	blob := &fwpByteBlob{Size: sd.Length(), Data: (*byte)(unsafe.Pointer(sd))}
	pin.Pin(blob)
	conds := []fwpmFilterCondition0{
		{FieldKey: fwpmConditionALEUserID, MatchType: fwpMatchEqual, Value: fwpValue0{Type: fwpSecurityDescriptor, Value: uintptr(unsafe.Pointer(blob))}},
		{FieldKey: fwpmConditionFlags, MatchType: fwpMatchFlagsAllSet, Value: fwpValue0{Type: fwpUint32, Value: fwpConditionFlagIsLoopback}},
	}
	if f.Protocol != 0 {
		conds = append(conds, fwpmFilterCondition0{FieldKey: fwpmConditionIPProtocol, MatchType: fwpMatchEqual, Value: fwpValue0{Type: fwpUint8, Value: uintptr(f.Protocol)}})
	}
	if f.RemotePort != 0 {
		conds = append(conds, fwpmFilterCondition0{FieldKey: fwpmConditionRemotePort, MatchType: fwpMatchEqual, Value: fwpValue0{Type: fwpUint16, Value: uintptr(f.RemotePort)}})
	}
	// Conditions on one field match any of their values.
	for _, a := range f.RemoteAddrs {
		if f.V6 != a.Is6() {
			return fmt.Errorf("WFP filter %s: %s is the wrong family", f.Key, a)
		}
		if a.Is4() {
			b := a.As4()
			conds = append(conds, fwpmFilterCondition0{FieldKey: fwpmConditionRemoteAddr, MatchType: fwpMatchEqual, Value: fwpValue0{Type: fwpUint32, Value: uintptr(binary.BigEndian.Uint32(b[:]))}})
			continue
		}
		b := new([16]byte)
		*b = a.As16()
		pin.Pin(b)
		conds = append(conds, fwpmFilterCondition0{FieldKey: fwpmConditionRemoteAddr, MatchType: fwpMatchEqual, Value: fwpValue0{Type: fwpByteArray16, Value: uintptr(unsafe.Pointer(b))}})
	}
	prov := win.GUID(WFPProvider)
	flt := &fwpmFilter0{
		FilterKey:           win.GUID(f.Key),
		DisplayData:         fwpmDisplayData0{Name: u16(f.Name), Description: u16(f.Description())},
		Flags:               fwpmFilterFlagPersistent,
		ProviderKey:         &prov,
		LayerKey:            fwpmLayerALEAuthConnectV4,
		SubLayerKey:         win.GUID(WFPSublayer),
		Weight:              fwpValue0{Type: fwpUint8, Value: uintptr(f.Weight)},
		NumFilterConditions: uint32(len(conds)),
		FilterCondition:     &conds[0],
		Action:              fwpmAction0{Type: fwpActionPermit},
	}
	if f.V6 {
		flt.LayerKey = fwpmLayerALEAuthConnectV6
	}
	if !f.Permit {
		flt.Action.Type = fwpActionBlock
		flt.Flags |= fwpmFilterFlagClearActionRight
	}
	var id uint64
	r, _, _ := procFwpmFilterAdd0.Call(uintptr(s.h), uintptr(unsafe.Pointer(flt)), 0, uintptr(unsafe.Pointer(&id)))
	runtime.KeepAlive(flt)
	runtime.KeepAlive(conds)
	if r != 0 {
		return wfpErr("add WFP filter "+f.Description(), r)
	}
	return nil
}

// SetWFP implements API.
func (*OSAPI) SetWFP(fs []WFPFilter) error {
	s, err := wfpOpen()
	if err != nil {
		return err
	}
	defer s.close()
	return s.tx(func() error {
		if err := s.addProvider(); err != nil {
			return err
		}
		if err := s.addSublayer(); err != nil {
			return err
		}
		for _, k := range WFPKeys() {
			if err := s.deleteFilter(k); err != nil {
				return err
			}
		}
		for _, f := range fs {
			if err := s.addFilter(f); err != nil {
				return err
			}
		}
		return nil
	})
}

// WFPInstalled implements API.
func (*OSAPI) WFPInstalled() ([]WFPState, error) {
	s, err := wfpOpen()
	if err != nil {
		return nil, err
	}
	defer s.close()
	var out []WFPState
	for _, k := range WFPKeys() {
		key := win.GUID(k)
		var f *fwpmFilter0
		r, _, _ := procFwpmFilterGetByKey0.Call(uintptr(s.h), uintptr(unsafe.Pointer(&key)), uintptr(unsafe.Pointer(&f)))
		if r == fwpEFilterNotFound {
			continue
		}
		if r != 0 {
			return nil, wfpErr("read WFP filter "+k.String(), r)
		}
		st := WFPState{
			Key:         k,
			Description: win.UTF16PtrToString(f.DisplayData.Description),
			V6:          f.LayerKey == fwpmLayerALEAuthConnectV6,
			OtherLayer:  f.LayerKey != fwpmLayerALEAuthConnectV4 && f.LayerKey != fwpmLayerALEAuthConnectV6,
			Permit:      f.Action.Type == fwpActionPermit,
			Block:       f.Action.Type == fwpActionBlock,
			Persistent:  f.Flags&fwpmFilterFlagPersistent != 0,
			Hard:        f.Flags&fwpmFilterFlagClearActionRight != 0,
			Sublayer:    f.SubLayerKey == win.GUID(WFPSublayer),
		}
		procFwpmFreeMemory0.Call(uintptr(unsafe.Pointer(&f)))
		out = append(out, st)
	}
	return out, nil
}

// RemoveWFP implements API.
func (*OSAPI) RemoveWFP() error {
	s, err := wfpOpen()
	if err != nil {
		return err
	}
	defer s.close()
	return s.tx(func() error {
		for _, k := range WFPKeys() {
			if err := s.deleteFilter(k); err != nil {
				return err
			}
		}
		sub, prov := win.GUID(WFPSublayer), win.GUID(WFPProvider)
		if r, _, _ := procFwpmSubLayerDelete0.Call(uintptr(s.h), uintptr(unsafe.Pointer(&sub))); r != 0 && r != fwpESublayerNotFound {
			return wfpErr("delete the Tentmaker WFP sublayer", r)
		}
		if r, _, _ := procFwpmProviderDelete0.Call(uintptr(s.h), uintptr(unsafe.Pointer(&prov))); r != 0 && r != fwpEProviderNotFound {
			return wfpErr("delete the Tentmaker WFP provider", r)
		}
		return nil
	})
}
