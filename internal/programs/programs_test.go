package programs

import "testing"

func TestNamesReturnsACopy(t *testing.T) {
	got := Names()
	if len(got) != 6 {
		t.Fatalf("Names() = %v, want the six programs the library runs", got)
	}
	got[0] = "changed"
	if Names()[0] == "changed" {
		t.Error("Names returned the package's own slice")
	}
}
