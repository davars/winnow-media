package media

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

const (
	// keptMaxBytesPerPixel is what the budget counts for each pixel of a kept
	// decode before it is written: five channels of 16 bits, the widest PAM
	// there is (CMYK with alpha).
	keptMaxBytesPerPixel = 10
	// maxKeptHeader bounds the PAM header read back.
	maxKeptHeader = 256
	// maxKeptProfile bounds a kept ICC profile.
	maxKeptProfile = 16 << 20
)

// keepPlan asks a still encode to write the image it decodes, upright and at
// full size, as a 16-bit PAM, and its ICC profile when it has one.
type keepPlan struct {
	pam, icc string
	// line is the "keep|" line the run printed right after the read.
	line string
	// err is why the run's profile cannot be kept, if it is not.
	err error
}

// keepLinePrefix starts the line a keep run prints right after the read: the
// resolution (0 when the file gives none), units, colorspace and depth the
// image was read with, its class and alpha trait, and the JPEG sampling
// factor and quantum format the reader set, empty when it set none. A reader
// that does not set a property makes ImageMagick warn and print nothing for
// it, which the run tolerates.
const keepLinePrefix = "keep|"

// prelude returns the operators that report what the read gave, turn the
// frame upright by orientation and write the files, ahead of the targets' own
// operators. The profile is written only when the image carries it.
func (k *keepPlan) prelude(orientation string, writeProfile bool) []string {
	ops := []string{"-precision", "17", "-print", keepLinePrefix + "%[fx:resolution.x]|%[fx:resolution.y]|%U|%[colorspace]|%z|%r|%A|%[jpeg:sampling-factor]|%[quantum:format]\\n", "-precision", "6"}
	ops = append(ops, orientOps(orientation)...)
	if writeProfile && k.icc != "" {
		ops = append(ops, "-write", "icc:"+k.icc)
	}
	return append(ops, "-depth", "16", "-write", "pam:"+k.pam)
}

// keptDecode is the decode of one image of a still that a Source keeps in its
// scratch, with what read-back checks of it.
type keptDecode struct {
	pam    string
	id     fileID
	header []byte
	w, h   int
	depth  int
	tuple  string
	soft   bool
	// xres, yres and units are the resolution the image was read with, which
	// the PAM does not carry.
	xres, yres, units string
	// sampling is the JPEG sampling factor the reader recorded, which the
	// JPEG writer reuses; empty for any other source.
	sampling string

	icc     string
	iccSize int64
	iccSum  [sha256.Size]byte
}

// pamTuples maps the tuple types the PAM writer produces to their depth.
var pamTuples = map[string]int{
	"GRAYSCALE": 1, "GRAYSCALE_ALPHA": 2, "RGB": 3, "RGB_ALPHA": 4, "CMYK": 4, "CMYK_ALPHA": 5,
}

// pamHeader is the header of a 16-bit PAM of the given size.
func pamHeader(w, h, depth int, tuple string) []byte {
	return fmt.Appendf(nil, "P7\nWIDTH %d\nHEIGHT %d\nDEPTH %d\nMAXVAL 65535\nTUPLTYPE %s\nENDHDR\n", w, h, depth, tuple)
}

// parsePAMHeader reads header, which must be exactly what pamHeader writes
// for a size and tuple type in pamTuples.
func parsePAMHeader(header []byte) (w, h, depth int, tuple string, err error) {
	lines := strings.Split(strings.TrimSuffix(string(header), "\n"), "\n")
	if len(lines) != 7 {
		return 0, 0, 0, "", errors.New("unexpected header")
	}
	if _, err := fmt.Sscanf(lines[1], "WIDTH %d", &w); err != nil {
		return 0, 0, 0, "", errors.New("unreadable WIDTH")
	}
	if _, err := fmt.Sscanf(lines[2], "HEIGHT %d", &h); err != nil {
		return 0, 0, 0, "", errors.New("unreadable HEIGHT")
	}
	tuple, ok := strings.CutPrefix(lines[5], "TUPLTYPE ")
	if !ok {
		return 0, 0, 0, "", errors.New("unreadable TUPLTYPE")
	}
	depth, ok = pamTuples[tuple]
	switch {
	case !ok:
		return 0, 0, 0, "", fmt.Errorf("tuple type %q", tuple)
	case w < 1 || h < 1 || w > maxDimension || h > maxDimension:
		return 0, 0, 0, "", fmt.Errorf("size %dx%d", w, h)
	case !bytes.Equal(header, pamHeader(w, h, depth, tuple)):
		return 0, 0, 0, "", errors.New("unexpected header")
	}
	return w, h, depth, tuple, nil
}

// keptCheckError says a kept file is not what was written. Any other error
// from the read-back is an I/O failure, which does not say the kept decode is
// bad.
type keptCheckError struct{ msg string }

func (e *keptCheckError) Error() string { return e.msg }

func checkFailed(format string, args ...any) error {
	return &keptCheckError{fmt.Sprintf(format, args...)}
}

// failedCheck reports whether err says a kept file failed its checks.
func failedCheck(err error) bool {
	var c *keptCheckError
	return errors.As(err, &c)
}

// openKept opens the file at path for reading without following a symlink
// and returns it with its status. A file that is missing, a symlink or not a
// regular file fails a check; any other error is an I/O failure.
func openKept(path string) (*os.File, os.FileInfo, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	switch {
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ELOOP):
		return nil, nil, checkFailed("the file is gone or replaced by a link")
	case err != nil:
		return nil, nil, err
	}
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = checkFailed("not a regular file")
	}
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	return f, info, nil
}

// readPAMHeader returns the header of the PAM f holds, through ENDHDR.
func readPAMHeader(f *os.File) ([]byte, error) {
	buf := make([]byte, maxKeptHeader)
	n, err := f.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	end := bytes.Index(buf[:n], []byte("ENDHDR\n"))
	if end < 0 {
		return nil, checkFailed("no header")
	}
	return buf[:end+len("ENDHDR\n")], nil
}

// pamSize is the size of a PAM file with the given header length and image.
func pamSize(header, w, h, depth int) int64 {
	return int64(header) + int64(w)*int64(h)*int64(depth)*2
}

// readProfile reads the profile file at path.
func readProfile(path string) ([]byte, error) {
	f, info, err := openKept(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if info.Size() > maxKeptProfile {
		return nil, checkFailed("profile of %d bytes", info.Size())
	}
	return io.ReadAll(io.LimitReader(f, maxKeptProfile+1))
}

// copyProfile copies the profile file from to a new file to.
func copyProfile(from, to string) error {
	data, err := readProfile(from)
	if err != nil {
		return err
	}
	return os.WriteFile(to, data, 0o600)
}

// keepFacts is what a keep run reported of the image it read.
type keepFacts struct {
	xres, yres, units, sampling string
	soft                        bool
}

// samplingFactor is the form of a JPEG sampling factor property.
var samplingFactor = regexp.MustCompile(`^\d+x\d+(,\d+x\d+)*$`)

// keptColorspaces are the colorspaces a PAM carries exactly.
var keptColorspaces = map[string]bool{"sRGB": true, "RGB": true, "Gray": true, "LinearGray": true, "CMYK": true}

// keptUnits are the resolution units ImageMagick reports.
var keptUnits = map[string]bool{"Undefined": true, "PixelsPerInch": true, "PixelsPerCentimeter": true}

// parseKeepFacts reads the line of a keep run and returns an error unless the
// image is one whose 16-bit PAM is exact: an integer depth of at most 16 in a
// colorspace the PAM carries, with a sampling factor of the form ImageMagick
// writes.
func parseKeepFacts(line string) (keepFacts, error) {
	f := strings.Split(strings.TrimPrefix(line, keepLinePrefix), "|")
	if !strings.HasPrefix(line, keepLinePrefix) || len(f) != 9 {
		return keepFacts{}, errors.New("no report of the read")
	}
	depth, err := strconv.Atoi(f[4])
	for _, v := range f[:2] {
		if _, perr := strconv.ParseFloat(v, 64); perr != nil {
			err = perr
		}
	}
	switch {
	case err != nil:
		return keepFacts{}, fmt.Errorf("unreadable report of the read: %w", err)
	case !keptUnits[f[2]]:
		return keepFacts{}, fmt.Errorf("resolution units %q", f[2])
	case !keptColorspaces[f[3]]:
		return keepFacts{}, fmt.Errorf("colorspace %q", f[3])
	case depth < 1 || depth > 16:
		return keepFacts{}, fmt.Errorf("depth %d", depth)
	case f[7] != "" && !samplingFactor.MatchString(f[7]):
		return keepFacts{}, fmt.Errorf("sampling factor %q", f[7])
	case f[8] != "" && f[8] != "unsigned":
		return keepFacts{}, fmt.Errorf("quantum format %q", f[8])
	}
	return keepFacts{xres: f[0], yres: f[1], units: f[2], sampling: f[7], soft: declaresSoft(f[5], f[6])}, nil
}

// splitKeep separates the keep line from the output of a run.
func splitKeep(out []byte) (rest []byte, line string) {
	var kept []string
	for _, l := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(l, keepLinePrefix) {
			line = l
			continue
		}
		kept = append(kept, l)
	}
	return []byte(strings.Join(kept, "\n")), line
}

// planKeep returns the plan for keeping the decode of image k, or nil when
// the Source does not keep decodes of it: Reuse is not set, the type's kept
// decode is not exact, an earlier call found the image cannot be kept, the
// kept decodes and a read-back's copy of them would exceed Resources.Disk.
func (s *Source) planKeep(k int, es *EnvSource) *keepPlan {
	if !s.reuse || !typeTable[s.typ].keptExact || s.noKeep[k] {
		return nil
	}
	need := int64(es.stored[0]) * int64(es.stored[1]) * keptMaxBytesPerPixel
	if 2*(s.keptBytes()+need) > s.p.res.Disk {
		return nil
	}
	kp := &keepPlan{
		pam: filepath.Join(s.dir, fmt.Sprintf(".kept-%d.pam", k)),
		icc: filepath.Join(s.dir, fmt.Sprintf(".kept-%d.icc", k)),
	}
	removeKeepFiles(kp)
	return kp
}

// removeKeepFiles deletes what a plan's run wrote.
func removeKeepFiles(kp *keepPlan) {
	_ = os.Remove(kp.pam)
	_ = os.Remove(kp.icc)
}

// adoptKept records the decode the run of es's plan wrote, if it is what the
// Source can read back; otherwise it deletes the files and keeps nothing,
// and remembers the image as one that cannot be kept when another call would
// not do better. The encode that wrote them is not affected.
func (s *Source) adoptKept(k int, es *EnvSource) {
	kp := es.keep
	es.keep = nil
	kd, err := s.recordKept(kp, es)
	if err != nil {
		removeKeepFiles(kp)
		if failedCheck(err) {
			s.noKeep[k] = true
		}
		return
	}
	s.kept[k] = kd
}

// recordKept returns the record of the kept decode kp's run wrote. Its error
// is a failed check (see failedCheck) when no later call would do better, and
// otherwise an I/O failure or a shortage of room that another call may not
// meet.
func (s *Source) recordKept(kp *keepPlan, es *EnvSource) (*keptDecode, error) {
	if kp.err != nil {
		return nil, kp.err
	}
	facts, err := parseKeepFacts(kp.line)
	if err != nil {
		return nil, checkFailed("%v", err)
	}
	f, info, err := openKept(kp.pam)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	header, err := readPAMHeader(f)
	if err != nil {
		return nil, err
	}
	w, h, depth, tuple, err := parsePAMHeader(header)
	if err != nil {
		return nil, checkFailed("%v", err)
	}
	if info.Size() != pamSize(len(header), w, h, depth) {
		return nil, checkFailed("unexpected size")
	}
	var profile []byte
	if kp.icc != "" {
		if profile, err = readProfile(kp.icc); err != nil {
			return nil, err
		}
	}
	// A read-back copies the decode and its profile again.
	size := info.Size() + int64(len(profile))
	switch {
	case 2*size > s.p.res.Disk:
		return nil, checkFailed("over the disk budget")
	case 2*(s.keptBytes()+size) > s.p.res.Disk:
		return nil, errors.New("over the disk budget with the other kept decodes")
	case es.unsettled || es.upright != [2]int{w, h}:
		return nil, checkFailed("size differs from the header read")
	}
	kd := &keptDecode{
		pam: kp.pam, id: idOf(info), header: header, w: w, h: h, depth: depth, tuple: tuple, soft: facts.soft,
		xres: facts.xres, yres: facts.yres, units: facts.units, sampling: facts.sampling,
	}
	if kp.icc != "" {
		kd.icc, kd.iccSize, kd.iccSum = kp.icc, int64(len(profile)), sha256.Sum256(profile)
	}
	return kd, nil
}

// dropKept deletes the kept decode of image k.
func (s *Source) dropKept(k int) {
	kd := s.kept[k]
	if kd == nil {
		return
	}
	_ = os.Remove(kd.pam)
	if kd.icc != "" {
		_ = os.Remove(kd.icc)
	}
	delete(s.kept, k)
}

// dropKeptExcept deletes the kept decodes of the images not in idx.
func (s *Source) dropKeptExcept(idx []int) {
	for k := range s.kept {
		if !slices.Contains(idx, k) {
			s.dropKept(k)
		}
	}
}

// keptBytes is the disk the kept decodes and their profiles hold.
func (s *Source) keptBytes() int64 {
	var n int64
	for _, kd := range s.kept {
		n += pamSize(len(kd.header), kd.w, kd.h, kd.depth) + kd.iccSize
	}
	return n
}

// copyKept writes the region r (the whole image when zero) of the kept decode
// to a new PAM in the scratch directory and returns its path. The kept file
// is opened without following a symlink and must still be the file that was
// written: same identity, same header, same size. Only Go reads it. An error
// that is not a failed check (see failedCheck) is an I/O failure.
func (s *Source) copyKept(ctx context.Context, kd *keptDecode, r region) (path string, err error) {
	f, info, err := openKept(kd.pam)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	if idOf(info) != kd.id {
		return "", checkFailed("the file changed since it was written")
	}
	if info.Size() != pamSize(len(kd.header), kd.w, kd.h, kd.depth) {
		return "", checkFailed("the file's size changed since it was written")
	}
	header := make([]byte, len(kd.header))
	if n, rerr := f.ReadAt(header, 0); !bytes.Equal(header[:n], kd.header) {
		return "", checkFailed("the header changed since it was written")
	} else if rerr != nil {
		return "", rerr
	}
	x, y, cw, ch := 0, 0, kd.w, kd.h
	if !r.zero() {
		if err := r.check(kd.w, kd.h); err != nil {
			return "", err
		}
		x, y, cw, ch = r.x, r.y, r.width, r.height
	}
	out, err := os.CreateTemp(s.dir, ".call-*.pam")
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(out.Name())
		}
	}()
	bw := bufio.NewWriterSize(out, 1<<20)
	if _, err = bw.Write(pamHeader(cw, ch, kd.depth, kd.tuple)); err != nil {
		_ = out.Close()
		return "", err
	}
	px := int64(kd.depth) * 2
	row := make([]byte, int64(cw)*px)
	for j := range ch {
		if j%64 == 0 {
			if err = ctx.Err(); err != nil {
				_ = out.Close()
				return "", err
			}
		}
		at := int64(len(header)) + (int64(y+j)*int64(kd.w)+int64(x))*px
		if _, err = f.ReadAt(row, at); err == nil {
			_, err = bw.Write(row)
		}
		if err != nil {
			_ = out.Close()
			return "", err
		}
	}
	if err = bw.Flush(); err == nil {
		err = out.Close()
	} else {
		_ = out.Close()
	}
	if err != nil {
		return "", err
	}
	after, statErr := f.Stat()
	if statErr != nil {
		err = statErr
		return "", err
	}
	if idOf(after) != kd.id {
		err = checkFailed("the file changed while it was read")
		return "", err
	}
	return out.Name(), nil
}

// copyKeptProfile writes the kept profile, checked against the digest
// recorded when it was written, to a new file and returns its path.
func (s *Source) copyKeptProfile(kd *keptDecode) (path string, err error) {
	data, err := readProfile(kd.icc)
	if err != nil {
		return "", err
	}
	if int64(len(data)) != kd.iccSize || sha256.Sum256(data) != kd.iccSum {
		return "", checkFailed("the profile changed since it was written")
	}
	out, err := os.CreateTemp(s.dir, ".call-*.icc")
	if err != nil {
		return "", err
	}
	_, err = out.Write(data)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(out.Name())
		return "", err
	}
	return out.Name(), nil
}

// readKept encodes encs from the kept decode of image k, in one ImageMagick
// run that reads a per-call copy of the region. If the kept files fail their
// checks they are dropped and used is false, and the caller encodes the image
// afresh. An error is an I/O or encode failure, which leaves the kept decode
// as it is.
func (s *Source) readKept(ctx context.Context, es *EnvSource, k int, r region, encs []encoding, paths []string) (res []encodedFrame, used bool, err error) {
	kd := s.kept[k]
	pam, err := s.copyKept(ctx, kd, r)
	if failedCheck(err) {
		s.dropKept(k)
		return nil, false, nil
	} else if err != nil {
		return nil, true, err
	}
	defer func() { _ = os.Remove(pam) }()
	frame := encodeFrame{input: "pam:" + pam, filter: filterFor(kd.soft)}
	if kd.units != "Undefined" || kd.xres != "0" || kd.yres != "0" {
		frame.prelude = []string{"-units", kd.units, "-density", kd.xres + "x" + kd.yres}
	}
	if kd.sampling != "" {
		frame.prelude = append(frame.prelude, "-set", "jpeg:sampling-factor", kd.sampling)
	}
	if kd.icc != "" {
		srgb, err := es.srgb()
		if err != nil {
			return nil, true, err
		}
		assign, err := s.copyKeptProfile(kd)
		if failedCheck(err) {
			s.dropKept(k)
			return nil, false, nil
		} else if err != nil {
			return nil, true, err
		}
		defer func() { _ = os.Remove(assign) }()
		frame.convertTo, frame.assign = srgb, assign
	}
	targets := make([]fusedTarget, len(encs))
	chk := encodeCheck{frames: len(encs), outputs: paths}
	for i, enc := range encs {
		f := frame
		f.fits = es.fits(r, enc)
		targets[i] = fusedTarget{frame: f, enc: enc, path: paths[i]}
		if !r.zero() {
			targets[i].width, targets[i].height = exactSize(r, enc)
		}
		chk.bounds = append(chk.bounds, boundOf(enc, targets[i].source(encodeSource{})))
	}
	res, err = es.env.runEncode(ctx, fusedArgs(nil, encodeSource{}, targets), chk)
	return res, true, err
}
