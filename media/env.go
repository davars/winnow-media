// Package media turns an untrusted image, animation or video file into
// upright, sized, sRGB images in an output directory: stills, the frames of an
// animation or video, and WebP animations of them, in the formats and sizes a
// caller asks for.
//
// A Processor runs the tools themselves, under limits. Processor.Open checks
// the file's size and type before pixels are read, pins the decoder to the
// type found, and returns a Source; Source.Info describes the file, and
// Source.Encode writes the frames and region a Select names, in the formats and
// sizes of its Encodings. The tools run with a fixed environment under a
// generated ImageMagick policy that the Processor has read back from the tool.
// The same input, selection, encodings and tool binaries produce the same
// output bytes.
//
// # Sources
//
// A Source is one open input. Open reads only headers (a still's or
// animation's) or runs one ffprobe (a video); no pixel is decoded until an
// Encode. A Source holds no process between calls, only what the next call
// needs, in a private directory under the Processor's own, and the input file
// open until Close; each Encode has its own Usage. Open opens the input once.
// The size check, the type sniff and Go's reads of the file use that
// descriptor everywhere. On Linux the tools read it too, as /dev/fd/3, so
// replacing or relinking the path after Open changes nothing that is read;
// elsewhere a tool reads through a link to the path, and only the per-Encode
// check guards it. The Source records the file's device, inode, size and
// modification and status-change times when it is opened and checks them on
// the descriptor before every Encode, which refuses a file that changed; a
// rename over the path, an unlink or a new link changes the file's
// status-change time, so it is refused too. A writer of the same file between
// that check and a tool's read is not excluded.
//
//   - A still is decoded and encoded in one ImageMagick run per Encode (a JPEG
//     at a reduced size when its targets are small, a still WebP at a reduced
//     size too). Nothing is kept between calls, unless the Source was opened
//     with OpenOptions.Reuse, which keeps a full-size decode in the private
//     directory for later Encodes. Where the header read does not say how the
//     picture is oriented, the first Encode learns it, and Info and
//     Image.Region then report the upright size.
//   - An animation is composited by the compositor, once for the frames an
//     Encode lacks, up to the largest of them. The Source keeps the composited
//     frames of its most recent call only, so a call that asks for a subset of
//     the last one's frames composites nothing.
//   - A video is probed once, at Open. Each Encode samples it afresh: one time
//     is read by a seek, several by one decode when the stream has at most 1e9
//     decoded pixels and by one seek each otherwise. No sample is kept, so a
//     Region decodes the cut part afresh.
//
// Calls on one Source are serialised, and Sources run in parallel.
// Processor.Close closes every Source still open.
//
// # Decode once, encode many
//
// A Source decodes a file once for all the Encodings of an Encode call. The
// decoder is chosen by the file's type and structure, never by its frame
// count:
//
//   - GIF, any number of frames: the Go compositor, which runs as a sandbox
//     tool.
//   - WebP with the VP8X animation flag, any number of frames: webp-frames,
//     which is its compositor.
//   - WebP without the animation flag: webp-frames still, then ImageMagick.
//   - Video: ffprobe, ffmpeg and ImageMagick.
//   - Any other image: ImageMagick, in the same run as the encode.
//
// Each frame of an animation is assigned the ICC profile it carries (a GIF may
// carry several) and converted to sRGB. ImageMagick's own decode of an animated
// WebP ignores the profile, as it ignores the EXIF orientation, so an oriented
// animation would be output as stored; this is why a WebP with the animation
// flag always goes through webp-frames, even with one frame.
//
// # The gate
//
// Before pixels are read, Open checks that the input is a regular file of at
// most Config.Caps.MaxInputBytes and takes its type from its bytes alone. A
// GIF, WebP, ISO base media file (HEIC, HEIF, AVIF, MP4, QuickTime) or EBML
// file (WebM, Matroska) is typed by its leading bytes before any tool runs, as
// is every type that has a signature. A file opening an AVI, ASF, FLV, MPEG
// program, elementary or transport stream, a QuickTime file without ftyp, or an
// ISO base media file of a brand that is no still image's is named by ffprobe
// first, in the sandbox, under a format whitelist of the demuxers of the video
// types in Config.Types that have none of their own signature. If that probe
// refuses it, the header read below follows. Other content is named by the one
// header read that the decode needs, under the generated policy, and the coder
// it reports is looked up in the table of Types. No tool is given the file's name: on Linux
// a tool is given /dev/fd/3, the descriptor Open holds, and elsewhere an
// extensionless link in the Source's private directory. No tool lets the
// content choose a coder or a demuxer outside Config.Types; only the
// detection probe lets it choose among the whitelisted demuxers. The decoder
// is then pinned to the type found, and its demuxer is the only format the
// decode may open.
// OpenOptions.Type replaces all of this with a type the caller names, which
// must be in Config.Types.
//
// The type must be in Config.Types, else Open gives an error wrapping
// ErrUnsupportedType, which names the type found. SVG, MVG, MSL, PostScript, EPS, PDF and text are never in Types:
// ImageMagick reads them through delegates or with external-reference or
// scripting semantics. Some types in Types are not identified from content
// and are decoded only when OpenOptions.Type names them (see Types), and a
// TIFF whose first image is a reduced-resolution preview, as in a camera raw
// file, is refused unless its type is named.
//
// # The caps
//
// Image headers are read without decoding pixels (-ping), by Open, and an input
// is refused with an error wrapping ErrTooLarge, naming the cap and the values,
// when it is over a limit of Config.Caps, whose doc comment states each limit
// and its ceiling. The frame caps apply to a still as it is decoded, not as it
// is stored; a refusal names both sizes. The measurements behind the ceilings
// and the values a deployment chose are recorded in docs/measurements.md.
//
// # The sandbox
//
// Importing this package installs a package init that runs the importing
// binary as the library's sandbox helper or frame compositor when a
// WINNOW_MEDIA_* marker is set in its environment, and does nothing
// otherwise; a program, or a test binary, needs no setup call.
//
// Every tool a Processor runs goes through a helper: the program's own
// executable, started again with such a marker. The helper sets no_new_privs
// and these limits (soft and hard): CPU time, address space and file size
// derived from the Processor's Resources (at the defaults 300 s, 8 GiB and
// 5 GiB: the time limit, memory plus map plus 5 GiB, and the disk limit plus a
// quarter), open files 1024, and tasks the user's current count plus 512. It
// installs a seccomp filter, in pure Go, that makes socket and socketpair fail
// with EPERM, kills a process using another system-call ABI (32-bit or x32)
// and allows everything else. It becomes a child subreaper, runs the tool as
// its child, and after the tool exits kills and reaps every process left under
// it, orphans included. A tool killed by a signal is reported as an error.
// Cancelling the context closes the helper's lifeline, on which it kills the
// tool and its descendants; a killed helper takes the tool with it.
//
// The tool's environment and arguments are exactly those the Processor
// passes, and it inherits descriptors 0, 1 and 2 and, when the run reads an
// input, that input as descriptor 3; the helper marks every other descriptor
// close-on-exec (close_range, Linux 5.11) before starting it. Nothing
// about the helper reaches it, and the consumer gets no process-wide side
// effect. The GIF compositor and webp-frames are tools of this kind: the first
// is the program's own executable, started with a marker its package init
// recognises, and the second is started from the tool directory like magick;
// their CPU and peak memory count in Usage. NewProcessor starts the helper
// once in a probe mode that tries socket under the filter and reads the limits
// back; it requires the call denied and every limit set. The sandbox needs
// Linux 5.11 or later on amd64 or arm64; elsewhere, and on an older kernel,
// NewProcessor returns an error unless Config.Unsandboxed is set. That runs each
// tool directly, in a process group that is killed with the call and when the
// tool exits, with no seccomp filter, rlimits or subreaper; the gate, caps,
// environment, policy and the Resources values that reach the policy and the
// tool flags still apply.
//
// Landlock is not used, so file confinement rests on the container. A
// deployment for hostile input must supply it: its own Deployment with no
// shared network namespace, a non-root user, a read-only root filesystem, all
// capabilities dropped, allowPrivilegeEscalation false, no secrets in the
// container, a memory limit, scratch on a size-limited volume, and a network
// policy denying egress.
//
// # The policy
//
// Tools run in the Processor's directory with an environment of exactly
// MAGICK_CONFIGURE_PATH, MAGICK_TEMPORARY_PATH, TMPDIR and LC_ALL=C, the two
// temporary paths naming a directory made for that run and removed when the
// call returns (a killed process, or a helper killed after its wait delay, can
// leave it). There is no PATH, HOME or XDG_CONFIG_HOME, so no configuration
// file of the caller's can widen the policy. The generated policy.xml is
// derived from Config.Types: it denies every delegate and every coder, then
// allows the coders of those types and those the library uses itself
// (the profile and intermediate formats, the output formats) for reading,
// writing or both, so that content can name no other coder. It also denies
// @ indirect reads and sets ceilings on memory, map, disk, area, width,
// height, list length, time and threads. The memory, map, disk, time and thread
// ceilings are those of the Processor's Resources.
//
// Every ffprobe and ffmpeg run opens its input with -protocol_whitelist file,
// so a nested input cannot reach the network. Every run but the detection probe
// of the gate also pins the -f demuxer to the type found (for mov with
// -enable_drefs 0, so a track's reference to an external data file is not
// followed); the probe is limited to the whitelisted demuxers, which exclude
// playlist and list formats that would open other files. Each run also bounds
// ffmpeg's allocation, and its threads (Resources.Threads, at most two for a
// source above 4096x2160).
package media

import (
	"context"
	"os"
	"syscall"
	"time"
)

// runResult is one completed tool run.
type runResult struct {
	Stdout []byte
	// Stderr is the start and end of the tool's standard error (the first 512
	// bytes and the last 4 KiB, with a note of the bytes dropped). ImageMagick
	// exits 0 when it hits its time limit and reports it there and nowhere
	// else, so every magick run is checked against it for ErrTimeLimit.
	Stderr []byte
	// Usage is nil when the runner cannot measure it; it is never zero-filled.
	Usage *Usage
}

// Usage is what one tool run consumed. A Processor operation returns the CPU
// time summed over every tool run of the call and the largest peak resident
// set size of any run, also when the call fails. Both cover the tool and its
// descendants, never the calling program.
type Usage struct {
	CPU    time.Duration // user plus system time
	MaxRSS int64         // largest resident set of the tool or any descendant, in bytes
}

// usageOf reports the resources an exited process consumed. It returns nil
// when state is nil or carries no rusage.
func usageOf(state *os.ProcessState) *Usage {
	if state == nil {
		return nil
	}
	ru, ok := state.SysUsage().(*syscall.Rusage)
	if !ok || ru == nil {
		return nil
	}
	return &Usage{
		CPU:    state.UserTime() + state.SystemTime(),
		MaxRSS: maxRSSBytes(ru),
	}
}

// toolEnv is what the decoding and encoding code needs to run tools: run
// receives bare tool names ("magick", "ffmpeg", "webp-frames") and compose runs
// the frame compositor. Both give the tool, as its input descriptor, the file
// the Processor opened the input as.
type toolEnv struct {
	run     func(ctx context.Context, name string, args ...string) (runResult, error)
	compose composer

	// resources are the limits the runs are held to and caps those of the
	// gate.
	resources Resources
	caps      caps
}

// threads is the thread count of the ffmpeg and ffprobe runs.
func (e toolEnv) threads() int { return e.resources.Threads }

// consumeTimeout is how long the compositor waits for a written frame to be
// consumed.
func (e toolEnv) consumeTimeout() time.Duration {
	if composeTimeout != 0 {
		return composeTimeout
	}
	return e.resources.TimeLimit
}

// composer composites the selected frames of a GIF or animated WebP in the
// sandbox and calls onFrame with each as it is written.
type composer func(ctx context.Context, req composeRequest, onFrame func(composedFrame) error) error
