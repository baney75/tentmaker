package mesh

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLinuxWakeCheckReadsSysfs(t *testing.T) {
	root := t.TempDir()
	old := sysNet
	sysNet = root
	t.Cleanup(func() { sysNet = old })
	mk := func(name, wakeup string, wireless bool) {
		dev := filepath.Join(root, name, "device", "power")
		if err := os.MkdirAll(dev, 0o755); err != nil {
			t.Fatal(err)
		}
		if wakeup != "" {
			if err := os.WriteFile(filepath.Join(dev, "wakeup"), []byte(wakeup+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if wireless {
			if err := os.MkdirAll(filepath.Join(root, name, "wireless"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk("enp3s0", "enabled", false)
	mk("enp4s0", "disabled", false)
	mk("enp5s0", "", false)
	mk("wlp2s0", "enabled", true)
	if err := os.MkdirAll(filepath.Join(root, "veth0"), 0o755); err != nil {
		t.Fatal(err)
	}

	if w := osWakeCheck("enp3s0"); !w.Physical || !w.Wired || !w.Wakeable {
		t.Errorf("enabled: %+v", w)
	}
	if w := osWakeCheck("enp4s0"); !w.Physical || w.Wakeable || !strings.Contains(w.Note, "ethtool -s enp4s0 wol g") {
		t.Errorf("disabled: %+v", w)
	}
	if w := osWakeCheck("enp5s0"); !w.Physical || w.Wakeable {
		t.Errorf("no wakeup file must fail closed: %+v", w)
	}
	if w := osWakeCheck("wlp2s0"); w.Wired || !w.Wakeable || !strings.Contains(w.Note, "WoWLAN") {
		t.Errorf("wireless: %+v", w)
	}
	if w := osWakeCheck("veth0"); w.Physical {
		t.Errorf("virtual: %+v", w)
	}
	if w := osWakeCheck("../etc"); w.Physical {
		t.Errorf("path in a name: %+v", w)
	}
}
