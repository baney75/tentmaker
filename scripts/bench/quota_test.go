package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// The bench waits for the reset when the shared limit is too low for its
// devices to poll unpaced, and starts at once when it isn't. Before, it
// started regardless: in run 45182016071 (issue #39) earlier runs had left
// the devices 161 to 326 of 5000, and pacing alone put the median job start
// at 22.5 s.
func TestQuotaWait(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	reset := now.Add(20 * time.Minute).Unix()
	for _, tc := range []struct {
		name string
		c    rateCore
		want time.Duration
	}{
		{"plenty", rateCore{5000, 4900, reset}, 0},
		{"exactly enough", rateCore{5000, 1250 + QuotaNeed, reset}, 0},
		{"drained by earlier runs", rateCore{5000, 326, reset}, 20*time.Minute + 5*time.Second},
		{"one short", rateCore{5000, 1250 + QuotaNeed - 1, reset}, 20*time.Minute + 5*time.Second},
		{"reset already passed", rateCore{5000, 100, now.Add(-time.Minute).Unix()}, 0},
		{"reset far off is bounded", rateCore{5000, 100, now.Add(3 * time.Hour).Unix()}, quotaMaxWait},
		{"small limit: a fresh hour is the best", rateCore{1000, 1000, reset}, 0},
		{"small limit, used", rateCore{1000, 999, reset}, 20*time.Minute + 5*time.Second},
		{"no reading", rateCore{}, 0},
	} {
		if got := QuotaWait(tc.c, now, QuotaNeed); got != tc.want {
			t.Errorf("%s: wait %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestRunQuota(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	in := `{"resources":{"core":{"limit":5000,"remaining":300,"reset":1800000600}}}`
	var out, errb bytes.Buffer
	if rc := runQuota(nil, strings.NewReader(in), &out, &errb, now); rc != 0 {
		t.Fatalf("rc %d: %s", rc, errb.String())
	}
	if got := strings.TrimSpace(out.String()); got != "605" {
		t.Fatalf("printed %q, want 605", got)
	}
	if rc := runQuota(nil, strings.NewReader("nope"), &out, &errb, now); rc != 1 {
		t.Fatalf("bad JSON: rc %d, want 1", rc)
	}
}
