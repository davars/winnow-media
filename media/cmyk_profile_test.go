package media

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// writeCMYKProfile writes a minimal CMYK output-class ICC profile (version 2,
// an 8-bit lookup table from CMYK to Lab on a 2-point grid) and returns its
// path. The values are a plausible subtractive mixing, not a measured press
// condition; tests need a CMYK profile that converts, not a faithful one.
func writeCMYKProfile(t *testing.T) string {
	t.Helper()
	const (
		headerLen = 128
		tagCount  = 1
		tagOffset = headerLen + 4 + 12*tagCount
	)
	var lut []byte
	lut = append(lut, "mft1"...)
	lut = append(lut, 0, 0, 0, 0, 4, 3, 2, 0)
	for _, v := range []int32{1, 0, 0, 0, 1, 0, 0, 0, 1} { // identity matrix, s15Fixed16
		lut = binary.BigEndian.AppendUint32(lut, uint32(v<<16))
	}
	identity := func(n int) {
		for range n {
			for i := range 256 {
				lut = append(lut, byte(i))
			}
		}
	}
	identity(4)
	// Input channels are C, M, Y, K with the first varying slowest. Lab
	// follows the v2 8-bit encoding: L 0..255 for 0..100, a and b offset by 128.
	for c := 0; c < 2; c++ {
		for m := 0; m < 2; m++ {
			for y := 0; y < 2; y++ {
				for k := 0; k < 2; k++ {
					fc, fm, fy, fk := float64(c), float64(m), float64(y), float64(k)
					l := 255 * (1 - 0.92*fk) * (1 - 0.18*fc - 0.12*fm - 0.04*fy)
					a := 128 + 45*(fm-fc)*(1-fk)
					b := 128 + 55*(fy-0.3*fc)*(1-fk)
					lut = append(lut, byte(l), byte(a), byte(b))
				}
			}
		}
	}
	identity(3)

	p := make([]byte, tagOffset, tagOffset+len(lut))
	binary.BigEndian.PutUint32(p[0:], uint32(tagOffset+len(lut)))
	binary.BigEndian.PutUint32(p[8:], 0x02400000)
	copy(p[12:], "prtr")
	copy(p[16:], "CMYK")
	copy(p[20:], "Lab ")
	binary.BigEndian.PutUint16(p[24:], 2026)
	binary.BigEndian.PutUint16(p[26:], 1)
	binary.BigEndian.PutUint16(p[28:], 1)
	copy(p[36:], "acsp")
	binary.BigEndian.PutUint32(p[68:], 63190) // D50 illuminant, s15Fixed16
	binary.BigEndian.PutUint32(p[72:], 65536)
	binary.BigEndian.PutUint32(p[76:], 54061)
	binary.BigEndian.PutUint32(p[headerLen:], tagCount)
	copy(p[headerLen+4:], "A2B0")
	binary.BigEndian.PutUint32(p[headerLen+8:], tagOffset)
	binary.BigEndian.PutUint32(p[headerLen+12:], uint32(len(lut)))
	p = append(p, lut...)

	path := filepath.Join(t.TempDir(), "cmyk.icc")
	if err := os.WriteFile(path, p, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
