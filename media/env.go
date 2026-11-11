// Package media builds and runs the media tools' command lines: bounded WebP
// encodes of still images and animations (a fallback ladder of dimension
// ceilings and frame caps, with the frame mapping each encode holds), and
// animated WebPs of a video's samples. It also offers media operations that
// turn one input file into upright, sized, sRGB images in an output directory.
//
// The package has two layers. The operations (Processor.Still,
// Processor.AnimationFrames, Processor.VideoFrames and their
// AnimationFramesAt, VideoFramesAt and VideoFrameRun variants, and the probes
// ProbeVideo and ProbeAnimation) run the tools themselves, for untrusted
// input: a Processor checks the file's size and type before any tool runs,
// pins the decoder to the type found, and runs the tools with a fixed
// environment under a generated ImageMagick policy that it has read back from
// the tool. The rest of the package, reached through an Env, decides
// arguments and interprets tool output without running programs: a caller
// supplies a Runner, a Stager and a Scratcher, so it controls how tools are
// resolved, how long they run and where scratch files live. The same inputs,
// arguments and tool binaries produce the same arguments, and so the same
// output bytes; callers pass policy values (quality, sizes, ceilings) in
// explicitly.
//
// # Decode once, encode many
//
// Env.Open reads a file's headers, applies the caps and selects its frames;
// Source.Encode then writes one WebP, PNG or JPEG per Encoding from that one
// decode. The decoder is chosen by the file's type and structure, never by
// its frame count:
//
//   - GIF, any number of frames: the Go compositor, in this process (as a
//     sandbox tool, in a Processor).
//   - WebP with the VP8X animation flag, any number of frames: webp-frames,
//     which Env.Run must be able to run.
//   - WebP without the animation flag: webp-frames still, then ImageMagick.
//   - Video: ffprobe, ffmpeg and ImageMagick.
//   - Any other image: ImageMagick, in the same run as the encode.
//
// The compositors write each selected frame as an uncompressed PAM file and
// wait for ImageMagick to consume it before writing more, so the directory
// holds about one batch. ImageMagick encodes the frames in batches of at most
// 16 million input pixels (about 256 MiB), resizing, flattening on white for
// JPEG and stripping metadata. The GIF compositor checks the block structure
// first (frame count, every frame inside the logical screen), composites as
// ImageMagick's -coalesce does and stops after the last selected frame. The
// animated WebP compositor runs libwebp's animation decoder, which composites
// one frame at a time; it is told the canvas and frame count of the header
// read and refuses a file that differs. An ICC profile of either is written
// beside the frames, assigned to them and converted to sRGB; ImageMagick's own
// decode of an animated WebP ignores the profile, as it ignores the EXIF
// orientation, so an oriented animation is output as stored, which is why a
// WebP with the animation flag always goes through webp-frames, even with one
// frame.
//
// # The gate
//
// Before any tool runs, a Processor operation checks that the input is a
// regular file of at most 25 MiB and takes its type from the file's leading
// bytes: GIF, WebP, HEIC and HEIF, MP4 and QuickTime, WebM and Matroska. The
// name and any claim of the caller are never consulted; a file that is not of
// a type the operation accepts gives an error wrapping ErrUnsupportedType,
// which names the type found. The decoder is then pinned to that type, so no
// tool picks a format from content. The accepted types, decoder pins and tools
// of each operation are not part of the API.
//
// # The caps
//
// Image headers are read without decoding pixels (-ping), here and in
// Env.Open, and an input is refused with an error wrapping ErrTooLarge, naming
// the cap and the values, when:
//
//   - a frame is wider or taller than 16384 pixels or has more than 52 million;
//   - an animation's canvas (the largest width by the largest height over its
//     frames) has more than 52 million pixels;
//   - an animation has more than 2000 frames, or more than 900 million pixels
//     to decode (all frames times the canvas);
//   - a video stream is wider or taller than 16384 or longer than six hours
//     (the stream's duration, else the container's), or a video container
//     holds more than 32 streams.
//
// AnimationFrames and AnimationFramesAt also refuse a call whose returned
// frames times the canvas exceed 64 million pixels; the message names the
// largest count that fits. The frame caps apply to a still as it is decoded,
// not as it is stored (see Env.Open); a refusal names both sizes. Where the
// numbers come from is recorded in docs/measurements.md.
//
// # The sandbox
//
// Every tool a Processor runs goes through a helper: the program's own
// executable, started again with a marker in its environment (see Init). The
// helper sets no_new_privs and these limits (soft and hard): CPU 210 s,
// address space 8 GiB, file size 5 GiB, open files 1024, and tasks the user's
// current count plus 512. It installs a seccomp filter, in pure Go, that makes
// socket and socketpair fail with EPERM, kills a process using another
// system-call ABI (32-bit or x32) and allows everything else. It becomes a
// child subreaper, runs the tool as its child, and after the tool exits kills
// and reaps every process left under it, orphans included. A tool killed by a
// signal is reported as an error. Cancelling the context closes the helper's
// lifeline, on which it kills the tool and its descendants; a killed helper
// takes the tool with it.
//
// The tool's environment and arguments are exactly those the Processor
// passes, and it inherits only descriptors 0, 1 and 2: the helper marks the
// rest close-on-exec (close_range, Linux 5.11) before starting it. Nothing
// about the helper reaches it, and the consumer gets no process-wide side
// effect. The GIF compositor and webp-frames are tools of this kind: the first
// is the program's own executable, started with a marker that Init
// recognises, and the second is started from the tool directory like magick;
// their CPU and peak memory count in Usage. NewProcessor refuses to return
// unless Init was called, and then starts the helper once in a probe mode
// that tries socket under the filter and reads the limits back; it requires
// the call denied and every limit set. The sandbox needs Linux 5.11 or later
// on amd64 or arm64; elsewhere, and on an older kernel, NewProcessor returns
// an error (the Env functions still work).
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
// derived from the coders each operation uses: it denies every delegate and
// every coder, then allows the used coders for reading, writing or both,
// denies @ indirect reads and sets ceilings on memory, map, disk, area, width,
// height, list length, time and threads. The memory and map ceilings are those
// of the MemoryProfile; the disk ceiling is 4 GiB whatever the profile.
//
// Every ffprobe and ffmpeg run opens its input with -protocol_whitelist file,
// so a nested input cannot reach the network, and the pinned -f demuxer; for
// mov also -enable_drefs 0, so a track's reference to an external data file is
// not followed. Each ffmpeg run also sets -threads (4, or 2 for a source over
// 4096x2160), -filter_threads of the same count and -max_alloc of 1 GiB;
// ffprobe sets -threads 4 and the same -max_alloc.
package media

import (
	"context"
	"os"
	"syscall"
	"time"
)

// Result is one completed tool run.
type Result struct {
	Stdout []byte
	// Stderr is the start and end of the tool's standard error (a Processor
	// keeps the first 512 bytes and the last 4 KiB, with a note of the bytes
	// dropped), or nil when the runner does not capture it. ImageMagick exits
	// 0 when it hits its time limit and reports it there and nowhere else, so
	// a runner that fills it lets every magick run fail with ErrTimeLimit, and
	// one that leaves it nil leaves only the size check of the encodes to
	// notice (an encode whose output is larger than its encoding allows, or
	// not the size it was asked for, fails and leaves no output).
	Stderr []byte
	// Usage is nil when the runner cannot measure it; it is never zero-filled.
	Usage *Usage
}

// Usage is what one tool run consumed. A Processor operation returns the CPU
// time summed over every tool run of the call and the largest peak resident
// set size of any run, also when the call fails.
type Usage struct {
	CPU    time.Duration // user plus system time
	MaxRSS int64         // peak resident set size, in bytes
}

// UsageOf reports the resources an exited process consumed. It returns nil
// when state is nil or carries no rusage.
func UsageOf(state *os.ProcessState) *Usage {
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

// Runner runs one tool to completion and returns its standard output and,
// when it can measure it, its resource usage.
//
// The package passes bare tool names ("magick", "ffmpeg"); resolving them to
// executables is the Runner's business. A non-nil error means the tool failed.
type Runner func(ctx context.Context, tool string, args ...string) (Result, error)

// Stager creates an empty scratch file whose name ends in ext (for example
// ".webp"); the tools choose the output format from the extension. cleanup
// removes the file and is safe to call once; the caller owns it. The tools
// write their output to the returned path.
type Stager func(ext string) (path string, cleanup func(), err error)

// Scratcher creates a new empty directory for intermediate files. cleanup
// removes it and what is in it and is safe to call once; the caller owns it.
type Scratcher func() (dir string, cleanup func(), err error)

// Env is what the package needs from its caller to run tools. Run is
// required by every method, and receives bare tool names ("magick",
// "ffmpeg"); it must also be able to run "webp-frames" for a WebP. Stage is
// required by the methods that produce an output file (EncodeRung,
// Source.Encode); Scratch is required by Open and EncodeRung, which keep
// decoded frames in it until the source is closed.
type Env struct {
	Run     Runner
	Stage   Stager
	Scratch Scratcher
	// Memory selects ImageMagick's memory limits; the zero value is
	// HighMemory. A Processor sets it from its own profile.
	Memory MemoryProfile

	// compose runs the frame compositor; it is set only by a Processor.
	compose composer
}

// composer composites the selected frames of a GIF or animated WebP in the
// sandbox and calls onFrame with each as it is written.
type composer func(ctx context.Context, req composeRequest, onFrame func(composedFrame) error) error
