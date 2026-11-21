package binsafe

import (
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

func fakeLookup(m map[string]Entry) Lookup {
	return func(p string) (Entry, error) {
		e, ok := m[filepath.ToSlash(p)]
		if !ok {
			return Entry{}, errors.New("no " + p)
		}
		return e, nil
	}
}

func rootDir() Entry  { return Entry{Mode: fs.ModeDir | 0o755} }
func rootFile() Entry { return Entry{Mode: 0o755} }

func TestUnix(t *testing.T) {
	base := map[string]Entry{
		"/": rootDir(), "/usr": rootDir(), "/usr/local": rootDir(), "/usr/local/bin": rootDir(),
		"/usr/local/bin/tentmaker": rootFile(),
		"/home":                    rootDir(), "/home/alice": {UID: 1000, GID: 1000, Mode: fs.ModeDir | 0o755},
		"/home/alice/.local":               {UID: 1000, GID: 1000, Mode: fs.ModeDir | 0o755},
		"/home/alice/.local/bin":           {UID: 1000, GID: 1000, Mode: fs.ModeDir | 0o755},
		"/home/alice/.local/bin/tentmaker": {UID: 1000, GID: 1000, Mode: 0o755},
		"/opt":                             rootDir(), "/opt/tm": {Mode: fs.ModeDir | 0o775, GID: 80}, "/opt/tm/tentmaker": rootFile(),
		"/srv": rootDir(), "/srv/tm": {Mode: fs.ModeDir | 0o775}, "/srv/tm/tentmaker": rootFile(),
		"/tmp": {Mode: fs.ModeDir | fs.ModeSticky | 0o777}, "/tmp/tentmaker": rootFile(),
		"/usr/local/bin/link": {Mode: fs.ModeSymlink | 0o777},
	}
	for path, want := range map[string]string{
		"/usr/local/bin/tentmaker":         "",
		"/home/alice/.local/bin/tentmaker": "owned by uid 1000",
		"/opt/tm/tentmaker":                "writable by group 80",
		"/srv/tm/tentmaker":                "", // group root may write
		"/tmp/tentmaker":                   "writable by every user",
		"/usr/local/bin/link":              "symbolic link",
	} {
		got, err := Walk(path, fakeLookup(base), Unix)
		if err != nil || (want == "") != (got == "") || !strings.Contains(got, want) {
			t.Errorf("%s: %q %v, want %q", path, got, err, want)
		}
	}
}

func TestWindows(t *testing.T) {
	admins := []ACE{{SID: SIDSystem, Mask: genericAll}, {SID: SIDAdministrators, Mask: genericAll}, {SID: "S-1-5-32-545", Mask: 0x1200a9}}
	ti := Entry{Owner: SIDTrustedInstaller, ACEs: admins}
	root := Entry{Owner: SIDSystem, ACEs: append([]ACE{
		{SID: "S-1-5-11", Mask: fileAppendData},               // Authenticated Users: create folders in C:\
		{SID: "S-1-5-11", Mask: 0x1301bf, InheritOnly: true},  // Modify for children only
		{SID: "S-1-3-0", Mask: genericAll, InheritOnly: true}, // CREATOR OWNER
	}, admins...)}
	user := "S-1-5-21-1-2-3-1001"
	m := map[string]Entry{
		"C:/": root, "C:/Program Files": ti, "C:/Program Files/Tentmaker": {Owner: SIDAdministrators, ACEs: admins},
		"C:/Program Files/Tentmaker/tentmaker.exe": {Owner: SIDAdministrators, ACEs: admins},
		"C:/Users": ti, "C:/Users/alice": {Owner: SIDSystem, ACEs: append([]ACE{{SID: user, Mask: genericAll}}, admins...)},
		"C:/Users/alice/tentmaker.exe": {Owner: user, ACEs: admins},
		"C:/Tools":                     {Owner: SIDAdministrators, ACEs: append([]ACE{{SID: user, Mask: fileWriteData}}, admins...)},
		"C:/Tools/tentmaker.exe":       {Owner: SIDAdministrators, ACEs: admins},
		"C:/Odd":                       {Owner: SIDAdministrators, ACEs: append([]ACE{{Unknown: true, SID: "an access entry of type 9"}}, admins...)},
		"C:/Odd/tentmaker.exe":         {Owner: SIDAdministrators, ACEs: append([]ACE{{SID: user, Mask: genericAll, Deny: true}}, admins...)},
		"C:/Open":                      {Owner: SIDAdministrators, ACEs: []ACE{{SID: SIDEveryone, Mask: 0xffffffff}}},
		"C:/Open/tentmaker.exe":        {Owner: SIDAdministrators, ACEs: admins},
	}
	for path, want := range map[string]string{
		`C:\Program Files\Tentmaker\tentmaker.exe`: "",
		`C:\Users\alice\tentmaker.exe`:             "is owned by " + user,
		`C:\Tools\tentmaker.exe`:                   "lets " + user, // may add a file next to the binary
		`C:\Odd\tentmaker.exe`:                     "type 9",
		`C:\Open\tentmaker.exe`:                    "lets " + SIDEveryone,
	} {
		look := func(p string) (Entry, error) {
			p = strings.ReplaceAll(p, `\`, "/")
			if p == "C:" || p == "C:." {
				p = "C:/"
			}
			e, ok := m[p]
			if !ok {
				return Entry{}, errors.New("no " + p)
			}
			return e, nil
		}
		got, err := walkWindows(path, look)
		if err != nil || (want == "") != (got == "") || !strings.Contains(got, want) {
			t.Errorf("%s: %q %v, want %q", path, got, err, want)
		}
	}
}

// walkWindows walks a Windows path on any OS (filepath on Linux doesn't
// split on backslashes).
func walkWindows(path string, look Lookup) (string, error) {
	parts := strings.Split(path, `\`)
	for i := len(parts); i >= 1; i-- {
		p := strings.Join(parts[:i], `\`)
		if i == 1 {
			p += `\`
		}
		e, err := look(p)
		if err != nil {
			return "", err
		}
		if why := Windows(p, len(parts)-i, e); why != "" {
			return why, nil
		}
	}
	return "", nil
}

func TestServiceDirs(t *testing.T) {
	if d := ServiceDirs("linux", nil); len(d) != 2 || d[0] != "/usr/local/bin" || d[1] != "/usr/local/libexec/tentmaker" {
		t.Errorf("linux %v", d)
	}
	env := func(k string) string { return map[string]string{"ProgramFiles": `D:\PF`}[k] }
	if d := ServiceDirs("windows", env); len(d) != 1 || d[0] != `D:\PF\Tentmaker` {
		t.Errorf("windows %v", d)
	}
}
