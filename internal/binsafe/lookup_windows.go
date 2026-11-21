//go:build windows

package binsafe

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var hostJudge Judge = Windows

func hostLookup(path string) (Entry, error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return Entry{}, fmt.Errorf("read the security of %s: %w", path, err)
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil {
		return Entry{}, errors.New("can't read the owner of " + path)
	}
	e := Entry{Owner: owner.String()}
	dacl, _, err := sd.DACL()
	if errors.Is(err, windows.ERROR_OBJECT_NOT_FOUND) || (err == nil && dacl == nil) {
		// No DACL, or a null one: everyone has every right.
		e.ACEs = []ACE{{SID: SIDEveryone, Mask: 0xffffffff}}
		return e, nil
	}
	if err != nil {
		return Entry{}, fmt.Errorf("read the access list of %s: %w", path, err)
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return Entry{}, fmt.Errorf("read the access list of %s: %w", path, err)
		}
		a := ACE{InheritOnly: ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0}
		switch ace.Header.AceType {
		case windows.ACCESS_ALLOWED_ACE_TYPE:
			a.Mask = uint32(ace.Mask)
			a.SID = (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()
		case windows.ACCESS_DENIED_ACE_TYPE, 6, 10, 12: // denied, denied object, denied callback (object)
			a.Deny = true
		default:
			a.Unknown, a.SID = true, fmt.Sprintf("an access entry of type %d", ace.Header.AceType)
		}
		e.ACEs = append(e.ACEs, a)
	}
	return e, nil
}
