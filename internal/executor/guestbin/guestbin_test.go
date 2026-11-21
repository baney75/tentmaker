package guestbin

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSync(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	places := 0
	place := func(s, d string) error {
		places++
		b, err := os.ReadFile(s)
		if err != nil {
			return err
		}
		return os.WriteFile(d, b, 0o755)
	}
	var sy Syncer
	os.WriteFile(src, []byte("v1"), 0o755)
	if copied, err := sy.Sync(src, dst, place); err != nil || !copied {
		t.Fatalf("missing copy: %v %v", copied, err)
	}
	if copied, _ := sy.Sync(src, dst, place); copied {
		t.Fatal("copied an equal file")
	}
	// Same size, new content (an update): copied.
	os.WriteFile(src, []byte("v2"), 0o755)
	os.Chtimes(src, time.Now().Add(time.Minute), time.Now().Add(time.Minute))
	if copied, _ := sy.Sync(src, dst, place); !copied {
		t.Fatal("stale copy kept")
	}
	if b, _ := os.ReadFile(dst); string(b) != "v2" {
		t.Fatalf("dst = %q", b)
	}
	// Equal content with a new mtime: hashed, not copied.
	os.Chtimes(dst, time.Now().Add(2*time.Minute), time.Now().Add(2*time.Minute))
	if copied, _ := sy.Sync(src, dst, place); copied {
		t.Fatal("copied equal content")
	}
	if places != 2 {
		t.Fatalf("%d places", places)
	}
	if _, err := sy.Sync(filepath.Join(dir, "nope"), dst, place); err == nil {
		t.Fatal("a missing source was accepted")
	}
}
