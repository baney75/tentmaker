package daemon

import (
	"net"
	"strconv"
	"testing"

	"github.com/baney75/tentmaker/internal/control"
	"github.com/baney75/tentmaker/internal/dispatch"
	"github.com/baney75/tentmaker/internal/executor/linux"
	"github.com/baney75/tentmaker/internal/executor/windows"
)

// The job fence opens exactly the relay port, and the relay and control API
// listen on different ports, so jobs can't reach status, pool or doctor
// (review r1 partB first-F1).
func TestFenceOpensRelayPortNotControlPort(t *testing.T) {
	port := func(addr string) int {
		_, p, err := net.SplitHostPort(addr)
		if err != nil {
			t.Fatal(err)
		}
		n, _ := strconv.Atoi(p)
		return n
	}
	relay, ctl := port(control.DefaultRelayAddr), port(control.TCPAddr)
	if relay == ctl {
		t.Fatalf("relay and control API share port %d", relay)
	}
	if port(dispatch.DefaultRelayAddr) != relay {
		t.Errorf("dispatch relay %s, control relay %s", dispatch.DefaultRelayAddr, control.DefaultRelayAddr)
	}
	for name, p := range map[string]int{"linux": linux.RelayPort, "windows": windows.RelayPort} {
		if p != relay {
			t.Errorf("the %s fence opens port %d, the relay listens on %d", name, p, relay)
		}
	}
}

// The Linux fence opens the Actions cache port the service listens on, and
// it is neither the relay's nor the control API's.
func TestFenceOpensActionsCachePort(t *testing.T) {
	_, p, err := net.SplitHostPort(control.DefaultCacheAddr)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(p)
	if n != linux.CachePort {
		t.Errorf("the linux fence opens port %d, the Actions cache listens on %d", linux.CachePort, n)
	}
	if n == linux.RelayPort || p == strconv.Itoa(port0(control.TCPAddr)) {
		t.Errorf("the Actions cache shares port %d with the relay or control API", n)
	}
}

func port0(addr string) int {
	_, p, _ := net.SplitHostPort(addr)
	n, _ := strconv.Atoi(p)
	return n
}

// The control API refuses the account the executors run CI jobs as.
func TestControlRefusesTheExecutorsRunnerUser(t *testing.T) {
	if linux.User != RunnerUser || windows.User != RunnerUser {
		t.Fatalf("runner users: linux %q, windows %q, control refuses %q", linux.User, windows.User, RunnerUser)
	}
}
