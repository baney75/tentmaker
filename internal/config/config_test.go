package config

import "testing"

func TestParseSize(t *testing.T) {
	good := map[string]Size{
		"4GiB": 4 * GiB, "4gib": 4 * GiB, "4G": 4 * GiB, "4GB": 4 * GiB, "4 GiB": 4 * GiB,
		"512MiB": 512 * MiB, "512M": 512 * MiB, "1.5GiB": Size(1.5 * float64(GiB)), "0": 0, "2TiB": 2 << 40,
	}
	for in, want := range good {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "4", "abc", "-1GiB", "4XB", "4GiBs"} {
		if _, err := ParseSize(in); err == nil {
			t.Errorf("ParseSize(%q) accepted", in)
		}
	}
}

func TestSizeRoundTrip(t *testing.T) {
	for _, s := range []Size{0, 4 * GiB, 512 * MiB, 3 * KiB} {
		got, err := ParseSize(s.String())
		if err != nil || got != s {
			t.Errorf("round trip %v: %v %v", s, got, err)
		}
	}
}

func TestDefaultValidates(t *testing.T) {
	for _, mem := range []int64{4 * int64(GiB), 8 * int64(GiB), 16 * int64(GiB), 64 * int64(GiB), 256 * int64(GiB)} {
		c := Default(mem, 8)
		if err := c.Validate(); err != nil {
			t.Errorf("Default(%d GiB) invalid: %v", mem/int64(GiB), err)
		}
	}
}
