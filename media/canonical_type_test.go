package media

import "testing"

func TestCanonicalType(t *testing.T) {
	for _, c := range []struct {
		in, want string
		ok       bool
	}{
		{"image/jpeg", "image/jpeg", true},
		{"image/vnd.microsoft.icon", "image/vnd.microsoft.icon", true},
		{"image/x-win-bitmap", "image/vnd.microsoft.icon", true},
		{"video/MP2T", "video/mp2t", true},
		{"Image/X-Win-Bitmap", "image/vnd.microsoft.icon", true},
		{"IMAGE/JPEG", "image/jpeg", true},
		{"application/pdf", "application/pdf", false},
		{"image/svg+xml", "image/svg+xml", false},
		{"text/plain", "text/plain", false},
		{"", "", false},
	} {
		got, ok := CanonicalType(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("CanonicalType(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
	for alias, target := range typeAliases {
		if _, ok := typeTable[target]; !ok {
			t.Errorf("alias %q names %q, which Types lacks", alias, target)
		}
	}
	for _, typ := range Types() {
		if got, ok := CanonicalType(typ); !ok || got != typ {
			t.Errorf("CanonicalType(%q) = %q, %v; every listed type is its own", typ, got, ok)
		}
	}
}
