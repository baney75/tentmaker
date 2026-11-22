package linux

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"

	"github.com/baney75/tentmaker/internal/core"
)

// The runner's worker sets ACTIONS_RESULTS_URL and ACTIONS_CACHE_URL in each
// step's environment from the job message, which would override the
// device's cache service that .env puts there. The patch renames the two
// names in Runner.Worker.dll's string heap (same length, so nothing else
// moves): the worker then sets ACTIONS_RESULTS_ORL and ACTIONS_CACHE_ORL,
// and a step sees .env's values under the usual names. The docker shim
// gives containers the real ones back.
var dllRenames = [][2]string{
	{"ACTIONS_RESULTS_URL", "ACTIONS_RESULTS_ORL"},
	{"ACTIONS_CACHE_URL", "ACTIONS_CACHE_ORL"},
}

func utf16le(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 2*len(u))
	for i, c := range u {
		b[2*i], b[2*i+1] = byte(c), byte(c>>8)
	}
	return b
}

// PatchWorkerDLL returns a copy of a Runner.Worker.dll with the two names
// renamed. Each name must appear exactly once, and its new name not at
// all: a runner that sets them anywhere else, or already patched, is
// refused rather than half patched.
func PatchWorkerDLL(dll []byte) ([]byte, error) {
	out := bytes.Clone(dll)
	for _, r := range dllRenames {
		from, to := utf16le(r[0]), utf16le(r[1])
		if n := bytes.Count(out, from); n != 1 {
			return nil, fmt.Errorf("Runner.Worker.dll names %s %d times, not once", r[0], n)
		}
		if n := bytes.Count(out, to); n != 0 {
			return nil, fmt.Errorf("Runner.Worker.dll already names %s", r[1])
		}
		i := bytes.Index(out, from)
		copy(out[i:], to)
	}
	return out, nil
}

// DLLPin is the Runner.Worker.dll of one pinned runner release, before and
// after the patch.
type DLLPin struct {
	Original, Patched string // sha256 hex
}

// Runner.Worker.dll of actions/runner 2.338.2, measured from the pinned
// tarballs: x64 and arm64 builds differ.
var workerDLLPins = map[string]DLLPin{
	"2.338.2-x64": {
		Original: "fdd27a1daadf9a4d1b8f92856583d50166c03e40c7afd670b8c2e32df5215a3f",
		Patched:  "5a4b8caa10821a58760388031e107f5da8d4c185713ce800c9e25e43182ae0d0",
	},
	"2.338.2-arm64": {
		Original: "98dfbf564515ab65263168bc98dc80d1c86f512beb6266cc8e749b7500f87c1e",
		Patched:  "af54159deb6d365a9a158f77f27b0df904ee11a9a339cd223527cc0e8dbac86c",
	},
}

func (e *Executor) dllPin(p Pin) (DLLPin, bool) {
	if e.o.WorkerDLL != nil {
		return *e.o.WorkerDLL, true
	}
	d, ok := workerDLLPins[p.Version+"-"+p.Arch]
	return d, ok
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// patchedDLL is where the verified patched copy for a runner release lives.
// The template stays as GitHub shipped it, so a job without the cache, or
// any job when this fails, runs the original.
func (e *Executor) patchedDLL(p Pin) string {
	return filepath.Join(e.p.Lib, "runner-cache", p.Version+"-"+p.Arch, "Runner.Worker.dll")
}

// ensureCachePatch makes the patched dll and the current docker shim, once
// per template, and reports whether jobs can use the device's Actions
// cache. Any mismatch turns the cache off and is logged once: jobs then
// use GitHub's cache as before.
func (e *Executor) ensureCachePatch() bool {
	tpl, err := filepath.EvalSymlinks(e.template())
	if err != nil {
		return false
	}
	e.patchMu.Lock()
	defer e.patchMu.Unlock()
	if e.patchFor == tpl {
		return e.patchOK
	}
	err = e.makeCachePatch(tpl)
	e.patchFor, e.patchOK = tpl, err == nil
	if err != nil {
		e.log.Warn("jobs use GitHub's Actions cache, not the device's", "executor", Name, "reason", err.Error())
	} else {
		e.log.Info("the device's Actions cache is ready for jobs", "executor", Name, "dll", e.patchedDLLPath)
	}
	return e.patchOK
}

func (e *Executor) makeCachePatch(tpl string) error {
	p, ok := e.pin()
	if !ok {
		return fmt.Errorf("no runner pinned for %s", e.arch)
	}
	pin, ok := e.dllPin(p)
	if !ok {
		return fmt.Errorf("no Runner.Worker.dll pin for runner %s %s", p.Version, p.Arch)
	}
	dst := e.patchedDLL(p)
	if cur, err := os.ReadFile(dst); err == nil && sha256Hex(cur) == pin.Patched {
		e.patchedDLLPath = dst
		return e.ensureShim()
	}
	orig, err := os.ReadFile(filepath.Join(tpl, "bin", "Runner.Worker.dll"))
	if err != nil {
		return err
	}
	if got := sha256Hex(orig); got != pin.Original {
		return fmt.Errorf("the template's Runner.Worker.dll is %s, not the pinned %s", got, pin.Original)
	}
	patched, err := PatchWorkerDLL(orig)
	if err != nil {
		return err
	}
	if got := sha256Hex(patched); got != pin.Patched {
		return fmt.Errorf("the patched Runner.Worker.dll is %s, not the pinned %s", got, pin.Patched)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := writeRoot(e.h, dst, patched, 0o644); err != nil {
		return err
	}
	e.patchedDLLPath = dst
	return e.ensureShim()
}

// ensureShim rewrites the docker shim when it predates the one that gives
// containers GitHub's cache variables back.
func (e *Executor) ensureShim() error {
	path := filepath.Join(e.shimDir(), "docker")
	want := []byte(dockerShim(e.p.DockerCLI))
	if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, want) {
		return nil
	}
	if err := writeRoot(e.h, path, want, 0o755); err != nil {
		return fmt.Errorf("docker shim: %w", err)
	}
	return nil
}

// ActionsCacheReady implements core.ActionsCacher.
func (e *Executor) ActionsCacheReady() bool {
	return e.cache.Enabled() && e.ensureCachePatch()
}

// ActionsRoot implements core.ActionsCacher.
func (e *Executor) ActionsRoot(c core.CacheRepo, create bool) (*os.Root, error) {
	return e.cache.ActionsRoot(c, create)
}

// cacheURL checks the service URL the worker passes: plain http to a
// loopback address, nothing that could break out of a .env line.
func cacheURL(s string) (string, error) {
	if strings.ContainsAny(s, "\r\n\x00 ") {
		return "", errors.New("the actions cache URL has a control character")
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("the actions cache URL is not plain http")
	}
	ap, err := netip.ParseAddrPort(u.Host)
	if err != nil || !ap.Addr().IsLoopback() {
		return "", errors.New("the actions cache URL is not a loopback address")
	}
	return "http://" + ap.String() + "/", nil
}

// actionsEnv is what .env adds for a job that uses the device's cache.
// It holds no secret: the service names the job by its connection.
func actionsEnv(base string) []string {
	return []string{
		"ACTIONS_RESULTS_URL=" + base,
		"ACTIONS_CACHE_URL=" + base,
		"ACTIONS_CACHE_SERVICE_V2=True",
	}
}

// usePatchedDLL replaces the job's Runner.Worker.dll with the patched one,
// as a hard link when it can (recorded in linked, which chownTree skips)
// and a copy otherwise.
func (e *Executor) usePatchedDLL(runnerDir string, linked map[string]bool) error {
	e.patchMu.Lock()
	src := e.patchedDLLPath
	e.patchMu.Unlock()
	if src == "" {
		return errors.New("no patched Runner.Worker.dll")
	}
	dst := filepath.Join(runnerDir, "bin", "Runner.Worker.dll")
	if err := os.Remove(dst); err != nil {
		return err
	}
	delete(linked, dst)
	if fi, err := os.Stat(src); err == nil && shareable(fi) {
		if err := os.Link(src, dst); err == nil {
			linked[dst] = true
			return nil
		}
	}
	return copyFile(src, dst, 0o644)
}
