package guards

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestHelperProcess is the command the runner tests start (this test binary
// again), so they never run a real host command.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("TENTMAKER_TEST_HELPER") != "1" {
		return
	}
	switch os.Getenv("TENTMAKER_TEST_MODE") {
	case "ok":
		fmt.Print("LC_ALL=" + os.Getenv("LC_ALL"))
		os.Exit(0)
	case "fail":
		fmt.Fprint(os.Stderr, "no such battery")
		os.Exit(3)
	case "hang":
		time.Sleep(time.Minute)
	}
	os.Exit(9)
}

func helper(t *testing.T, mode string) {
	t.Setenv("TENTMAKER_TEST_HELPER", "1")
	t.Setenv("TENTMAKER_TEST_MODE", mode)
}

func TestRunCommand(t *testing.T) {
	ctx := context.Background()
	helper(t, "ok")
	out, err := runCommand(ctx, os.Args[0], "-test.run=^TestHelperProcess$")
	if err != nil || string(out) != "LC_ALL=C" {
		t.Fatalf("ok: %q, %v", out, err)
	}
	helper(t, "fail")
	if _, err := runCommand(ctx, os.Args[0], "-test.run=^TestHelperProcess$"); err == nil || !strings.Contains(err.Error(), "no such battery") {
		t.Fatalf("fail: %v", err)
	}
	helper(t, "hang")
	old := cmdTimeout
	cmdTimeout = 200 * time.Millisecond
	defer func() { cmdTimeout = old }()
	start := time.Now()
	_, err = runCommand(ctx, os.Args[0], "-test.run=^TestHelperProcess$")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("hang: %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("timeout took %s", time.Since(start))
	}
}
