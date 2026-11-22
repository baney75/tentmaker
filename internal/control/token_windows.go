//go:build windows

package control

import (
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// protectedDACL grants SYSTEM and Administrators full control and blocks
// inheritance, the same DACL internal/secrets puts on its files.
const protectedDACL = "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"

// writeToken writes the token atomically like config.WriteAtomic, but the
// temp file gets the protected DACL when it is created, so the token is
// never readable through the state dir's inherited ACL, even for a moment.
func writeToken(path string, data []byte) error {
	sd, err := windows.SecurityDescriptorFromString(protectedDACL)
	if err != nil {
		return fmt.Errorf("build security descriptor: %w", err)
	}
	sa := &windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	tmp := filepath.Join(filepath.Dir(path), ".tmp-"+filepath.Base(path))
	_ = os.Remove(tmp)
	p, err := windows.UTF16PtrFromString(tmp)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_WRITE, 0, sa, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return fmt.Errorf("create %s: %w", tmp, err)
	}
	f := os.NewFile(uintptr(h), tmp)
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// A rename on one volume keeps the file's own DACL.
	return os.Rename(tmp, path)
}
