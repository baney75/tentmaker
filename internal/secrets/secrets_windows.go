//go:build windows

package secrets

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/baney75/tentmaker/internal/config"
)

// DefaultDir is where Default keeps secrets on this OS.
func DefaultDir() string {
	pd := os.Getenv("ProgramData")
	if pd == "" {
		pd = `C:\ProgramData`
	}
	return filepath.Join(pd, "Tentmaker", "secrets")
}

// Dir returns a store that keeps one DPAPI-encrypted file per secret in dir.
// The directory and each file get a protected DACL that grants only SYSTEM
// and Administrators, so the caller must run elevated.
func Dir(dir string) Store { return &dpapiStore{dir: dir} }

type dpapiStore struct{ dir string }

// protectedDACL: Administrators own it; SYSTEM and Administrators get full
// control, inherited by files and subfolders; P blocks inheritance from the
// parent. The owner matters: a user who made the folder before setup would
// otherwise stay its owner and could rewrite the DACL (review r1 partS
// first-F3).
const protectedDACL = "O:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"

// magic marks a Tentmaker secret file, version 1.
var magic = []byte("TMS1")

const maxSecret = 1 << 20

func (s *dpapiStore) path(name string) (string, error) {
	if err := checkName(name); err != nil {
		return "", err
	}
	return filepath.Join(s.dir, name), nil
}

func protect(path string) error {
	sd, err := windows.SecurityDescriptorFromString(protectedDACL)
	if err != nil {
		return fmt.Errorf("build security descriptor: %w", err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("read DACL: %w", err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return fmt.Errorf("read owner: %w", err)
	}
	err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		owner, nil, dacl, nil)
	if err != nil {
		return fmt.Errorf("set ACL on %s: %w", path, err)
	}
	return nil
}

func entropy(name string) []byte { return []byte("tentmaker-secret:" + name) }

func blob(b []byte) *windows.DataBlob {
	d := &windows.DataBlob{Size: uint32(len(b))}
	if len(b) > 0 {
		d.Data = &b[0]
	}
	return d
}

func takeBlob(out windows.DataBlob) []byte {
	if out.Data == nil {
		return nil
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return bytes.Clone(unsafe.Slice(out.Data, out.Size))
}

func encrypt(name string, plain []byte) ([]byte, error) {
	var out windows.DataBlob
	err := windows.CryptProtectData(blob(plain), nil, blob(entropy(name)), 0, nil,
		windows.CRYPTPROTECT_UI_FORBIDDEN|windows.CRYPTPROTECT_LOCAL_MACHINE, &out)
	if err != nil {
		return nil, fmt.Errorf("secret %q: DPAPI encrypt: %w", name, err)
	}
	return takeBlob(out), nil
}

func decrypt(name string, sealed []byte) ([]byte, error) {
	var out windows.DataBlob
	err := windows.CryptUnprotectData(blob(sealed), nil, blob(entropy(name)), 0, nil,
		windows.CRYPTPROTECT_UI_FORBIDDEN, &out)
	if err != nil {
		return nil, fmt.Errorf("secret %q: DPAPI decrypt: %w", name, err)
	}
	return takeBlob(out), nil
}

func (s *dpapiStore) Get(name string) ([]byte, error) {
	p, err := s.path(name)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("secret %q: %w", name, ErrNotFound)
		}
		return nil, fmt.Errorf("secret %q: read: %w", name, unwrapPath(err))
	}
	if len(b) > maxSecret+4096 || !bytes.HasPrefix(b, magic) {
		return nil, fmt.Errorf("secret %q: %s is not a Tentmaker secret file", name, p)
	}
	return decrypt(name, b[len(magic):])
}

func (s *dpapiStore) Set(name string, v []byte) error {
	p, err := s.path(name)
	if err != nil {
		return err
	}
	if len(v) > maxSecret {
		return fmt.Errorf("secret %q is larger than %d bytes", name, maxSecret)
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("secret store %s: %w", s.dir, unwrapPath(err))
	}
	if err := protect(s.dir); err != nil {
		return fmt.Errorf("secret store: %w", err)
	}
	sealed, err := encrypt(name, v)
	if err != nil {
		return err
	}
	if err := config.WriteAtomic(p, append(bytes.Clone(magic), sealed...), 0o600); err != nil {
		return fmt.Errorf("secret %q: write: %w", name, unwrapPath(err))
	}
	if err := protect(p); err != nil {
		return fmt.Errorf("secret %q: %w", name, err)
	}
	return nil
}

func (s *dpapiStore) Delete(name string) error {
	p, err := s.path(name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("secret %q: delete: %w", name, unwrapPath(err))
	}
	return nil
}

func unwrapPath(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}
