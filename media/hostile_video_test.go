package media

import (
	"context"
	"encoding/binary"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// aliasEntry returns a QuickTime data reference entry of type alis, the kind
// ffmpeg follows, that names the file called name beside the one that holds
// it.
func aliasEntry(name string) []byte {
	path := "/" + name
	if len(path)%2 == 1 {
		path += "\x00"
	}
	var body []byte
	body = append(body, 0, 0, 0, 0)               // version and flags: not self-contained
	body = append(body, make([]byte, 10)...)      // record header
	body = append(body, make([]byte, 28)...)      // volume name length and name
	body = append(body, make([]byte, 12)...)      // volume attributes
	body = append(body, 0)                        // file name length
	body = append(body, make([]byte, 63)...)      // file name
	body = append(body, make([]byte, 16)...)      // file attributes
	body = binary.BigEndian.AppendUint16(body, 1) // levels up from the alias
	body = binary.BigEndian.AppendUint16(body, 1) // levels down to the target
	body = append(body, make([]byte, 16)...)
	body = binary.BigEndian.AppendUint16(body, 2) // absolute path
	body = binary.BigEndian.AppendUint16(body, uint16(len(path)))
	body = append(body, path...)
	body = append(body, 0xFF, 0xFF, 0, 0) // end of tags
	box := binary.BigEndian.AppendUint32(nil, uint32(8+len(body)))
	return append(append(box, "alis"...), body...)
}

// externalDrefMP4 returns mp4 with every data reference entry replaced by an
// alias to the file called sibling in the same directory, and with the media
// data zeroed, so the frames can only come from that file, which must hold
// mp4's original bytes: the sample offsets are unchanged.
func externalDrefMP4(t *testing.T, mp4 []byte, sibling string) []byte {
	t.Helper()
	var rewrite func(b []byte) []byte
	rewrite = func(b []byte) []byte {
		var out []byte
		for len(b) >= 8 {
			size := int(binary.BigEndian.Uint32(b))
			if size < 8 || size > len(b) {
				t.Fatalf("unsupported box size %d", size)
			}
			typ, body := string(b[4:8]), b[8:size]
			switch typ {
			case "moov", "trak", "mdia", "minf", "dinf":
				body = rewrite(body)
			case "dref":
				body = append(append([]byte{}, body[:8]...), aliasEntry(sibling)...)
			case "mdat":
				body = make([]byte, len(body))
			}
			out = binary.BigEndian.AppendUint32(out, uint32(8+len(body)))
			out = append(append(out, typ...), body...)
			b = b[size:]
		}
		return out
	}
	return rewrite(mp4)
}

// frameHashes decodes the first frame of path through the processor's runner
// with the given input arguments and returns ffmpeg's frame hash line.
func frameHashes(p *Processor, input []string) (string, error) {
	args := append([]string{"-nostdin", "-v", "error"}, input...)
	res, err := p.run(context.Background(), "ffmpeg", append(args, "-frames:v", "1", "-f", "framemd5", "-")...)
	return string(res.Stdout), err
}

func TestMOVExternalDataReference(t *testing.T) {
	env := requireFFmpeg(t)
	p := requireProcessor(t)
	data := sdrClip(t, env, "160x120", 10, 2)
	raw, err := os.ReadFile(data)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "referenced.mp4"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	hostile := filepath.Join(dir, "input")
	if err := os.WriteFile(hostile, externalDrefMP4(t, raw, "referenced.mp4"), 0o600); err != nil {
		t.Fatal(err)
	}

	want, err := frameHashes(p, []string{"-f", "mov", "-i", data})
	if err != nil || !strings.Contains(want, ",") {
		t.Fatalf("hashing the referenced file: %q, %v", want, err)
	}

	// Positive control: with references enabled the demuxer follows the
	// entry to the other file and decodes its frames.
	got, err := frameHashes(p, []string{"-protocol_whitelist", "file", "-enable_drefs", "1", "-f", "mov", "-i", hostile})
	if err != nil || got != want {
		t.Fatalf("control: references enabled gave %q, %v; want the referenced file's frames %q", got, err, want)
	}

	// Under the package's arguments the file is not read. The demuxer's
	// default already disables references; TestInputArgs pins the argument.
	got, err = frameHashes(p, inputArgs(decoder{"ffmpeg", "mov"}, hostile))
	if err == nil && got == want {
		t.Errorf("the demuxer followed the external reference: %q", got)
	}
	t.Logf("package arguments: %q, %v", got, err)
}

func TestDemuxerOptionsAreKnownToTheirDemuxer(t *testing.T) {
	env := requireFFmpeg(t)
	mov := sdrClip(t, env, "160x120", 10, 1)
	mkv := writeClip(t, env, ".mkv", append(testsrc("160x120", 10, 1), "-c:v", "libx264", "-preset", "ultrafast")...)
	p := requireProcessor(t)
	for name, path := range map[string]string{"mov": mov, "matroska": mkv} {
		if _, err := frameHashes(p, inputArgs(decoder{"ffmpeg", name}, path)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestInputArgs(t *testing.T) {
	for _, c := range []struct {
		demuxer string
		want    string
	}{
		{"mov", "-protocol_whitelist file -enable_drefs 0 -f mov -i in"},
		{"matroska", "-protocol_whitelist file -f matroska -i in"},
	} {
		if got := strings.Join(inputArgs(decoder{"ffmpeg", c.demuxer}, "in"), " "); got != c.want {
			t.Errorf("%s: %q, want %q", c.demuxer, got, c.want)
		}
	}
}

// hlsPlaylist returns a playlist with one segment, the URL segment.
func hlsPlaylist(segment string) []byte {
	return []byte("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n" +
		"#EXTINF:2.0,\n" + segment + "\n#EXT-X-ENDLIST\n")
}

func TestHLSPlaylistClaimedAsMP4(t *testing.T) {
	env := requireFFmpeg(t)
	p := requireProcessor(t)
	segment := filepath.Join(t.TempDir(), "segment.mp4")
	copyFile(t, sdrClip(t, env, "160x120", 10, 2), segment)
	playlist := writeFile(t, "x.mp4", hlsPlaylist("file://"+segment))

	t.Run("refused before any tool runs", func(t *testing.T) {
		rec := record(p)
		_, _, err := p.VideoFrames(context.Background(), playlist, Encoding{}, 0, t.TempDir())
		if !errors.Is(err, ErrUnsupportedType) {
			t.Fatalf("err = %v, want ErrUnsupportedType", err)
		}
		if n := rec.count(); n != 0 {
			t.Errorf("%d tool runs", n)
		}
	})

	probe := func(demuxer string) (string, error) {
		res, err := p.run(context.Background(), "ffprobe",
			append([]string{"-v", "error", "-show_entries", "format=duration"}, inputArgs(decoder{"ffprobe", demuxer}, playlist)...)...)
		return string(res.Stdout), err
	}

	// The whitelist admits file, so the hls demuxer reads a file:// segment;
	// only the pinned demuxer keeps a playlist from being parsed.
	t.Run("hls demuxer reads a file segment", func(t *testing.T) {
		out, err := probe("hls")
		if err != nil || !strings.Contains(out, "duration=2.0") {
			t.Fatalf("control: -f hls gave %q, %v", out, err)
		}
	})
	for _, demuxer := range []string{"mov", "matroska"} {
		t.Run("forced "+demuxer+" does not parse a playlist", func(t *testing.T) {
			if out, err := probe(demuxer); err == nil {
				t.Errorf("-f %s opened the playlist: %q", demuxer, out)
			}
		})
	}
}

// ffmpeg's defaults already refuse these segments; TestInputArgs pins the
// whitelist argument itself.
func TestInputWhitelistBlocksNetworkSegments(t *testing.T) {
	requireFFmpeg(t)
	// Unsandboxed, so that the whitelist is the only thing between the
	// demuxer and the network.
	env := testEnv(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	playlist := writeFile(t, "x.m3u8", hlsPlaylist(srv.URL+"/segment.ts"))
	run := func(args ...string) {
		_, _ = env.Run(context.Background(), "ffprobe", append(append([]string{"-v", "quiet"}, args...), "-f", "hls", "-i", playlist)...)
	}

	// Control: with the network protocols allowed the demuxer fetches the
	// segment.
	run("-protocol_whitelist", "file,http,tcp,crypto")
	if hits.Load() == 0 {
		t.Fatal("control: the hls demuxer never requested the segment")
	}
	hits.Store(0)

	args := inputArgs(decoder{"ffprobe", "hls"}, playlist)
	_, _ = env.Run(context.Background(), "ffprobe", append([]string{"-v", "quiet"}, args...)...)
	if n := hits.Load(); n != 0 {
		t.Errorf("the segment was requested %d times despite the whitelist", n)
	}
}

// The sandbox refuses the socket itself, so a tool whose own whitelist
// allowed the network still cannot reach it.
func TestSandboxBlocksNetworkSegmentsWhenTheWhitelistAllowsThem(t *testing.T) {
	requireFFmpeg(t)
	p := requireProcessor(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	playlist := writeFile(t, "x.m3u8", hlsPlaylist(srv.URL+"/segment.ts"))
	res, err := p.run(context.Background(), "ffprobe", "-v", "error", "-protocol_whitelist", "file,http,tcp,crypto", "-f", "hls", "-i", playlist)
	if n := hits.Load(); n != 0 {
		t.Errorf("the segment was requested %d times through the sandbox", n)
	}
	if err == nil || !strings.Contains(err.Error(), "not permitted") {
		t.Errorf("err = %v, stdout %q; want a refused socket", err, res.Stdout)
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
