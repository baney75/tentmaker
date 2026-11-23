package core

import "testing"

// A job's cache outcome is read more than once; a hit seen once stays.
func TestMergeCache(t *testing.T) {
	for _, tt := range []struct{ old, v, want string }{
		{"", "", ""},
		{"", CacheMiss, CacheMiss},
		{"", CacheHit, CacheHit},
		{CacheMiss, CacheHit, CacheHit},
		{CacheHit, CacheMiss, CacheHit},
		{CacheHit, "", CacheHit},
		{CacheMiss, "", CacheMiss},
	} {
		if got := MergeCache(tt.old, tt.v); got != tt.want {
			t.Errorf("MergeCache(%q, %q) = %q, want %q", tt.old, tt.v, got, tt.want)
		}
	}
}
