//go:build windows

package windows

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	win "golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// Calls x/sys does not wrap.
var (
	advapi32 = win.NewLazySystemDLL("advapi32.dll")
	netapi32 = win.NewLazySystemDLL("netapi32.dll")
	user32   = win.NewLazySystemDLL("user32.dll")

	procLogonUserW                = advapi32.NewProc("LogonUserW")
	procLsaOpenPolicy             = advapi32.NewProc("LsaOpenPolicy")
	procLsaClose                  = advapi32.NewProc("LsaClose")
	procLsaAddAccountRights       = advapi32.NewProc("LsaAddAccountRights")
	procLsaRemoveAccountRights    = advapi32.NewProc("LsaRemoveAccountRights")
	procLsaEnumerateAccountRights = advapi32.NewProc("LsaEnumerateAccountRights")
	procLsaFreeMemory             = advapi32.NewProc("LsaFreeMemory")
	procLsaNtStatusToWinError     = advapi32.NewProc("LsaNtStatusToWinError")

	procNetUserAdd              = netapi32.NewProc("NetUserAdd")
	procNetUserSetInfo          = netapi32.NewProc("NetUserSetInfo")
	procNetUserDel              = netapi32.NewProc("NetUserDel")
	procNetUserGetLocalGroups   = netapi32.NewProc("NetUserGetLocalGroups")
	procNetLocalGroupDelMembers = netapi32.NewProc("NetLocalGroupDelMembers")

	procGetProcessWindowStation = user32.NewProc("GetProcessWindowStation")
	procSetProcessWindowStation = user32.NewProc("SetProcessWindowStation")
	procCreateWindowStationW    = user32.NewProc("CreateWindowStationW")
	procCreateDesktopW          = user32.NewProc("CreateDesktopW")
	procCloseWindowStation      = user32.NewProc("CloseWindowStation")
)

const (
	logon32LogonBatch      = 4
	logon32ProviderDefault = 0

	nerrUserNotFound    = 2221
	errMemberNotInAlias = 1377
	maxPreferredLength  = 0xFFFFFFFF

	userPrivUser       = 1
	ufScript           = 0x0001
	ufPasswdCantChange = 0x0040
	ufDontExpirePasswd = 0x10000
	ufNotDelegated     = 0x100000
	accountFlags       = ufScript | ufPasswdCantChange | ufDontExpirePasswd | ufNotDelegated

	policyCreateAccount      = 0x0010
	policyLookupNames        = 0x0800
	statusObjectNameNotFound = 0xC0000034

	cpuRateControlEnable  = 0x1
	cpuRateControlHardCap = 0x4

	winstaAllAccess = 0x37F

	// Jobs get their own window station and desktop. The service's own
	// (Service-0x0-3e7$) stays closed to the runner user.
	winstaName  = "Tentmaker"
	desktopName = "Default"

	userListKey = `SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon\SpecialAccounts\UserList`
)

// OSAPI is the real API.
type OSAPI struct {
	mu      sync.Mutex
	deskSID string // the SID the job desktop was made for
}

// NewOSAPI returns the real API.
func NewOSAPI() *OSAPI { return &OSAPI{} }

func u16(s string) *uint16 {
	p, err := win.UTF16PtrFromString(s)
	if err != nil {
		// Only a NUL can fail; every caller passes a validated name or path.
		p, _ = win.UTF16PtrFromString("")
	}
	return p
}

// IsAdmin implements API. LocalSystem's token has Administrators enabled.
func (*OSAPI) IsAdmin() (bool, error) {
	sid, err := win.CreateWellKnownSid(win.WinBuiltinAdministratorsSid)
	if err != nil {
		return false, err
	}
	return win.GetCurrentProcessToken().IsMember(sid)
}

// FreeDisk implements API.
func (*OSAPI) FreeDisk(path string) (uint64, error) {
	var free uint64
	if err := win.GetDiskFreeSpaceEx(u16(path), &free, nil, nil); err != nil {
		return 0, err
	}
	return free, nil
}

// UserSID implements API. It looks the name up on this computer only, so a
// domain account with the same name never matches.
func (*OSAPI) UserSID(name string) (string, error) {
	cn, err := win.ComputerName()
	if err != nil {
		return "", err
	}
	sid, _, typ, err := win.LookupSID("", cn+`\`+name)
	if errors.Is(err, win.ERROR_NONE_MAPPED) {
		return "", ErrNoUser
	}
	if err != nil {
		return "", fmt.Errorf("look up %s: %w", name, err)
	}
	if typ != win.SidTypeUser {
		return "", fmt.Errorf("%s is not a user account", name)
	}
	return sid.String(), nil
}

type userInfo1 struct {
	Name        *uint16
	Password    *uint16
	PasswordAge uint32
	Priv        uint32
	HomeDir     *uint16
	Comment     *uint16
	Flags       uint32
	ScriptPath  *uint16
}

type userInfo1003 struct{ Password *uint16 }

type userInfo1008 struct{ Flags uint32 }

type localGroupMembersInfo3 struct{ DomainAndName *uint16 }

func netErr(op string, r uintptr) error { return fmt.Errorf("%s: %w", op, syscall.Errno(r)) }

// CreateUser implements API: a standard user whose password never expires
// and that it can't change.
func (*OSAPI) CreateUser(name string, password []byte) error {
	pw, err := win.UTF16FromString(string(password))
	if err != nil {
		return errors.New("the password has a NUL")
	}
	defer clear(pw)
	ui := userInfo1{
		Name:     u16(name),
		Password: &pw[0],
		Priv:     userPrivUser,
		Comment:  u16("Runs Tentmaker CI jobs"),
		Flags:    accountFlags,
	}
	var parm uint32
	r, _, _ := procNetUserAdd.Call(0, 1, uintptr(unsafe.Pointer(&ui)), uintptr(unsafe.Pointer(&parm)))
	runtime.KeepAlive(pw)
	if r != 0 {
		return netErr("NetUserAdd", r)
	}
	return nil
}

// SetPassword implements API. It also resets the account flags, which
// re-enables a disabled account.
func (*OSAPI) SetPassword(name string, password []byte) error {
	pw, err := win.UTF16FromString(string(password))
	if err != nil {
		return errors.New("the password has a NUL")
	}
	defer clear(pw)
	var parm uint32
	ui := userInfo1003{Password: &pw[0]}
	r, _, _ := procNetUserSetInfo.Call(0, uintptr(unsafe.Pointer(u16(name))), 1003, uintptr(unsafe.Pointer(&ui)), uintptr(unsafe.Pointer(&parm)))
	runtime.KeepAlive(pw)
	if r != 0 {
		return netErr("NetUserSetInfo", r)
	}
	fl := userInfo1008{Flags: accountFlags}
	r, _, _ = procNetUserSetInfo.Call(0, uintptr(unsafe.Pointer(u16(name))), 1008, uintptr(unsafe.Pointer(&fl)), uintptr(unsafe.Pointer(&parm)))
	if r != 0 {
		return netErr("NetUserSetInfo", r)
	}
	return nil
}

// DeleteUser implements API.
func (*OSAPI) DeleteUser(name string) error {
	r, _, _ := procNetUserDel.Call(0, uintptr(unsafe.Pointer(u16(name))))
	if r != 0 && r != nerrUserNotFound {
		return netErr("NetUserDel", r)
	}
	return nil
}

// LocalGroups implements API: direct memberships only.
func (*OSAPI) LocalGroups(name string) ([]Group, error) {
	var buf *byte
	var read, total uint32
	r, _, _ := procNetUserGetLocalGroups.Call(0, uintptr(unsafe.Pointer(u16(name))), 0, 0,
		uintptr(unsafe.Pointer(&buf)), maxPreferredLength, uintptr(unsafe.Pointer(&read)), uintptr(unsafe.Pointer(&total)))
	if r == nerrUserNotFound {
		return nil, ErrNoUser
	}
	if r != 0 {
		return nil, netErr("NetUserGetLocalGroups", r)
	}
	if buf == nil {
		return nil, nil
	}
	defer win.NetApiBufferFree(buf)
	var out []Group
	for _, p := range unsafe.Slice((**uint16)(unsafe.Pointer(buf)), read) {
		g := Group{Name: win.UTF16PtrToString(p)}
		if sid, _, _, err := win.LookupSID("", g.Name); err == nil {
			g.SID = sid.String()
		}
		out = append(out, g)
	}
	return out, nil
}

// RemoveFromGroup implements API.
func (*OSAPI) RemoveFromGroup(group, name string) error {
	cn, err := win.ComputerName()
	if err != nil {
		return err
	}
	m := localGroupMembersInfo3{DomainAndName: u16(cn + `\` + name)}
	r, _, _ := procNetLocalGroupDelMembers.Call(0, uintptr(unsafe.Pointer(u16(group))), 3, uintptr(unsafe.Pointer(&m)), 1)
	if r != 0 && r != errMemberNotInAlias {
		return netErr("NetLocalGroupDelMembers", r)
	}
	return nil
}

type lsaUnicodeString struct {
	Length        uint16
	MaximumLength uint16
	Buffer        *uint16
}

type lsaObjectAttributes struct {
	Length                   uint32
	RootDirectory            uintptr
	ObjectName               uintptr
	Attributes               uint32
	SecurityDescriptor       uintptr
	SecurityQualityOfService uintptr
}

func ntErr(op string, status uintptr) error {
	r, _, _ := procLsaNtStatusToWinError.Call(status)
	return fmt.Errorf("%s: %w", op, syscall.Errno(r))
}

func lsaOpen() (uintptr, error) {
	oa := lsaObjectAttributes{}
	oa.Length = uint32(unsafe.Sizeof(oa))
	var h uintptr
	st, _, _ := procLsaOpenPolicy.Call(0, uintptr(unsafe.Pointer(&oa)), policyLookupNames|policyCreateAccount, uintptr(unsafe.Pointer(&h)))
	if st != 0 {
		return 0, ntErr("LsaOpenPolicy", st)
	}
	return h, nil
}

func lsaClose(h uintptr) { procLsaClose.Call(h) }

// AccountRights implements API.
func (*OSAPI) AccountRights(sidStr string) ([]string, error) {
	sid, err := win.StringToSid(sidStr)
	if err != nil {
		return nil, err
	}
	h, err := lsaOpen()
	if err != nil {
		return nil, err
	}
	defer lsaClose(h)
	var p *lsaUnicodeString
	var n uint32
	st, _, _ := procLsaEnumerateAccountRights.Call(h, uintptr(unsafe.Pointer(sid)), uintptr(unsafe.Pointer(&p)), uintptr(unsafe.Pointer(&n)))
	if st == statusObjectNameNotFound {
		return nil, nil
	}
	if st != 0 {
		return nil, ntErr("LsaEnumerateAccountRights", st)
	}
	defer procLsaFreeMemory.Call(uintptr(unsafe.Pointer(p)))
	var out []string
	for _, s := range unsafe.Slice(p, n) {
		out = append(out, string(utf16.Decode(unsafe.Slice(s.Buffer, s.Length/2))))
	}
	return out, nil
}

// AddRights implements API.
func (*OSAPI) AddRights(sidStr string, rights []string) error {
	if len(rights) == 0 {
		return nil
	}
	sid, err := win.StringToSid(sidStr)
	if err != nil {
		return err
	}
	h, err := lsaOpen()
	if err != nil {
		return err
	}
	defer lsaClose(h)
	keep := make([][]uint16, len(rights))
	us := make([]lsaUnicodeString, len(rights))
	for i, r := range rights {
		u := utf16.Encode([]rune(r))
		keep[i] = append(u, 0)
		us[i] = lsaUnicodeString{Length: uint16(2 * len(u)), MaximumLength: uint16(2*len(u) + 2), Buffer: &keep[i][0]}
	}
	st, _, _ := procLsaAddAccountRights.Call(h, uintptr(unsafe.Pointer(sid)), uintptr(unsafe.Pointer(&us[0])), uintptr(len(us)))
	runtime.KeepAlive(keep)
	if st != 0 {
		return ntErr("LsaAddAccountRights", st)
	}
	return nil
}

// RemoveAllRights implements API.
func (*OSAPI) RemoveAllRights(sidStr string) error {
	sid, err := win.StringToSid(sidStr)
	if err != nil {
		return err
	}
	h, err := lsaOpen()
	if err != nil {
		return err
	}
	defer lsaClose(h)
	st, _, _ := procLsaRemoveAccountRights.Call(h, uintptr(unsafe.Pointer(sid)), 1, 0, 0)
	if st != 0 && st != statusObjectNameNotFound {
		return ntErr("LsaRemoveAccountRights", st)
	}
	return nil
}

// HideUser implements API.
func (*OSAPI) HideUser(name string, hide bool) error {
	if hide {
		k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, userListKey, registry.SET_VALUE)
		if err != nil {
			return err
		}
		defer k.Close()
		return k.SetDWordValue(name, 0)
	}
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, userListKey, registry.SET_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer k.Close()
	if err := k.DeleteValue(name); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}

func dpapi(b []byte, protect bool) ([]byte, error) {
	if len(b) == 0 {
		return nil, errors.New("nothing to protect")
	}
	in := win.DataBlob{Size: uint32(len(b)), Data: &b[0]}
	var out win.DataBlob
	var err error
	if protect {
		err = win.CryptProtectData(&in, nil, nil, 0, nil, win.CRYPTPROTECT_LOCAL_MACHINE|win.CRYPTPROTECT_UI_FORBIDDEN, &out)
	} else {
		err = win.CryptUnprotectData(&in, nil, nil, 0, nil, win.CRYPTPROTECT_UI_FORBIDDEN, &out)
	}
	if err != nil {
		return nil, err
	}
	defer win.LocalFree(win.Handle(unsafe.Pointer(out.Data)))
	raw := unsafe.Slice(out.Data, out.Size)
	res := bytes.Clone(raw)
	if !protect {
		clear(raw)
	}
	return res, nil
}

// Protect implements API with DPAPI in machine scope: any process on this
// machine can unprotect it, so the file's ACL is what keeps it private.
func (*OSAPI) Protect(b []byte) ([]byte, error) { return dpapi(b, true) }

// Unprotect implements API.
func (*OSAPI) Unprotect(b []byte) ([]byte, error) { return dpapi(b, false) }

func aclFrom(aces []ACE) (*win.ACL, error) {
	ea := make([]win.EXPLICIT_ACCESS, 0, len(aces))
	for _, a := range aces {
		sid, err := win.StringToSid(a.SID)
		if err != nil {
			return nil, err
		}
		inh := uint32(win.NO_INHERITANCE)
		if a.Inherit {
			inh = win.SUB_CONTAINERS_AND_OBJECTS_INHERIT
		}
		ea = append(ea, win.EXPLICIT_ACCESS{
			AccessPermissions: win.ACCESS_MASK(a.Mask),
			AccessMode:        win.GRANT_ACCESS,
			Inheritance:       inh,
			Trustee: win.TRUSTEE{
				TrusteeForm:  win.TRUSTEE_IS_SID,
				TrusteeType:  win.TRUSTEE_IS_UNKNOWN,
				TrusteeValue: win.TrusteeValueFromSID(sid),
			},
		})
	}
	return win.ACLFromEntries(ea, nil)
}

// SetDACL implements API. The DACL is protected: nothing is inherited from
// the parent (ProgramData lets Users create folders).
func (*OSAPI) SetDACL(path string, aces []ACE) error {
	acl, err := aclFrom(aces)
	if err != nil {
		return err
	}
	return win.SetNamedSecurityInfo(path, win.SE_FILE_OBJECT,
		win.DACL_SECURITY_INFORMATION|win.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil)
}

func securityAttrs(aces []ACE) (*win.SecurityAttributes, error) {
	acl, err := aclFrom(aces)
	if err != nil {
		return nil, err
	}
	sd, err := win.NewSecurityDescriptor()
	if err != nil {
		return nil, err
	}
	if err := sd.SetDACL(acl, true, false); err != nil {
		return nil, err
	}
	sa := &win.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	return sa, nil
}

// desktopFor makes the job window station and desktop once per daemon.
// Console programs need one they may open; the runner user gets this one
// and nothing of the service's.
func (a *OSAPI) desktopFor(sid string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	name := winstaName + `\` + desktopName
	if a.deskSID != "" {
		if a.deskSID != sid {
			return "", errors.New("the job desktop was made for another account; restart the daemon")
		}
		return name, nil
	}
	sa, err := securityAttrs([]ACE{
		{SIDSystem, win.GENERIC_ALL, false},
		{SIDAdmins, win.GENERIC_ALL, false},
		{sid, win.GENERIC_ALL, false},
	})
	if err != nil {
		return "", err
	}
	ws, _, e1 := procCreateWindowStationW.Call(uintptr(unsafe.Pointer(u16(winstaName))), 0, winstaAllAccess, uintptr(unsafe.Pointer(sa)))
	if ws == 0 {
		return "", fmt.Errorf("CreateWindowStation: %w", e1)
	}
	old, _, e2 := procGetProcessWindowStation.Call()
	if old == 0 {
		procCloseWindowStation.Call(ws)
		return "", fmt.Errorf("GetProcessWindowStation: %w", e2)
	}
	// CreateDesktop makes the desktop in the caller's window station.
	if r, _, e := procSetProcessWindowStation.Call(ws); r == 0 {
		procCloseWindowStation.Call(ws)
		return "", fmt.Errorf("SetProcessWindowStation: %w", e)
	}
	dk, _, e3 := procCreateDesktopW.Call(uintptr(unsafe.Pointer(u16(desktopName))), 0, 0, 0, win.GENERIC_ALL, uintptr(unsafe.Pointer(sa)))
	procSetProcessWindowStation.Call(old)
	runtime.KeepAlive(sa)
	if dk == 0 {
		procCloseWindowStation.Call(ws)
		return "", fmt.Errorf("CreateDesktop: %w", e3)
	}
	// Both handles stay open for the daemon's life so the objects persist.
	a.deskSID = sid
	return name, nil
}

func logonUser(user string, password []byte) (win.Token, error) {
	pw, err := win.UTF16FromString(string(password))
	if err != nil {
		return 0, errors.New("the password has a NUL")
	}
	defer clear(pw)
	var tok win.Token
	r, _, e := procLogonUserW.Call(uintptr(unsafe.Pointer(u16(user))), uintptr(unsafe.Pointer(u16("."))),
		uintptr(unsafe.Pointer(&pw[0])), logon32LogonBatch, logon32ProviderDefault, uintptr(unsafe.Pointer(&tok)))
	runtime.KeepAlive(pw)
	if r == 0 {
		return 0, fmt.Errorf("LogonUser %s: %w", user, e)
	}
	return tok, nil
}

type cpuRateInfo struct {
	ControlFlags uint32
	CpuRate      uint32
}

func setJobLimits(job win.Handle, p ProcSpec) error {
	var ext win.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	ext.BasicLimitInformation.LimitFlags = win.JOB_OBJECT_LIMIT_JOB_MEMORY |
		win.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE |
		win.JOB_OBJECT_LIMIT_DIE_ON_UNHANDLED_EXCEPTION |
		win.JOB_OBJECT_LIMIT_PRIORITY_CLASS
	ext.BasicLimitInformation.PriorityClass = win.BELOW_NORMAL_PRIORITY_CLASS
	ext.JobMemoryLimit = uintptr(p.MemoryBytes)
	if _, err := win.SetInformationJobObject(job, win.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&ext)), uint32(unsafe.Sizeof(ext))); err != nil {
		return fmt.Errorf("job memory limit: %w", err)
	}
	cpu := cpuRateInfo{ControlFlags: cpuRateControlEnable | cpuRateControlHardCap, CpuRate: p.CPURate}
	if _, err := win.SetInformationJobObject(job, win.JobObjectCpuRateControlInformation,
		uintptr(unsafe.Pointer(&cpu)), uint32(unsafe.Sizeof(cpu))); err != nil {
		return fmt.Errorf("job CPU rate: %w", err)
	}
	ui := win.JOBOBJECT_BASIC_UI_RESTRICTIONS{UIRestrictionsClass: win.JOB_OBJECT_UILIMIT_DESKTOP |
		win.JOB_OBJECT_UILIMIT_DISPLAYSETTINGS | win.JOB_OBJECT_UILIMIT_EXITWINDOWS |
		win.JOB_OBJECT_UILIMIT_GLOBALATOMS | win.JOB_OBJECT_UILIMIT_HANDLES |
		win.JOB_OBJECT_UILIMIT_READCLIPBOARD | win.JOB_OBJECT_UILIMIT_SYSTEMPARAMETERS |
		win.JOB_OBJECT_UILIMIT_WRITECLIPBOARD}
	if _, err := win.SetInformationJobObject(job, win.JobObjectBasicUIRestrictions,
		uintptr(unsafe.Pointer(&ui)), uint32(unsafe.Sizeof(ui))); err != nil {
		return fmt.Errorf("job UI limits: %w", err)
	}
	return nil
}

type osProcess struct {
	mu   sync.Mutex
	job  win.Handle // 0 once closed
	done chan struct{}
	code int
}

func (p *osProcess) Done() <-chan struct{} { return p.done }

func (p *osProcess) ExitCode() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.code
}

func (p *osProcess) Terminate() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.job == 0 {
		return nil
	}
	return win.TerminateJobObject(p.job, 1)
}

// wait ends the whole job object once the main process exits, as systemd
// does with a unit's cgroup.
func (p *osProcess) wait(proc win.Handle) {
	win.WaitForSingleObject(proc, win.INFINITE)
	var code uint32
	if err := win.GetExitCodeProcess(proc, &code); err != nil {
		code = 1
	}
	win.CloseHandle(proc)
	p.mu.Lock()
	win.TerminateJobObject(p.job, 1)
	win.CloseHandle(p.job)
	p.job = 0
	p.code = int(code)
	p.mu.Unlock()
	close(p.done)
}

// StartProcess implements API: LogonUser (batch), then CreateProcessAsUser
// suspended, into a fresh Job Object, then resume. Only the stdin pipe and
// the log file are inherited.
func (a *OSAPI) StartProcess(p ProcSpec) (Process, error) {
	desk, err := a.desktopFor(p.UserSID)
	if err != nil {
		return nil, err
	}
	env, err := envBlock(p.Env)
	if err != nil {
		return nil, err
	}
	tok, err := logonUser(p.User, p.Password)
	if err != nil {
		return nil, err
	}
	defer tok.Close()
	job, err := win.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			win.CloseHandle(job)
		}
	}()
	if err := setJobLimits(job, p); err != nil {
		return nil, err
	}
	inherit := &win.SecurityAttributes{InheritHandle: 1}
	inherit.Length = uint32(unsafe.Sizeof(*inherit))
	var inR, inW win.Handle
	if err := win.CreatePipe(&inR, &inW, inherit, 0); err != nil {
		return nil, fmt.Errorf("CreatePipe: %w", err)
	}
	defer win.CloseHandle(inR)
	stdin := os.NewFile(uintptr(inW), "job-stdin")
	defer func() {
		if !ok {
			stdin.Close()
		}
	}()
	if err := win.SetHandleInformation(inW, win.HANDLE_FLAG_INHERIT, 0); err != nil {
		return nil, err
	}
	logH, err := win.CreateFile(u16(p.LogPath), win.GENERIC_WRITE, win.FILE_SHARE_READ, inherit,
		win.CREATE_ALWAYS, win.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, fmt.Errorf("create the job log: %w", err)
	}
	defer win.CloseHandle(logH)
	al, err := win.NewProcThreadAttributeList(1)
	if err != nil {
		return nil, err
	}
	defer al.Delete()
	handles := []win.Handle{inR, logH}
	if err := al.Update(win.PROC_THREAD_ATTRIBUTE_HANDLE_LIST, unsafe.Pointer(&handles[0]),
		uintptr(len(handles))*unsafe.Sizeof(handles[0])); err != nil {
		return nil, err
	}
	var si win.StartupInfoEx
	si.Cb = uint32(unsafe.Sizeof(si))
	si.Flags = win.STARTF_USESTDHANDLES
	si.StdInput, si.StdOutput, si.StdErr = inR, logH, logH
	si.Desktop = u16(desk)
	si.ProcThreadAttributeList = al.List()
	cmdline := u16(win.ComposeCommandLine(append([]string{p.App}, p.Args...)))
	var pi win.ProcessInformation
	flags := uint32(win.CREATE_SUSPENDED | win.CREATE_UNICODE_ENVIRONMENT | win.CREATE_NO_WINDOW | win.EXTENDED_STARTUPINFO_PRESENT)
	if err := win.CreateProcessAsUser(tok, u16(p.App), cmdline, nil, nil, true, flags, &env[0], u16(p.Dir), &si.StartupInfo, &pi); err != nil {
		return nil, fmt.Errorf("CreateProcessAsUser: %w", err)
	}
	runtime.KeepAlive(handles)
	if err := win.AssignProcessToJobObject(job, pi.Process); err != nil {
		win.TerminateProcess(pi.Process, 1)
		win.CloseHandle(pi.Thread)
		win.CloseHandle(pi.Process)
		return nil, fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	if _, err := win.ResumeThread(pi.Thread); err != nil {
		win.TerminateJobObject(job, 1)
		win.CloseHandle(pi.Thread)
		win.CloseHandle(pi.Process)
		return nil, fmt.Errorf("ResumeThread: %w", err)
	}
	win.CloseHandle(pi.Thread)
	ok = true
	secret := bytes.Clone(p.Stdin)
	go func() {
		stdin.Write(secret)
		clear(secret)
		stdin.Close()
	}()
	proc := &osProcess{job: job, done: make(chan struct{}), code: -1}
	go proc.wait(pi.Process)
	return proc, nil
}

func processOwner(pid uint32) (string, bool) {
	h, err := win.OpenProcess(win.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return "", false
	}
	defer win.CloseHandle(h)
	var tok win.Token
	if err := win.OpenProcessToken(h, win.TOKEN_QUERY, &tok); err != nil {
		return "", false
	}
	defer tok.Close()
	tu, err := tok.GetTokenUser()
	if err != nil {
		return "", false
	}
	return tu.User.Sid.String(), true
}

// KillUserProcesses implements API. It sweeps a few times so a process
// that starts another while dying is caught too.
func (*OSAPI) KillUserProcesses(sid string) (int, error) {
	total := 0
	self := uint32(os.Getpid())
	for pass := 0; pass < 5; pass++ {
		snap, err := win.CreateToolhelp32Snapshot(win.TH32CS_SNAPPROCESS, 0)
		if err != nil {
			return total, err
		}
		var pe win.ProcessEntry32
		pe.Size = uint32(unsafe.Sizeof(pe))
		n := 0
		var errs []error
		for err = win.Process32First(snap, &pe); err == nil; err = win.Process32Next(snap, &pe) {
			if pe.ProcessID == 0 || pe.ProcessID == self {
				continue
			}
			if owner, ok := processOwner(pe.ProcessID); !ok || owner != sid {
				continue
			}
			h, err := win.OpenProcess(win.PROCESS_TERMINATE, false, pe.ProcessID)
			if err != nil {
				errs = append(errs, fmt.Errorf("open pid %d: %w", pe.ProcessID, err))
				continue
			}
			if err := win.TerminateProcess(h, 1); err != nil {
				errs = append(errs, fmt.Errorf("end pid %d: %w", pe.ProcessID, err))
			} else {
				n++
			}
			win.CloseHandle(h)
		}
		win.CloseHandle(snap)
		total += n
		if len(errs) > 0 {
			return total, errors.Join(errs...)
		}
		if n == 0 {
			return total, nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return total, errors.New("the runner user keeps starting processes")
}

// DeleteUserTasks implements API: every scheduled task whose principal is
// the runner user, found in %SystemRoot%\System32\Tasks.
func (*OSAPI) DeleteUserTasks(name, sid string) (int, error) {
	sys, err := win.GetSystemDirectory()
	if err != nil {
		return 0, err
	}
	root := filepath.Join(sys, "Tasks")
	var victims []string
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		if info, err := d.Info(); err != nil || info.Size() > 1<<20 {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		if taskRunsAs(decodeTaskXML(data), name, sid) {
			if rel, err := filepath.Rel(root, p); err == nil {
				victims = append(victims, `\`+rel)
			}
		}
		return nil
	})
	n := 0
	var errs []error
	for _, tn := range victims {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		out, err := exec.CommandContext(ctx, filepath.Join(sys, "schtasks.exe"), "/Delete", "/TN", tn, "/F").CombinedOutput()
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("delete task %s: %v: %s", tn, err, trunc(strings.TrimSpace(string(out)), 200)))
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}

// Fetch implements API.
func (*OSAPI) Fetch(ctx context.Context, url, dst string) error { return httpFetch(ctx, url, dst) }
