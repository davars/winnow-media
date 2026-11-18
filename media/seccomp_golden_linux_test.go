//go:build linux && (amd64 || arm64)

package media

import (
	"encoding/binary"
	"encoding/hex"
	"testing"
)

// TestFilterProgramBytes pins the seccomp program the helper installs, byte for
// byte, in the kernel's struct sock_filter layout.
func TestFilterProgramBytes(t *testing.T) {
	var got []byte
	for _, f := range filterProgram() {
		got = binary.NativeEndian.AppendUint16(got, f.Code)
		got = append(got, f.Jt, f.Jf)
		got = binary.NativeEndian.AppendUint32(got, f.K)
	}
	if g := hex.EncodeToString(got); g != wantFilterProgramHex {
		t.Errorf("filter program changed:\n got %s\nwant %s", g, wantFilterProgramHex)
	}
}
