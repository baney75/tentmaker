//go:build windows

package binsafe

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// OwnNewFiles makes Administrators the owner of every file and folder this
// process creates from now on. By default Windows makes the elevated user
// the owner, and an owner can always rewrite the DACL, so that user's
// unelevated processes could change a folder or binary setup made for the
// SYSTEM service, and Windows judges it a problem. Setting the token's
// default owner, rather than changing the owner after the create, leaves no
// moment in which the user owns the new object. It fails when the process
// is not elevated.
func OwnNewFiles() error {
	var tok windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_ADJUST_DEFAULT|windows.TOKEN_QUERY, &tok); err != nil {
		return fmt.Errorf("open this process's token: %w", err)
	}
	defer tok.Close()
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return fmt.Errorf("make the Administrators SID: %w", err)
	}
	owner := struct{ Owner *windows.SID }{admins}
	if err := windows.SetTokenInformation(tok, windows.TokenOwner, (*byte)(unsafe.Pointer(&owner)), uint32(unsafe.Sizeof(owner))); err != nil {
		return fmt.Errorf("make Administrators the owner of new files: %w", err)
	}
	return nil
}
