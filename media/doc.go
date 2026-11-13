// Package media turns an untrusted image, animation or video file into
// upright, sized, sRGB images: stills, the frames of an animation or video,
// and animated WebPs of them, in the formats and sizes a caller asks for. It
// runs ImageMagick, ffmpeg and a WebP compositor as external programs, under
// limits and, on Linux, in a sandbox.
//
// A [Processor] holds the configuration and runs the tools. [Processor.Open]
// admits one file and returns a [Source] without decoding a pixel.
// [Source.Info] describes the file, and [Source.Encode] writes the frames and
// region a [Select] names into an empty directory, once for each [Encoding]
// it is given. A Source can be encoded from as often as the caller likes
// until it is closed. [Types] lists the types that are read; the outputs are
// JPEG, WebP and PNG. The examples show each step.
//
// # Kinds
//
// [Info.Kind] says how a Source's frames are read, and is the one field to
// switch on: Info.Images, Info.Animation and Info.Video are each filled in for
// their own kind only.
//
//   - A [Still] is an image file. It may hold several images (TIFF pages, icon
//     sizes), which are selected by index.
//   - An [Animation] is a GIF or an animated WebP. Its frames are counted, and
//     selected by index or by time. Every GIF is an Animation, a GIF of one
//     frame included, and so is every WebP marked as animated; treating a
//     one-frame animation as a still is the caller's choice.
//   - A [Video] is sampled by time.
//
// Animations and videos are separate kinds because an animation's frames are
// built on each other, so reading one decodes those before it, while a video
// is sought into by time.
//
// A caller need not know the kind before it calls. [OpenOptions.KeepDecode]
// may be set for any file, and a [FrameSelection] that does not apply to the
// Source's kind is refused with [ErrInvalidRequest] before any tool runs.
//
// # What a Source keeps
//
// One Encode decodes its frames once for all of its Encodings. Between calls:
//
//   - A still keeps nothing, so every Encode decodes it again, unless the
//     Source was opened with OpenOptions.KeepDecode.
//   - An animation keeps the composited frames of its most recent call, so a
//     call for some of the same frames does not decode those again.
//   - A video keeps nothing: every Encode samples it afresh.
//
// The exception is an animation whose selected frames exceed
// [Resources.Disk]: it is composited once for each Encoding, a batch of
// frames at a time, and keeps nothing.
//
// A Source holds no process between calls. Until Close it holds the input
// file, open, and a scratch directory under [Config.Dir].
//
// # Admission
//
// Open admits a file before any pixel is decoded:
//
//   - It must be a regular file (else ErrInvalidRequest) of at most
//     Caps.MaxInputBytes (else [ErrTooLarge]).
//   - Its type is taken from its bytes, never from its name, and must be one
//     of [Config.Types]; otherwise the error wraps [ErrUnsupportedType].
//     [OpenOptions.Type] names the type instead, for the few that bytes do
//     not identify (see [Types]).
//   - The sizes its headers declare must be within [Config.Caps] and the
//     fixed limits listed at [Caps]; otherwise the error wraps ErrTooLarge.
//     Two sizes are judged at Encode instead: see Caps.MaxFramePixels.
//
// The decoder is then pinned to that type, so the content can select no other.
//
// Open opens the file once, and the library reads it only through that
// descriptor. On Linux the tools read the same descriptor, so a file later put
// at the same path is never read. Every Encode first checks that the file is
// still the one opened and has not been modified, and returns
// [ErrInputChanged] if it has. A file rewritten in place between that check
// and a tool's read is not detected.
//
// # Lifecycle and concurrency
//
// A Processor and its Sources are safe for concurrent use. Calls on one
// Source run one at a time, and Sources run in parallel; the Processor does
// not limit how many, so the caller does (see Sizing).
//
// The library sets no deadline on a call: pass a context that has one.
// Cancelling a call's context kills its tools and returns an error wrapping
// the context's; an Encode waiting its turn on a Source returns as soon as
// its context ends. An Encode that fails or is cancelled leaves its output
// directory as it found it and the Source usable, though the Source may have
// dropped what it kept between calls.
//
// Close waits for running calls and does not interrupt them: to stop sooner,
// cancel their contexts first. [Processor.Close] closes every Source still
// open. Open and Encode after Close return [ErrClosed].
//
// # Errors
//
// Callers branch on the sentinels below with errors.Is, on [*ToolError] with
// errors.As, and on the context's own errors. Error text is not part of the
// contract; it carries neither the input's path nor a tool's standard error.
//
//	Error               Meaning                          Retry
//	ErrUnsupportedType  the file's type is not accepted  no, unless the type is
//	                                                     named in OpenOptions.Type
//	ErrTooLarge         the file, or the work asked of   not as called: ask for
//	                    it, is over a limit              less, or raise the limits
//	ErrInvalidRequest   the call is wrong, not the       no: correct the call
//	                    file's content
//	ErrInputChanged     the file is not the one opened   after opening it again
//	ErrClosed           Source or Processor is closed    no: open a new one
//	ErrTimeLimit        a tool reached a time limit      maybe, with larger limits
//	                                                     or on a less loaded host
//	context errors      the caller's context ended       the caller's choice
//	*ToolError          a tool failed, most often on a   unknown
//	                    file its decoder cannot read
//
// An error that is none of these, such as the operating system's for a file
// that cannot be opened, says nothing about a retry. Each sentinel's own
// comment is its full contract. A ToolError's standard error is for
// operators' logs only (see [ToolError.Stderr]).
//
// # Hostile input
//
// With the sandbox, which needs Linux, the package guarantees this of any
// input file:
//
//   - The library chooses the decoder, from the file's bytes and
//     Config.Types. ImageMagick runs under a generated policy that allows only
//     the coders of those types and the few the library uses itself, and
//     denies every delegate and indirect read; ffmpeg may open only the
//     input, with the demuxer of its type, and follows no reference to
//     another file or to the network. [NewProcessor] reads the policy back
//     and fails unless it is the only one in effect.
//   - A tool starts with a fixed environment, the input, and its standard
//     streams, and nothing else of the calling process.
//   - A tool cannot create a socket (socket, socketpair and io_uring are
//     denied) or gain privileges, and is limited in CPU time, address space,
//     file size and open files. NewProcessor fails unless it sees these
//     applied.
//   - When a tool exits or its call is cancelled, every process it started
//     is killed.
//
// It does not guarantee that the decoders are free of bugs, and it does not
// confine what an exploited tool can reach. Such a tool runs as the calling
// user: it can read and write that user's files and signal that user's other
// processes. Its resident memory is not limited, only its address space. The
// limit on the processes it may start counts all of the user's tasks, and is
// not enforced for root. A deployment for hostile input supplies the rest: a
// container or equivalent with its own unprivileged user, a read-only root
// filesystem, a memory limit, a size limit on Config.Dir, no network and no
// secrets. It also sets Config.Types and Config.Caps; the zero [Config]
// accepts every type and the largest sizes the library can handle.
//
// The sandbox needs Linux 5.11 or later on amd64 or arm64, a mounted /proc,
// and a host that lets an unprivileged process install a seccomp filter, as
// container runtimes do by default. It uses no user namespace and needs no
// privilege. Elsewhere NewProcessor fails unless [Config.Unsandboxed] is set.
// Then only admission, the caps, the policy, the fixed environment and the
// killing of a tool's process group hold: no system call is denied, and no
// limit is set but those ImageMagick and ffmpeg apply to themselves, so
// Resources.CPUTime has no effect. On macOS, where that is the only mode, the
// tools also read the input by its path, so a file put at that path while an
// Encode runs can be read. The package builds for Linux and macOS only.
//
// # Helper processes
//
// The library starts the program's own executable again: as the helper each
// tool runs under in the sandbox, and, in either mode, as the GIF compositor.
// This package's init recognises both by a WINNOW_MEDIA_* marker in the
// environment and never returns. No setup call is needed, in a program or in
// a test binary. Two consequences: the executable must still be at its path
// while the program runs, and package initialization elsewhere in the
// program, which Go may order before this package's, runs in every such
// process, with an otherwise empty environment and before the sandbox
// applies. Keep it free of side effects.
//
// # Determinism
//
// The same input, Config, OpenOptions, Select, Encodings, library version and
// tool binaries give the same output bytes on one architecture. Nothing is
// promised across library versions, tool builds or architectures.
//
// # Sizing
//
// [Resources] bounds one tool run. A call runs one tool at a time, except an
// animation whose selected frames exceed Resources.Disk: its compositor
// waits, holding its memory, while ImageMagick encodes each batch. A large
// still takes seconds, and 64 samples of a 4K video take minutes; the
// [measurements] record times and peaks. For the most calls the caller lets
// run at once, plan for:
//
//   - Memory: about that many times the peak of one run. Resources.Memory and
//     Map bound ImageMagick's pixel cache, the largest consumer, but not a
//     tool's other memory. Map is backed by files under Config.Dir, so put
//     Dir on a disk, not in memory.
//   - Disk under Config.Dir: that many times Resources.Disk for ImageMagick's
//     cache, and as much again for each call on a video, whose sampled frames
//     wait there uncompressed (a call that asks for more than Resources.Disk
//     of them is refused with ErrTooLarge). Add what open Sources keep
//     between calls: up to half of Resources.Disk for each KeepDecode Source,
//     and up to Resources.Disk for each animation. These bound the large
//     items, not a call's scratch as a whole, so give Dir a size limit of its
//     own.
//   - Time: Resources.TimeLimit is wall-clock time ImageMagick may spend on
//     one image, and Resources.CPUTime the CPU time of one tool run over all
//     its threads. Neither bounds a call, which may run many tools (see
//     Lifecycle and concurrency).
//
// # Compatibility
//
// From v1 the module follows Go's compatibility rules for its exported
// identifiers. Also covered: what a zero value means, which inputs and
// requests are refused and with which sentinel, and the names and order of
// the files Encode writes.
//
// Limits only loosen: the ceilings and fixed limits at Caps, the ceilings a
// zero Encoding.MaxEdge and MaxArea stand for, and [MaxVideoSamples] may rise
// in a later v1 release but do not fall, so a request one v1 release accepts
// is not refused by a later one for being over them.
//
// Not covered: output bytes (see Determinism), error text, the versions of
// the tools, the defaults of Resources, and the set Types returns, which may
// grow. A deployment sets the fields it relies on; a nil Config.Types follows
// Types, so a deployment for hostile input names its types.
//
// Later versions may add fields to the structs, values to [Kind] and
// [Format], and sentinels, so that an error which wraps none now may wrap one
// later: write struct literals with field names, and give a switch over a
// Kind or Format a default case.
//
// # Testing
//
// Processor and Source are concrete types. Code that uses them is easiest to
// test behind a small interface of its own, with a fake in unit tests. Tests
// that run the real tools need the tools on PATH, as in the development shell
// of the module's Nix flake, and a host the sandbox supports: on macOS they
// set Config.Unsandboxed. Give each a Config.Dir of its own, such as
// t.TempDir(), and skip where NewProcessor fails for want of either.
//
// [measurements]: https://github.com/davars/winnow-media/blob/main/docs/measurements.md
package media
