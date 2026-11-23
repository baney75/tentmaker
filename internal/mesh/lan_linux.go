package mesh

import (
	"os"
	"path/filepath"
	"strings"
)

// sysNet is where Linux lists network interfaces; tests point it at a
// temporary tree.
var sysNet = "/sys/class/net"

// osWakeCheck reads sysfs, with no ethtool: an interface with a device
// link is a physical adapter, one with a wireless (or phy80211) entry is
// Wi-Fi, and its device's power/wakeup says whether the driver lets it wake
// the machine. A wakeup file that is missing or unreadable is "can't wake":
// fail closed.
func osWakeCheck(name string) ifaceWake {
	if strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return ifaceWake{}
	}
	dir := filepath.Join(sysNet, name)
	if _, err := os.Stat(filepath.Join(dir, "device")); err != nil {
		return ifaceWake{}
	}
	w := ifaceWake{Physical: true, Wired: true}
	for _, sub := range []string{"wireless", "phy80211"} {
		if _, err := os.Stat(filepath.Join(dir, sub)); err == nil {
			w.Wired = false
		}
	}
	b, err := os.ReadFile(filepath.Join(dir, "device", "power", "wakeup"))
	switch v := strings.TrimSpace(string(b)); {
	case err != nil:
		w.Note = "the adapter reports no wakeup setting in sysfs, so it can't wake the device"
	case v == "enabled":
		w.Wakeable = true
		w.Note = "sysfs: device/power/wakeup is enabled"
	default:
		w.Note = "wakeup is " + v + " for " + name + "; enable it (ethtool -s " + name + " wol g, and Wake-on-LAN in the firmware)"
	}
	if !w.Wired && w.Wakeable {
		w.Note += "; a wireless adapter wakes only if it supports WoWLAN"
	}
	return w
}
