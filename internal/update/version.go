package update

import (
	"fmt"
	"strconv"
	"strings"
)

// Version is a parsed semantic version: MAJOR.MINOR.PATCH with an optional
// prerelease such as "dev.202611210400.abc1234". Build metadata after "+"
// is accepted and ignored.
type Version struct {
	Major, Minor, Patch int64
	Pre                 []string
}

// ParseVersion parses "1.2.3", "v1.2.3" or "1.2.3-dev.202611210400.abc1234".
func ParseVersion(s string) (Version, error) {
	orig := s
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	var pre string
	if i := strings.IndexByte(s, '-'); i >= 0 {
		s, pre = s[:i], s[i+1:]
		if pre == "" {
			return Version{}, fmt.Errorf("version %q has an empty prerelease", orig)
		}
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("version %q is not MAJOR.MINOR.PATCH", orig)
	}
	var nums [3]int64
	for i, p := range parts {
		if p == "" || !allDigits(p) {
			return Version{}, fmt.Errorf("version %q is not MAJOR.MINOR.PATCH", orig)
		}
		n, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			return Version{}, fmt.Errorf("version %q: %v", orig, err)
		}
		nums[i] = n
	}
	v := Version{Major: nums[0], Minor: nums[1], Patch: nums[2]}
	if pre != "" {
		v.Pre = strings.Split(pre, ".")
		for _, id := range v.Pre {
			if id == "" {
				return Version{}, fmt.Errorf("version %q has an empty prerelease part", orig)
			}
		}
	}
	return v, nil
}

// String formats v without a leading "v".
func (v Version) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if len(v.Pre) > 0 {
		s += "-" + strings.Join(v.Pre, ".")
	}
	return s
}

// Compare returns -1, 0 or 1 following semver 2.0 precedence: a release
// sorts after its prereleases, numeric prerelease parts compare as numbers
// and sort before text parts.
func (v Version) Compare(o Version) int {
	for _, d := range [][2]int64{{v.Major, o.Major}, {v.Minor, o.Minor}, {v.Patch, o.Patch}} {
		if d[0] != d[1] {
			return cmpInt(d[0], d[1])
		}
	}
	switch {
	case len(v.Pre) == 0 && len(o.Pre) == 0:
		return 0
	case len(v.Pre) == 0:
		return 1
	case len(o.Pre) == 0:
		return -1
	}
	for i := 0; i < len(v.Pre) && i < len(o.Pre); i++ {
		if c := cmpIdent(v.Pre[i], o.Pre[i]); c != 0 {
			return c
		}
	}
	return cmpInt(int64(len(v.Pre)), int64(len(o.Pre)))
}

// Newer reports whether candidate is a higher version than current. An
// unparsable current version (a hand build) counts as older than anything.
func Newer(candidate, current string) (bool, error) {
	c, err := ParseVersion(candidate)
	if err != nil {
		return false, err
	}
	cur, err := ParseVersion(current)
	if err != nil {
		return true, nil
	}
	return c.Compare(cur) > 0, nil
}

func cmpIdent(a, b string) int {
	an, bn := allDigits(a), allDigits(b)
	switch {
	case an && bn:
		// Compare as numbers without overflow: longer (after leading zeros) is bigger.
		a, b = strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
		if len(a) != len(b) {
			return cmpInt(int64(len(a)), int64(len(b)))
		}
		return strings.Compare(a, b)
	case an:
		return -1
	case bn:
		return 1
	}
	return strings.Compare(a, b)
}

func cmpInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
