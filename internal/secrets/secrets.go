// Package secrets keeps forge tokens in the operating system's secret store
// on broker devices.
//
// On Linux and macOS a secret is one file under a root-owned 0700 directory
// (/etc/tentmaker/secrets, "/Library/Application Support/Tentmaker/secrets"),
// mode 0600, written atomically. A read refuses a file or directory that is
// group or world accessible, a symlink, or not owned by the current effective
// user, so a misconfigured store fails closed.
//
// On Windows a secret is a DPAPI (machine scope) encrypted file under
// %ProgramData%\Tentmaker\secrets, whose DACL is protected and grants only
// SYSTEM and Administrators.
//
// Secret values never appear in an error message.
package secrets

import (
	"fmt"
	"io/fs"
	"regexp"
)

// Store holds named secrets.
type Store interface {
	// Get returns the secret. A missing secret is an error that matches
	// ErrNotFound (and fs.ErrNotExist) with errors.Is.
	Get(name string) ([]byte, error)
	// Set writes the secret atomically, replacing any old value.
	Set(name string, v []byte) error
	// Delete removes the secret. Deleting a missing secret is not an error.
	Delete(name string) error
}

// ErrNotFound is returned by Get for a secret that does not exist.
var ErrNotFound = fmt.Errorf("secret not found: %w", fs.ErrNotExist)

// NamePattern is the only shape a secret name may take. Names become file
// names, so nothing else is allowed.
var NamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

func checkName(name string) error {
	if !NamePattern.MatchString(name) {
		return fmt.Errorf("secret name %q is not allowed: use 1 to 64 of a-z, 0-9, '.', '_' and '-', starting with a letter or digit", name)
	}
	return nil
}

// Default returns the store at this OS's standard location.
func Default() Store { return Dir(DefaultDir()) }
