package update

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// PrevSuffix and OldSuffix name the files Apply leaves next to the binary:
// <bin>.prev is the previous version kept for Rollback; <bin>.old is the
// running exe Windows renamed aside (deleted on the next update).
const (
	PrevSuffix = ".prev"
	OldSuffix  = ".old"
)

// Apply copies newBin next to curBin, runs `<staged> version --json` and
// requires wantVersion within the health timeout, keeps the current binary
// as <curBin>.prev, then swaps the new one in atomically. On Windows the
// running exe is renamed to <curBin>.old first, since it can't be replaced
// while it runs. If the health check fails nothing is swapped.
func (u *Updater) Apply(ctx context.Context, newBin, curBin, wantVersion string) error {
	staged := stagedPath(curBin)
	if err := copyFile(newBin, staged, 0o755); err != nil {
		os.Remove(staged)
		return fmt.Errorf("staging the new binary: %w", err)
	}
	if err := u.HealthCheck(ctx, staged, wantVersion); err != nil {
		os.Remove(staged)
		return err
	}
	if _, err := os.Stat(curBin); err == nil {
		prev := curBin + PrevSuffix
		if err := copyFile(curBin, prev+".tmp", 0o755); err != nil {
			os.Remove(prev + ".tmp")
			os.Remove(staged)
			return fmt.Errorf("saving the current binary for rollback: %w", err)
		}
		if err := os.Rename(prev+".tmp", prev); err != nil {
			os.Remove(prev + ".tmp")
			os.Remove(staged)
			return fmt.Errorf("saving the current binary for rollback: %w", err)
		}
	}
	if err := u.swap(staged, curBin); err != nil {
		os.Remove(staged)
		return err
	}
	return nil
}

// Rollback puts <curBin>.prev back in place of curBin. The .prev file stays,
// so a second Rollback is harmless.
func (u *Updater) Rollback(curBin string) error {
	prev := curBin + PrevSuffix
	st, err := os.Stat(prev)
	if err != nil || !st.Mode().IsRegular() || st.Size() == 0 {
		return fmt.Errorf("no previous binary at %s to roll back to", prev)
	}
	staged := stagedPath(curBin)
	if err := copyFile(prev, staged, 0o755); err != nil {
		os.Remove(staged)
		return fmt.Errorf("staging the previous binary: %w", err)
	}
	if err := u.swap(staged, curBin); err != nil {
		os.Remove(staged)
		return err
	}
	return nil
}

// HealthCheck runs `<bin> version --json` and requires it to report want.
// It accepts {"version":"X"} and, for older builds, a "tentmaker X" line.
func (u *Updater) HealthCheck(ctx context.Context, bin, want string) error {
	ctx, cancel := context.WithTimeout(ctx, u.o.HealthTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "version", "--json")
	cmd.Stdin = nil
	cmd.WaitDelay = 2 * time.Second
	var out bytes.Buffer
	cmd.Stdout = &limitedBuffer{b: &out, n: 64 << 10}
	err := cmd.Run()
	if ctx.Err() != nil {
		return fmt.Errorf("health check: %s version --json did not finish within %s", filepath.Base(bin), u.o.HealthTimeout)
	}
	if err != nil {
		return fmt.Errorf("health check: %s version --json failed: %v", filepath.Base(bin), err)
	}
	got, err := parseVersionOutput(out.Bytes())
	if err != nil {
		return fmt.Errorf("health check: %v", err)
	}
	if strings.TrimPrefix(got, "v") != strings.TrimPrefix(want, "v") {
		return fmt.Errorf("health check: new binary reports version %q, want %q", got, want)
	}
	return nil
}

func parseVersionOutput(b []byte) (string, error) {
	b = bytes.TrimSpace(b)
	var v struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(b, &v) == nil && v.Version != "" {
		return v.Version, nil
	}
	if f := strings.Fields(string(b)); len(f) == 2 && f[0] == "tentmaker" {
		return f[1], nil
	}
	return "", fmt.Errorf("version output %q has no version", truncate(string(b), 120))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// stagedPath is where the new binary waits next to curBin. On Windows it
// keeps the .exe extension so it can run for the health check.
func stagedPath(curBin string) string {
	if strings.EqualFold(filepath.Ext(curBin), ".exe") {
		return curBin[:len(curBin)-4] + ".new.exe"
	}
	return curBin + ".new"
}

func (u *Updater) swap(staged, curBin string) error {
	if u.o.GOOS != "windows" {
		if err := os.Rename(staged, curBin); err != nil {
			return fmt.Errorf("swapping in the new binary: %w", err)
		}
		return nil
	}
	// Windows: a running exe can be renamed but not replaced or deleted.
	old := curBin + OldSuffix
	if err := os.Remove(old); err != nil && !errors.Is(err, os.ErrNotExist) {
		// A previous .old is still running; pick a fresh name.
		old = curBin + OldSuffix + "-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	moved := false
	if _, err := os.Stat(curBin); err == nil {
		if err := os.Rename(curBin, old); err != nil {
			return fmt.Errorf("moving the running binary aside: %w", err)
		}
		moved = true
	}
	if err := os.Rename(staged, curBin); err != nil {
		if moved {
			if rerr := os.Rename(old, curBin); rerr != nil {
				return fmt.Errorf("swapping in the new binary: %v; restoring the old one also failed: %v (it is at %s)", err, rerr, old)
			}
		}
		return fmt.Errorf("swapping in the new binary: %w", err)
	}
	return nil
}

// copyFile writes src to dst (truncating) and syncs it.
func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, perm)
}

type limitedBuffer struct {
	b *bytes.Buffer
	n int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.n - l.b.Len(); room > 0 {
		if len(p) > room {
			l.b.Write(p[:room])
		} else {
			l.b.Write(p)
		}
	}
	return len(p), nil
}
