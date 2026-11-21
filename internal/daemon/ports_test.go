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

// The control API refuses the account the executors run CI jobs as.
func TestControlRefusesTheExecutorsRunnerUser(t *testing.T) {
	if linux.User != RunnerUser || windows.User != RunnerUser {
		t.Fatalf("runner users: linux %q, windows %q, control refuses %q", linux.User, windows.User, RunnerUser)
	}
}
