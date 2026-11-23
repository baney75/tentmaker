//go:build !linux

package mesh

import "strings"

// osWakeCheck on macOS and Windows: the standard library can't read an
// adapter's wake setting, so an adapter that passed pickLAN's checks is
// assumed physical and wakeable. Names that say Wi-Fi count as wireless.
// mesh.wake = false opts a device out, and a wake that brings no device
// online backs off.
func osWakeCheck(name string) ifaceWake {
	n := strings.ToLower(name)
	wired := true
	for _, w := range []string{"wi-fi", "wifi", "wlan", "wireless", "airport"} {
		if strings.Contains(n, w) {
			wired = false
		}
	}
	return ifaceWake{Physical: true, Wired: wired, Wakeable: true,
		Note: "assumed: this OS gives no standard check; set wake = false under [mesh] to opt out"}
}
