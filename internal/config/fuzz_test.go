package config

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// FuzzParseSize: no panic, never negative or past MaxSize, and String
// parses back to the same size.
func FuzzParseSize(f *testing.F) {
	for _, s := range []string{"0", "4GiB", "4G", "4GB", "512MiB", "1.5KiB", "8gib", " 2 TiB ", "1B", "0.1K",
		"99999999999999999999T", "1e9G", "", "-1GiB", "NaN", "1.", "00000000000000000000001K"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		v, err := ParseSize(s)
		if err != nil {
			return
		}
		if v < 0 || v > MaxSize {
			t.Fatalf("ParseSize(%q) = %d, outside 0..MaxSize", s, int64(v))
		}
		back, err := ParseSize(v.String())
		if err != nil || back != v {
			t.Fatalf("ParseSize(%q) = %d; String %q parses to %d, %v", s, int64(v), v.String(), int64(back), err)
		}
	})
}

// FuzzConfigRoundTrip: a file Load accepts survives Validate, Save and
// Load unchanged, and nothing panics on any input.
func FuzzConfigRoundTrip(f *testing.F) {
	seeds := []string{
		"",
		"device = \"box\"\n[budget.everyday]\nmemory = \"4GiB\"\ncpus = 2\n",
		"[budget]\non_battery = \"off\"\npause_below_percent = 30\nstop_below_percent = 10\n[[budget.schedule]]\nname = \"nights\"\ndays = [\"mon\", \"fri\"]\nfrom = \"22:00\"\nto = \"07:00\"\nprofile = \"boost\"\n",
		"[job]\nmemory = \"3GiB\"\ncpus = 1.5\nmax_duration = \"2h\"\nidle_ttl = \"90s\"\nmax_slots = 2\n",
		"[[repos]]\nname = \"a/b\"\nmode = \"checks\"\nbranches = [\"release/1.2\"]\nworkflows = []\n[forges.work]\nkind = \"forgejo\"\nurl = \"https://git.example.com\"\n",
		"labels = []\ndeny_repo_patterns = [\"x/*\"]\n[guards]\nmin_free_disk = \"1KiB\"\nmax_load_per_cpu = 0.5\npause_when_running = [\"steam.exe\"]\n",
		"[job]\ncpus = nan\n",
		"[budget.boost]\ncpus = inf\nmemory = \"1023B\"\n",
		"[mesh]\nenabled = false\nport = 70000\nmembers = [\"a@b\"]\n[update]\nchannel = \"dev\"\nauto = false\n",
		"cache_max = \"0.5K\"\n",
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		dir := t.TempDir()
		in := filepath.Join(dir, "in.toml")
		if err := os.WriteFile(in, data, 0o600); err != nil {
			t.Fatal(err)
		}
		c1, err := Load(in, 16<<30, 8)
		if err != nil {
			return
		}
		if len(c1.Validate()) == 0 {
			// A config Validate accepts must not hand the budget NaN,
			// infinite or negative CPUs, or a negative slot cap.
			for _, v := range []float64{c1.Budget.Everyday.CPUs, c1.Budget.Boost.CPUs, c1.Job.CPUs} {
				if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
					t.Fatalf("Validate accepted cpus = %v\ninput:\n%s", v, data)
				}
			}
			if c1.Job.MaxSlots < 0 {
				t.Fatalf("Validate accepted max_slots = %d\ninput:\n%s", c1.Job.MaxSlots, data)
			}
		}
		out := filepath.Join(dir, "out.toml")
		if err := Save(out, c1); err != nil {
			t.Fatalf("Save of a loaded config failed: %v\ninput:\n%s", err, data)
		}
		c2, err := Load(out, 16<<30, 8)
		if err != nil {
			saved, _ := os.ReadFile(out)
			t.Fatalf("Load of a saved config failed: %v\nsaved:\n%s", err, saved)
		}
		// %#v prints maps sorted and tells nil from empty; NaN prints as NaN.
		if a, b := fmt.Sprintf("%#v", c1), fmt.Sprintf("%#v", c2); a != b {
			saved, _ := os.ReadFile(out)
			t.Fatalf("round trip changed the config\nbefore: %s\nafter:  %s\nsaved:\n%s", a, b, saved)
		}
	})
}
