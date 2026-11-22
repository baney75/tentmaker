// Package binsafe decides whether a root or SYSTEM service may run a binary:
// no one but root (Windows: SYSTEM, Administrators and TrustedInstaller) may
// be able to change the file or any folder above it. A user who could swap
// the binary, or the .prev copy an update rolls back to, would get root on
// the next restart or update.
//
// The judges are pure functions of what a lookup reports, so the rules for
// every OS are tested on any OS; Problem wires in this host's lookup.
package binsafe

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"runtime"
	"strings"
)

// Entry is what a lookup reports for one path.
type Entry struct {
	// Unix.
	UID, GID uint32
	Mode     fs.FileMode
	// Windows: the owner's SID and the DACL's allow entries. A missing or
	// null DACL is one entry for Everyone with every right.
	Owner string
	ACEs  []ACE
}

// ACE is one access control entry of a Windows DACL.
type ACE struct {
	SID         string
	Mask        uint32
	Deny        bool
	InheritOnly bool // applies only to children, not to this object
	Unknown     bool // an allow type this package can't read; counts as every right
}

// Lookup reports the entry for path (Lstat semantics: a symlink is not
// followed).
type Lookup func(path string) (Entry, error)

// Judge says why someone who isn't root could change path ("" when no one
// can). level is 0 for the file, 1 for its folder, 2 and up for folders
// above.
type Judge func(path string, level int, e Entry) string

// Walk runs judge on path and every folder above it, stopping at the first
// problem.
func Walk(path string, look Lookup, judge Judge) (string, error) {
	p := filepath.Clean(path)
	for level := 0; ; level++ {
		e, err := look(p)
		if err != nil {
			return "", err
		}
		if why := judge(p, level, e); why != "" {
			return why, nil
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", nil
		}
		p = parent
	}
}

// Unix judges by owner and mode: the owner must be root, and no one but
// root's group may write.
func Unix(path string, level int, e Entry) string {
	switch {
	case e.Mode&fs.ModeSymlink != 0:
		return path + " is a symbolic link"
	case e.UID != 0:
		return fmt.Sprintf("%s is owned by uid %d, not root", path, e.UID)
	case e.Mode.Perm()&0o002 != 0:
		return path + " is writable by every user"
	case e.Mode.Perm()&0o020 != 0 && e.GID != 0:
		return fmt.Sprintf("%s is writable by group %d", path, e.GID)
	}
	return ""
}

// Well-known Windows SIDs that may change a service binary.
const (
	SIDSystem           = "S-1-5-18"
	SIDAdministrators   = "S-1-5-32-544"
	SIDTrustedInstaller = "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"
	SIDEveryone         = "S-1-1-0"
)

func trustedSID(s string) bool {
	return s == SIDSystem || s == SIDAdministrators || s == SIDTrustedInstaller
}

// Windows access rights.
const (
	fileWriteData   = 0x2 // a folder: add a file
	fileAppendData  = 0x4
	fileDeleteChild = 0x40
	rightDelete     = 0x10000
	rightWriteDAC   = 0x40000
	rightWriteOwner = 0x80000
	genericAll      = 0x10000000
	genericWrite    = 0x40000000

	anyLevel = rightDelete | rightWriteDAC | rightWriteOwner | genericAll | genericWrite
)

// changeRights are the rights that let someone change path at level: the
// file's data, the folder's entries (add a file to plant a staged name,
// delete or rename one), and anywhere: delete, rename, or rewrite the ACL
// or owner. Creating a folder in C:\ (which Authenticated Users may) can't
// touch the path, so above the binary's own folder add rights don't count.
func changeRights(level int) uint32 {
	switch level {
	case 0:
		return anyLevel | fileWriteData | fileAppendData
	case 1:
		return anyLevel | fileWriteData | fileDeleteChild
	}
	return anyLevel | fileDeleteChild
}

// Windows judges by owner and DACL: the owner must be SYSTEM,
// Administrators or TrustedInstaller, and no allow entry that applies to the
// object may give anyone else a right that changes it.
func Windows(path string, level int, e Entry) string {
	if !trustedSID(e.Owner) {
		return fmt.Sprintf("%s is owned by %s, not SYSTEM or Administrators", path, e.Owner)
	}
	need := changeRights(level)
	for _, a := range e.ACEs {
		if a.Deny || a.InheritOnly || trustedSID(a.SID) {
			continue
		}
		if a.Unknown || a.Mask&need != 0 {
			return fmt.Sprintf("%s lets %s change it", path, a.SID)
		}
	}
	return ""
}

// Problem says why someone other than root (Windows: SYSTEM and
// Administrators) could change the binary at path or a folder above it, or
// "" when no one can. path must be canonical (filepath.EvalSymlinks of
// itself): a path that resolves through a symbolic link is a problem even
// when its target passes, because whoever can write the folder holding the
// link can repoint it after the check (user-writable /usr/local with bin
// pointing at a root-owned folder). Callers resolve first and use the
// resolved path for everything the check vouches for (review r2 partB
// sol-second-F7).
func Problem(path string) (string, error) {
	return problem(path, filepath.EvalSymlinks, hostLookup, hostJudge)
}

func problem(path string, eval func(string) (string, error), look Lookup, judge Judge) (string, error) {
	resolved, err := eval(path)
	if err != nil {
		return "", err
	}
	if !SameFile(runtime.GOOS, resolved, path) {
		return fmt.Sprintf("%s resolves through a symbolic link to %s; use that path", path, resolved), nil
	}
	return Walk(resolved, look, judge)
}

// SameFile reports whether a and b name the same path, ignoring case on
// Windows.
func SameFile(goos, a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if goos == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// ServiceDirs are the root-owned folders setup may copy the binary into for
// the service, in order of preference.
func ServiceDirs(goos string, getenv func(string) string) []string {
	if goos == "windows" {
		pf := getenv("ProgramFiles")
		if pf == "" {
			pf = `C:\Program Files`
		}
		return []string{pf + `\Tentmaker`}
	}
	return []string{"/usr/local/bin", "/usr/local/libexec/tentmaker"}
}

// BinaryName is the binary's file name on goos.
func BinaryName(goos string) string {
	if goos == "windows" {
		return "tentmaker.exe"
	}
	return "tentmaker"
}
