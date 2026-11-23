package cli

import (
	"context"
	"strings"
	"testing"
)

// The controller refuses a timeout the tool should have refused, so a
// fraction can never truncate to 0 (no limit) on the way to the daemon.
func TestMCPSandboxExecRefusesFractionalTimeout(t *testing.T) {
	for _, bad := range []float64{0.5, -1, 1e300} {
		_, err := controller{}.SandboxExec(context.Background(), "sbx-0123456789ab", []string{"true"}, "", bad, "")
		if err == nil || !strings.Contains(err.Error(), "whole number") {
			t.Errorf("timeout %g: %v", bad, err)
		}
	}
}
