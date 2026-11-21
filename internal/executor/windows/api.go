package windows

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf16"
)

// ErrNoUser means the account does not exist.
var ErrNoUser = errors.New("no such user")

// Well-known SIDs and file access masks the executor uses.
const (
	SIDSystem      = "S-1-5-18"
	SIDAdmins      = "S-1-5-32-544"
	SIDUsers       = "S-1-5-32-545"
	SIDOwnerRights = "S-1-3-4" // caps what a file's owner may do (no WRITE_DAC)

	AccessFull           = 0x1F01FF // FILE_ALL_ACCESS
	AccessModify         = 0x1301BF // read, write, execute, delete; no WRITE_DAC or WRITE_OWNER
	AccessReadAndExecute = 0x1200A9
)

// Account rights Install sets on the runner user. Jobs log on as a batch
// job; every other logon type is denied.
var (
	GrantRights = []string{"SeBatchLogonRight"}
	DenyRights  = []string{
		"SeDenyInteractiveLogonRight",
		"SeDenyRemoteInteractiveLogonRight",
		"SeDenyNetworkLogonRight",
		"SeDenyServiceLogonRight",
	}
)

// ACE is one allow entry in a protected DACL.
type ACE struct {
	SID     string
	Mask    uint32
	Inherit bool // applies to subfolders and files
}

// Group is a local group a user belongs to directly.
type Group struct {
	Name string
	SID  string
}

// ProcSpec is one job process. Password and Stdin are secrets: the real API
// passes them only to LogonUser and the stdin pipe.
type ProcSpec struct {
	User        string
	UserSID     string
	Password    []byte
	App         string
	Args        []string
	Dir         string
	Env         []string
	Stdin       []byte
	LogPath     string // stdout and stderr, created fresh
	MemoryBytes int64  // JOB_OBJECT_LIMIT_JOB_MEMORY
	CPURate     uint32 // hard cap in 1/100 of a percent of the whole machine, 1..10000
}

// Process is a started job process inside its own Job Object.
type Process interface {
	Done() <-chan struct{} // closed once the main process exited and the job object was emptied
	ExitCode() int         // valid once Done is closed
	// MemoryLimitHit says the job object refused memory past its limit.
	// Valid once Done is closed.
	MemoryLimitHit() bool
	Terminate() error // kills every process in the job object
}

// API is everything the executor does to Windows. Tests swap in a fake.
type API interface {
	IsAdmin() (bool, error)
	FreeDisk(path string) (uint64, error)

	UserSID(name string) (string, error) // ErrNoUser when missing
	CreateUser(name string, password []byte) error
	SetPassword(name string, password []byte) error
	DeleteUser(name string) error // a missing user is fine
	LocalGroups(name string) ([]Group, error)
	RemoveFromGroup(group, name string) error
	AccountRights(sid string) ([]string, error)
	AddRights(sid string, rights []string) error
	RemoveAllRights(sid string) error
	HideUser(name string, hide bool) error

	Protect(b []byte) ([]byte, error) // DPAPI, machine scope
	Unprotect(b []byte) ([]byte, error)
	SetDACL(path string, aces []ACE) error // replaces the DACL and blocks inheritance

	StartProcess(p ProcSpec) (Process, error)
	KillUserProcesses(sid string) (int, error)
	DeleteUserTasks(name, sid string) (int, error)
	Fetch(ctx context.Context, url, dst string) error
	// PowerShell runs a script in Windows PowerShell and returns its
	// output. Scripts hold no secrets.
	PowerShell(ctx context.Context, script string) (string, error)

	// SetWFP makes fs Tentmaker's only WFP filters (wfp.go), with its
	// provider and sublayer, in one transaction. WFPInstalled reads back
	// the ones in place; RemoveWFP removes them all.
	SetWFP(fs []WFPFilter) error
	WFPInstalled() ([]WFPState, error)
	RemoveWFP() error
}

func httpFetch(ctx context.Context, url, dst string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", url, resp.Status)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	part := dst + ".part"
	f, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(part)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(part)
		return err
	}
	return os.Rename(part, dst)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func fetchVerified(ctx context.Context, a API, url, dst, want string) error {
	if _, err := os.Stat(dst); errors.Is(err, os.ErrNotExist) {
		if err := a.Fetch(ctx, url, dst); err != nil {
			return err
		}
	}
	got, err := fileSHA256(dst)
	if err != nil {
		return err
	}
	if got != want {
		os.Remove(dst)
		return fmt.Errorf("%s: sha256 %s, want %s; deleted it", filepath.Base(dst), got, want)
	}
	return nil
}

var userIDPattern = regexp.MustCompile(`(?is)<UserId>\s*([^<]*?)\s*</UserId>`)

// decodeTaskXML turns a Task Scheduler file (UTF-16 LE with a BOM, or
// UTF-8) into a string.
func decodeTaskXML(b []byte) string {
	if len(b) >= 2 && b[0] == 0xFF && b[1] == 0xFE {
		u := make([]uint16, 0, len(b)/2)
		for i := 2; i+1 < len(b); i += 2 {
			u = append(u, uint16(b[i])|uint16(b[i+1])<<8)
		}
		return string(utf16.Decode(u))
	}
	return strings.TrimPrefix(string(b), "\uFEFF")
}

// taskRunsAs reports whether a task's principal is the user, by SID, bare
// name or COMPUTER\name.
func taskRunsAs(xml, name, sid string) bool {
	for _, m := range userIDPattern.FindAllStringSubmatch(xml, -1) {
		v := m[1]
		if strings.EqualFold(v, sid) || strings.EqualFold(v, name) ||
			(len(v) > len(name) && strings.EqualFold(v[len(v)-len(name)-1:], `\`+name)) {
			return true
		}
	}
	return false
}

// envBlock builds a CreateProcess environment block, sorted by name
// without regard to case as Windows expects.
func envBlock(env []string) ([]uint16, error) {
	env = slices.Clone(env)
	sort.SliceStable(env, func(i, j int) bool {
		return strings.ToUpper(envName(env[i])) < strings.ToUpper(envName(env[j]))
	})
	var b []uint16
	for _, kv := range env {
		if strings.IndexByte(kv, 0) >= 0 || !strings.Contains(kv, "=") {
			return nil, fmt.Errorf("bad environment entry %q", trunc(envName(kv), 40))
		}
		b = append(b, utf16.Encode([]rune(kv))...)
		b = append(b, 0)
	}
	if len(b) == 0 {
		b = append(b, 0)
	}
	return append(b, 0), nil
}

func envName(kv string) string {
	if i := strings.IndexByte(kv[min(1, len(kv)):], '='); i >= 0 {
		return kv[:i+min(1, len(kv))]
	}
	return kv
}
