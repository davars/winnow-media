package media

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	typeBMP = "image/bmp"

	bmpHeaderLen      = 54
	bmpDIBSizeOffset  = 14
	bmpBitCountOffset = 28
	bmpClrUsedOffset  = 46
)

// bmpNeedsStaging reports whether the leading bytes hdr of a BMP of size bytes
// have a color count ImageMagick refuses: it rejects a biClrUsed above the
// file's size as insufficient data, even at 16 bits per pixel and over, where
// the field means nothing. Any other BMP, including one with a short or
// unreadable header, is read as it is.
func bmpNeedsStaging(hdr []byte, size int64) bool {
	return len(hdr) >= bmpHeaderLen && hdr[0] == 'B' && hdr[1] == 'M' &&
		binary.LittleEndian.Uint32(hdr[bmpDIBSizeOffset:]) >= 40 &&
		binary.LittleEndian.Uint16(hdr[bmpBitCountOffset:]) >= 16 &&
		int64(binary.LittleEndian.Uint32(hdr[bmpClrUsedOffset:])) > size
}

// bmpCopied, when not nil, is called by stageBMP after it has copied; tests
// set it.
var bmpCopied func()

// stageBMP returns a read-only copy of the BMP f holds, made in dir, with the
// color count zeroed, when bmpNeedsStaging says its header needs that, and nil
// otherwise. info is what f's Stat said at open. The copy has the descriptor's
// content as of the copy, so a later change to the original is not seen; a
// change during it is ErrInputChanged. The caller closes the copy and removes
// dir.
func stageBMP(f *os.File, info os.FileInfo, dir string) (*os.File, error) {
	size := info.Size()
	var hdr [bmpHeaderLen]byte
	if n, err := f.ReadAt(hdr[:], 0); n < len(hdr) || (err != nil && err != io.EOF) {
		return nil, nil
	}
	if !bmpNeedsStaging(hdr[:], size) {
		return nil, nil
	}
	clear(hdr[bmpClrUsedOffset : bmpClrUsedOffset+4])
	path := filepath.Join(dir, "bmp")
	dst, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("source: %w", withoutPath(err))
	}
	_, err = dst.Write(hdr[:])
	if err == nil {
		var n int64
		n, err = io.Copy(dst, io.NewSectionReader(f, bmpHeaderLen, size-bmpHeaderLen))
		if err == nil && n != size-bmpHeaderLen {
			err = fmt.Errorf("source: %w while it was opened", ErrInputChanged)
		}
	}
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	if bmpCopied != nil {
		bmpCopied()
	}
	cur, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("source: %w", withoutPath(err))
	}
	if idOf(cur) != idOf(info) || unlinked(cur) {
		return nil, fmt.Errorf("source: %w while it was opened", ErrInputChanged)
	}
	return os.Open(path)
}
