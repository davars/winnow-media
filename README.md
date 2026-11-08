# winnow-media

A Go library and Nix flake for running external media tools (ImageMagick,
FFmpeg, Poppler, pandoc and friends) from a pinned, reproducible tool
closure. Licensed under AGPL-3.0.

This repository currently contains the tool closure, the Go package that
finds it, and the `media` package that builds the tools' command lines and
runs them on untrusted input inside a sandbox (resource limits, a
system-call filter and cleanup of leftover processes).

## The `tool` package

`github.com/davars/winnow-media/tool` resolves tool names to executable
paths.

- `tool.Resolve(name)` returns `<toolDir>/<name>` when a tool directory was
  injected at link time. It fails if that file is missing or not executable
  and never falls back to `PATH` in that case. With no injected directory
  (development builds) it returns `exec.LookPath(name)`.
- `tool.FlakeRevision()` returns the flake revision injected at link time,
  or `""` in development builds.

## The `media` package

`github.com/davars/winnow-media/media` builds ImageMagick and FFmpeg command
lines and interprets their output. Apart from the operations below, it does
not run programs itself: the caller supplies an `Env`, so it decides how tools
are resolved, limited and cancelled, and where scratch files live.

- `Env{Run, Stage, Scratch}`: `Run` is a `Runner`, `func(ctx, tool string, args ...string)
  (Result, error)`, and receives bare tool names (`"magick"`, `"ffmpeg"`).
  A `Result` holds the tool's `Stdout`, its `Stderr` (the start and end, which a `Processor` keeps as the first 512 bytes and the last 4 KiB with a note of those dropped; nil when the
  runner does not capture it) and a `*Usage` (CPU time, peak RSS in
  bytes) that is nil when the runner cannot measure it; `UsageOf` builds one
  from an exited `*os.ProcessState`. ImageMagick exits 0 when it hits its time
  limit and prints `time limit exceeded` on stderr only, so a runner that
  fills `Stderr` lets every magick run fail with `ErrTimeLimit`
  (a `Processor` always does); whatever the runner, an encode whose output is
  larger than its encoding allows, or does not have the size it was asked for,
  fails and leaves no output.
  `Stage` is a `Stager` that creates an empty scratch file whose name ends in
  an extension (`".webp"`) and returns its path and a cleanup function.
  `Scratch` is a `Scratcher` that creates a new empty directory for decoded
  frames and returns it with a cleanup function; `Open` and `EncodeRung`
  require it.
- Decode once, encode many: `Env.Open(ctx, path, Hints)` reads a file's
  headers, applies the caps and selects its frames; `Source.Encode(ctx,
  maxFrames, targets...)` writes one WebP, PNG or JPEG per `Encoding` through
  `Env.Stage` and returns an `Artifact` for each (path, cleanup, measured size
  and the frame mapping that artifact holds); `Source.EncodeFrames` writes one
  file per frame into a directory; `Source.Frames` and
  `Source.Close` complete it. `Hints` carry the coder-fallback key for an
  extensionless TGA, ICO, CUR or G3, an orientation (EXIF 1-8) that replaces
  the file's own, the number of frames to select from an animation (0 for
  every frame) and `Still`, which selects the first image of an image file
  and excludes `Frames`.
  The decoder is chosen by the file's type and structure, never by its frame
  count:

  | Source | Decoder |
  |---|---|
  | GIF, any number of frames | the Go compositor, in this process |
  | WebP with the VP8X animation flag, any number of frames | `webp-frames`, which `Env.Run` must be able to run |
  | WebP without the animation flag | `webp-frames still`, then ImageMagick |
  | any other image | ImageMagick, in the same run as the encode (`[0]`) |

  A still other than a WebP is never written out: one ImageMagick run decodes
  it and writes every target, each from a clone of the decoded image, so a
  source costs one decode however many targets it has. The frames of a GIF or
  animated WebP are composited onto their canvas into `Env.Scratch` by `Open`,
  where every
  `Encode` reads them (about four bytes per pixel of the canvas for each
  selected frame); the GIF compositor stops after the last selected frame.
  `maxFrames` of `Encode` selects, evenly spaced from the first, that many of
  the frames `Open` selected (all of them for 0); when each rung's
  count divides the count `Open` selected (a halving ladder from 12, 16, 20,
  24 or 32 does; one from 14 or 18 does not), that is the source frames the
  rung selects of the whole file.
  Each `Encode` returns its own mapping, read back from the scene delays of
  the file it wrote: the WebP writer merges a frame into the scene before it
  when the two are equal or nearly so at that size. A file of several images
  that is neither a GIF nor a WebP is refused with `ErrUnsupportedType`
  unless `Hints.Still` is set.
  An MP4, QuickTime, WebM or Matroska file is a video, as is any container
  ffmpeg detects among `detectedFormats`. A file that sniffs as a video, or
  that leads a QuickTime atom, AVI, ASF, FLV, an MPEG program or video stream
  or a transport stream, is probed as one and ImageMagick does not read it
  unless the probe refuses it. Any other file that sniffs as no type is read
  as an image first and probed as a video only if ImageMagick cannot read it;
  a file that is neither is refused with `ErrUnsupportedType`. The Processor
  accepts only the sniffed types. `Open` probes a video
  (refusing one over the caps with `ErrTooLarge`) and chooses `Hints.Frames`
  sample times, 15 when 0 and at most 64, at the centres of equal intervals of
  the duration. The first `Encode` reads the samples, once per seek or by one
  select decode as `Processor.VideoFrames` does, at the size its targets need,
  and a later `Encode` that needs larger frames reads them again. A video's
  `Frame`s have `SourceFrame` 0 and the sample time as `SourceTimeMS`, and
  `Source.Frames` lists the chosen times until that first `Encode` makes them
  final. `Hints.Still` and `Hints.Orientation` are errors for a video. Samples
  are encoded as any animation's frames are: each is asked to show for half a
  second, and the mapping is read back from the scene delays, a scene the
  writer merged from several samples lasting their sum.
  A JPEG is decoded at a reduced size (`-define jpeg:size`) when the largest
  target is far smaller: the smallest eighth of the stored size that covers
  twice every target in both dimensions. The output size is then computed in
  Go from the headers' size and orientation (`fitSize`), because
  ImageMagick would round each side of the reduced frame separately.
  The still caps (a side of at most 16384 and at most 52 million pixels)
  apply to the frame the decode produces, on `Env` as on the `Processor`,
  and a still over them is refused with `ErrTooLarge`. `Open` refuses a still
  that no decode stage can reduce, a WebP included (see below), when its
  stored size is over them, before the profile read that may decode it. `Encode` judges a JPEG against the
  targets it is given, before any pixel is decoded: at the reduced size, or
  at its stored size when the reduction does not apply. A header read made
  with the hint confirms the size the decoder will produce, since a lossless
  JPEG ignores `jpeg:size`; when it differs from the computed size the JPEG
  is decoded and judged unreduced. One function chooses the reduction and
  gives the size that is checked, so the two cannot disagree. A 73 MP JPEG
  (7308x10000) is admitted when the largest target reduces it to at most
  six eighths (a target up to 3750 px) and refused at seven eighths (about
  55.9 MP) and where nothing reduces it; a 16382x3628 JPEG is admitted at
  targets of 2048 and below.
  A still WebP is always decoded by `webp-frames still`, so one decoder
  serves it at every target. libwebp scales to any size, which ImageMagick's
  WebP reader cannot: the decode is at twice the largest target in the stored
  aspect, never upscaled, when every target needs a resize and that is at
  most 7/8 of the stored size on both sides, and at the stored size
  otherwise; so one target that needs no resize makes the decode full-size
  for all targets of that `Encode`, as it does for a JPEG. The tool writes a
  PAM with the declared alpha and the ICC profile beside it, reported like an
  animation frame, and every target is encoded from that one frame in a run of
  its own, at the exact size `fitSize` gives when the decode was reduced and
  at the size ImageMagick computes otherwise. A WebP is reduced inside the
  decoder but is not in the set of formats whose caps follow the decoded size:
  its stored size is held to the still caps at the header, before any read of
  the whole image. libwebp's scaling bounds memory only for lossy images
  without alpha (268 MP of lossless, an 82 KB file, needs about 1 GB to
  decode at 1024; lossy with alpha about 370 MB; lossy without alpha 7 MB), so
  the scaling is a gain in time and memory for those, not a cap relaxation.
  The frame has no orientation tag, so the orientation is given: the hint if
  there is one, else the first EXIF directory when it settles it, else what
  ImageMagick's read of the whole image reports (its header read under `-ping`
  misses a WebP's orientation). The metadata comes from the tool, which reads
  it as ImageMagick does, through libwebp's mux: a file whose chunks the mux
  refuses (an alpha image without the VP8X alpha flag, a misplaced or repeated
  ALPH chunk, flags that disagree with the chunks) has no EXIF and no profile
  for ImageMagick, is decoded all the same, and so is oriented as stored. The
  decode size does not depend on the orientation, so the tool runs first and
  the exact output sizes follow from what it reports. The package's reading of
  the EXIF is the first directory's orientation, and declines (and asks
  ImageMagick) when an entry before it has a type outside 1 to 12, a count of
  2^31 or more or is a pointer to another directory, when the orientation is
  not a single SHORT of 1 to 8, and when the file has an XMP chunk, which
  ImageMagick also reads an orientation from. The profile is the tool's, so
  the one whole-image read ImageMagick may be asked for is the orientation's.
  Pixels differ from ImageMagick's own decode and resize by under 1 level of
  255 on average, because libwebp's scaler and upsampling are not its
  filter.
- `Env.ProbeFrames` reads each scene's delay and ICC flag from the headers,
  so a still of any size is accepted. The profile of a single-frame WebP is
  found in its RIFF chunk list when the layout settles it and by one decode
  otherwise, and a single-frame JXL is always decoded once for it (libjxl
  gives every JXL a profile); those decodes are bounded by the resource limits
  only, not by the frame caps. A GIF or WebP animation is refused with
  `ErrTooLarge` over the frame-size, frame-count and decoded-pixel caps above.
  `Ladder` lists the fallback rungs (frames are spent before resolution).
  `Env.EncodeRung` is `Open`, `Encode` of one rung as a WebP, still or
  animation, and `Close`, and returns the frame mapping the artifact holds.
  `animated` false makes the file a still (`Hints.Still`). The caller's `orient`
  replaces the file's orientation, in an animation as in a still, and a
  `profilePath` replaces the package's own sRGB profile.
  `Env.IdentifyArtifact` is the post-encode scene and size check, read from
  headers.
- `Env.RunMagickWithCoderFallback` retries a failed ImageMagick decode with the
  coder `CoderForMIME` names for formats it cannot sniff from an extensionless
  path. A run that reaches ImageMagick's time limit fails with `ErrTimeLimit`
  (when the runner fills `Stderr`) and is not retried.
- `LimitArgs` and `DeleteExpr` are the argument fragments consumers reuse for
  their own invocations over these artifacts. `MemoryProfile.LimitArgs()`
  bounds memory, map, area and list length (2048 scenes) for the process and
  time for each image (not for the process as a whole), with the profile's
  values, and `LimitArgs()` is `HighMemory.LimitArgs()`. A header read that
  reaches the list length fails with `ErrTooLarge`.
- `MemoryProfile` chooses ImageMagick's memory and map limits; see
  [Memory profiles](#memory-profiles). `Env.Memory` sets it for the `Env`
  methods, which add the profile's flags to every magick run, and its zero
  value is `HighMemory`.

### Operations

An operation turns one input file into upright, sized sRGB images in an
output directory. The operations are methods of a `Processor`, which runs the
tools itself because the input is untrusted:

- `Init()` must be the first call in `main` and in `TestMain` of any program
  that creates a `Processor`; see [the sandbox](#the-sandbox).
- `NewProcessor(ctx, profile MemoryProfile) (*Processor, error)` resolves
  `magick`, `ffmpeg`, `ffprobe` and `webp-frames` with the `tool` package,
  creates a private directory with `os.MkdirTemp`, installs a generated
  ImageMagick policy there and reads it back with `magick -list policy` and
  `-list resource`. It returns an error, removing the directory, unless the
  first policy file listed is the installed one with exactly the rules
  written, no other file contributes a rule and no resource limit reads back
  above the ceiling written (ImageMagick may lower one, such as threads to
  the host's CPUs). The profile sets the policy's memory and map ceilings and
  the flags of every run; an undefined one is an error. `Close` removes the
  directory.
- `Processor.Still(ctx, path, Encoding, dir) (Image, Usage, error)` accepts
  HEIC and HEIF (ImageMagick coder `heic`) and writes `still.<ext>`.
- `Processor.AnimationFrames(ctx, path, Encoding, maxFrames, dir) ([]Image, Usage, error)`
  accepts GIF (coder `gif`) and WebP (coder `webp`) and writes
  `frame-000.<ext>`, `frame-001.<ext>`, and so on. `maxFrames` (0 for 16,
  otherwise 1-64) bounds the evenly spaced frames sampled from the composited
  animation; a frame identical to the one before it after resizing is dropped,
  and `Image.Frame` records the source frame each file came from.
  - A GIF is composited by the library's own decoder, running as the sandbox's
    tool: the executable started again like `Init`'s helper, under the same
    limits and filter. It checks the block structure first (frame count, every
    frame inside the logical screen), decodes the frames, composites
    them one canvas at a time as ImageMagick's `-coalesce` does, stops after the
    last selected frame, and writes each selected frame as an uncompressed PAM
    file. It waits for the files to be
    encoded before writing more, so the output directory holds about one batch.
    ImageMagick encodes the frames a batch per run (at most 16 million input
    pixels, about 256 MiB), resizing, flattening on white for JPEG and
    stripping metadata. Memory is about one byte per pixel of every frame in the
    compositor plus one batch in ImageMagick. A GIF's ICC profiles (the
    `ICCRGBG1012` extension, any number of distinct ones) are written beside
    the frames as they are first needed. Each frame carries the profile it has
    after `-coalesce`, and is assigned it and converted to sRGB, whatever the
    profiles of the frames beside it in the batch. A GIF that ends between
    blocks without its trailer is decoded as ImageMagick does; one truncated
    inside a block or with a frame outside the logical screen is refused.
  - An animated WebP is composited by `webp-frames`
    (`tools/webp-frames/webp-frames.c`), a small C program linked against the
    pinned libwebp, run as a sandbox tool. The header read, the caps and the
    refusals come first, so the program never starts for an input they refuse.
    It runs libwebp's animation decoder (`WebPAnimDecoder`), which composites
    one frame at a time, and writes each selected frame as an uncompressed PAM
    file, waiting for the files to be encoded as the GIF compositor does;
    ImageMagick encodes them in the same batches. The program is told the
    canvas and frame count of the header read and refuses a file that differs.
    Memory is two canvases in the program plus one batch in ImageMagick, and
    does not grow with the number of frames. The file's ICC profile, if it has
    one, is written like a GIF's, assigned to the frames and converted to sRGB;
    ImageMagick's own decode of an animated WebP ignores it, as it ignores the
    EXIF orientation, so an oriented animation is output as stored; this is why
    a WebP with the animation flag always goes through it, even with one frame
    (a WebP without the flag is a still, which `webp-frames still` decodes,
    described above). Against `-coalesce` the animation's pixels are identical
    except where frames are blended over earlier content (blending without
    disposal), which differs by at most 1 level of alpha, 1 or 2 of colour where
    the result is opaque and 2 where it is partly transparent; and canvas no
    frame covers is transparent, where ImageMagick makes it opaque black if the
    file declares no alpha. The output of an opaque animation can differ in file
    shape (PNG colour type).
- `Processor.VideoFrames(ctx, path, Encoding, maxFrames, dir) ([]Image, Usage, error)`
  accepts MP4 and QuickTime (ffmpeg demuxer `mov`) and WebM and Matroska
  (demuxer `matroska`) and writes `frame-000.<ext>`, `frame-001.<ext>`, and
  so on. `maxFrames` is as for `AnimationFrames`. It runs `ffprobe`, `ffmpeg`
  and `magick`.
  - ffprobe picks the video stream flagged default that is not an attached
    picture, else the first video stream that is not one, and reads its size,
    display rotation, duration (the stream's, else the container's), frame
    count and colour transfer. Output is upright, so a
    stream rotated by 90 or 270 degrees gives portrait files. An input with no
    video stream is an error.
  - The samples are the centres of `maxFrames` equal intervals of the
    duration, fewer when the stream has fewer frames (the duration is the only
    guide when the frame count is unknown). Targets that coincide are
    merged, and a clip of unknown duration gives one sample at 0.
  - A stream of at most `selectMaxPixels` decoded pixels (frames times width
    times height as stored) is read by one ffmpeg run whose `select` filter
    keeps, for each sample time, the first frame at or after it. The frame
    count is the container's or, where it gives none, as in Matroska and
    MPEG-TS, the duration times the declared rate, so a variable-rate stream
    is estimated high and may take the seeks; a stream with neither never
    qualifies. A longer stream is read by one ffmpeg run per sample, which
    starts decoding a margin before the time and applies the same select. The
    margin is sixteen frames (the deepest reordering H.264 and HEVC allow, at
    the slower of the declared and the average rate) and at least half a
    second, so that the decode passes the leading pictures of an open GOP,
    which a seek to the keyframe alone cannot decode. It costs, on average,
    that many extra decoded frames per sample, and at worst one more GOP when a
    keyframe falls just inside it: one 720p keyint-300 sample took 0.07 s
    without the margin and 0.70 s with it. Both paths keep ffmpeg's timestamps
    as the container holds them (`-copyts`) and compare them with the sample
    time added to the stream's start, so they read the same frames, also when
    the stream starts after the container, by a fraction of a millisecond too.
    The seek runs are sequential: one process with all the seeks as separate
    inputs is faster but holds every decoder open at once (14 GB peak on 4K
    HEVC).
  - HLG and PQ video is scaled to the target size and then converted to SDR:
    linearize, convert the primaries to BT.709, tone-map with `hable`, and return to the
    BT.709 transfer and matrix. Other video gets no colour filter.
  - ffmpeg decodes, tone-maps HDR and reduces, and does nothing else. It
    scales an SDR frame to the smallest size covering twice the output in each
    dimension, when that is smaller than the frame, and an HDR frame to the
    output size, when larger, ahead of its tone map; Magick makes the final
    resize to the exact size computed from the upright source. ffmpeg writes
    each sample as PAM, at the decoder's own depth (16 bits for a 10-bit
    stream), to a hidden scratch file in `dir`. Magick encodes the samples to
    the encoding's format and quality with metadata stripped, a few
    per run (at most 16 million input pixels, about 256 MiB), so ImageMagick
    never holds more than that however many samples there are; a sample
    identical to the one before it (by ImageMagick signature) is dropped. The
    scratch files are removed before `VideoFrames` returns.
  - `Image.TimeMS` is the sample time in ms; the file shows the first frame at
    or after it. A sample has no known frame index, so `Image.Frame` is
    always 0.
  - Every ffprobe and ffmpeg run opens its input with `-protocol_whitelist
    file`, so a nested input cannot reach the network, and the pinned `-f`
    demuxer; for `mov` also `-enable_drefs 0`, so a track's reference to an
    external data file is not followed. Each ffmpeg run also sets
    `-threads 4` (decoder), `-filter_threads 4` and `-max_alloc` of 1 GiB;
    ffprobe sets `-threads 4` and the same `-max_alloc`.

- `Processor.ProbeVideo(ctx, path) (VideoInfo, Usage, error)` reads the
  properties of the video stream `VideoFrames` samples without decoding a
  frame, in one `ffprobe` run: duration in ms, frame rate, upright width and
  height, display rotation, whether it is HDR (HLG or PQ), codec name,
  whether the container has an audio stream and the frame count the container
  declares (0 when it declares none). It runs the gate and caps of
  `VideoFrames`, so a file `VideoFrames` refuses is refused with the same
  error.
- `Processor.ProbeAnimation(ctx, path) (AnimationInfo, Usage, error)` reads
  the frame count, upright canvas size and the display delay of each frame
  (ms; a stored delay below 20 ms is 100, as browsers show it) from the
  header read `AnimationFrames` makes, under the same gate and caps. The
  canvas is upright by the orientation the header reports, which a one-frame
  file can carry where only a decode sees it; `AnimationFrames` sizes its
  output from the decoded frame.
- `Processor.VideoFramesAt(ctx, path, Encoding, timesMS []int64, dir)
  ([]Image, Usage, error)` samples the given times, read by one decode or
  one seek per time as the stream's size decides, exactly as `VideoFrames`
  does; `VideoFrames` is this with evenly spaced times. `timesMS` holds 1 to 64 times in any order; they are sorted
  and repeats are sampled once. A time must be 0 or inside the duration;
  anything else is an error before a frame is decoded. `Image.TimeMS` is the
  requested time.
- `Processor.VideoFrameRun(ctx, path, Encoding, startMS int64, count int, dir)
  ([]Image, Usage, error)` returns `count` (1 to 64) consecutive frames from
  the first one at or after `startMS`, from a single ffmpeg run, so its cost
  is one run, not one per frame. A stream read by seeks starts that run a
  margin before `startMS` with one `-ss`; a stream read once is decoded from
  its start, with no `-ss`. ffmpeg names its scratch files by each frame's
  presentation time in microseconds (`-frame_pts` with a 1:1000000 encoder
  time base), which is where `Image.TimeMS` comes from, in ms: the frame's
  own time, not the request. A run that reaches the end returns fewer frames. `startMS` is
  checked as the times of `VideoFramesAt` are.
- `Processor.AnimationFramesAt(ctx, path, Encoding, indices []int, dir)
  ([]Image, Usage, error)` writes the frames with the given indices through
  the compositors `AnimationFrames` uses; `AnimationFrames` is this with
  evenly spaced indices. `indices` holds 1 to 64 indices in any order, sorted
  and de-duplicated; an index must be below `AnimationInfo.Frames`, else the
  call is an error before a frame is decoded. `Image.Frame` is the index.
- Every one of these runs the same gate as `VideoFrames` and `AnimationFrames`
  (type from the bytes, size caps, header read and caps, sandbox, policy)
  before it decodes anything; none reads a file without it. The limit of 64
  samples per call is the largest the caps were measured with; a caller with
  a time budget chooses a smaller count from `ProbeVideo`'s fields. As in
  `VideoFrames`, a sample identical to the one before it is dropped.

`path` and `dir` may be relative. Each call returns a `Usage`: the CPU time
summed over every tool run of the call and the largest peak resident set size
of any run, also when the call fails.

**The gate.** Before any tool runs, an operation checks that the input is a
regular file of at most 25 MiB and takes its type from the file's leading
bytes: GIF, WebP, HEIC and HEIF, MP4 and QuickTime, WebM and Matroska. The
name and any claim of the caller are never consulted; a file that is not of a
type the operation accepts gives an error wrapping `ErrUnsupportedType`, which
names the type found. The decoder is then pinned to that type, so no tool
picks a format from content. Image headers are read without decoding pixels
(`-ping`) and an input is refused when a frame is wider or taller than 16384
pixels or has more than 52 million, when an animation's canvas (the largest
width by the largest height over its frames) has more than 52 million pixels,
when an animation has more than 2000
frames or more than 900 million pixels to decode (all frames times the
canvas), when a video stream is wider or taller than 16384 or longer than six
hours (the stream's duration, else the container's), or when a video
container holds more than 32 streams. `AnimationFrames` and
`AnimationFramesAt` also refuse a call whose returned frames times the canvas
exceed 64 million pixels, and the message names the largest count that fits.
These refusals wrap `ErrTooLarge`, naming the cap and the values. The frame
caps apply to a still as it is decoded, not as it is stored (see `Env.Open`
above); a refusal names both sizes.

**Where the numbers come from.** The caps and ceilings were set from
measurements of the largest inputs they admit, made on a 12-CPU x86_64 host
with `measure` (below). A 25 MiB 4K 60 fps video, 10-bit HEVC (HLG or PQ) or
H.264, takes about 2.5 minutes with 64 samples and uses up to 2.7 times its
wall time in CPU, since the decoder and filters are bound to 4 threads. Its
peak, 2.8 GiB, is ImageMagick encoding the samples; ffmpeg stays near 300 MiB.
A 48 MP HEIC takes 8 s and 0.9 GiB. The still caps were measured on the
three Raspberry Pi 5 nodes of the media service, which runs with its
low-memory configuration: every format passes at 52 MP with at least 34% time
and 39% memory headroom, and a HEIC panorama at 52 MP takes about 25 s
(inferred from 28.7 s at 59.6 MP). 80 MP was rejected, since AVIF (1399 MiB
at 80 MP) and HEIC panoramas would need exceptions. The decision and its
measurements are in winnow's
`docs/plans/archive/2026-11-07_unified-pipeline.md`.
ImageMagick keeps 16 bytes per pixel, so
animations set the frame and pixel caps: it holds every composited frame
beside the decoded ones, and a GIF of full-canvas frames read by ImageMagick
completed up to 185 MP in all and failed at 194 MP, while a single 81 MP frame
completed and a 90 MP one did not. The pixel caps sit below those points.
ImageMagick's memory and map ceilings are those of the memory profile (1 and
2 GiB for `HighMemory`) and its disk ceiling is 4 GiB whatever the profile.
The time ceiling is at least four times the longest ImageMagick run measured
(36 s), and the ffmpeg allocation limit four times what an 8K frame needs. A
Raspberry Pi 5 measured at worst about 4 times that host: 4.1 times the CPU
for 4K HEVC decode, and 4.0 times the wall time for two 8000x8000 GIF frames
(a disk-bound run); peak memory was the same. The library sets no deadline
on a whole operation: a caller that wants one passes a context, and a 4K
HEVC video takes 8 minutes on the Raspberry Pi 5.

**The policy.** Tools run through the sandbox helper, in the Processor's
directory, with an environment of exactly `MAGICK_CONFIGURE_PATH`,
`MAGICK_TEMPORARY_PATH`, `TMPDIR` and `LC_ALL=C`, the two temporary paths
naming a directory made for that run and removed when the call returns (a
killed process or a helper killed after its wait delay can leave it). There is
no `PATH`, `HOME` or `XDG_CONFIG_HOME`, so no configuration file of the
caller's can widen the policy. The generated `policy.xml` is derived from the
coders each operation uses. It denies every delegate and every coder, then
allows the used coders for reading, writing or both, denies `@` indirect reads
and sets ceilings on memory, map, disk, area, width, height, list length, time
and threads.

`Encoding` sizes and encodes each output; zero fields take the defaults.
`Format` is `JPEG` (default), `WebP` or `PNG`. `Quality` is 1-100 (default 90),
`MaxEdge` the longest side in pixels, 16-16384 (default 2576), and `MaxArea`
the pixel count, 256-100000000 (default 3750000). The output satisfies both
bounds with one resample, is never upscaled, is upright, is converted to sRGB
when the source carries an ICC profile, has transparency flattened onto white
for JPEG, and carries no metadata: the converted profile is not embedded, and
an image without one is left as it is. The sRGB profile is embedded in the
library and exists as a temporary dot-file in the output directory only while
a tool runs.

Every image output, from a still, an animation, a video sample or `EncodeRung`,
is made by one ImageMagick argument list, so the choices below do not depend
on how the pixels reached the encode. The size is the largest one within both
bounds with the source's aspect ratio, each side rounded down (a 4000x3001
image at `MaxEdge` 2048 is 2048x1536). ImageMagick works it out, from `%[fx:]`
expressions of the size of the frame after `-auto-orient`, and is asked for it
exactly (`-resize WxH!`), so it is the size of the picture actually produced
whatever orientation the file's header does or does not report. The filter follows
what the source frame declares: Mitchell when it is palette-based (GIF, PNG8)
or declares alpha (a PNG colour type with alpha or a `tRNS` chunk, WebP alpha,
a HEIC or AVIF alpha plane), Lanczos for opaque truecolour and video. It is
ImageMagick's own default rule read from the source file rather than from
the intermediate a path hands ImageMagick, so a still read directly and the
same image composited choose alike. The header read reports it for
ImageMagick stills (a grayscale JPEG or PSD is no palette whatever
ImageMagick's image class says; TIFF, JPEG 2000, OpenEXR and PCX, whose
`-ping` read omits alpha or the palette, are read once in full for it when
an encode resizes), the
GIF compositor reports it for every frame and `webp-frames` reports the
file's declared alpha. Each frame is oriented and converted
inside its own parentheses, so frames of one run may carry different profiles.
The size of an output is read from the run that wrote it. Every output is
written at 8 bits per channel, whatever the depth of the source.

`dir` is an existing, empty directory the caller owns; the operation names its
own output files and moves no bytes beyond reading the source and writing
there. Every argument is validated before any tool runs, so a value out of
range, an unaccepted type, an input over a cap or a non-empty `dir` is an
error with nothing written. On any later failure the operation removes what
it wrote and `dir` is empty again. Each `Image` has the file's `Name`
relative to `dir`, its `Width` and `Height`, and `Frame` (the animation frame
index) and `TimeMS` (the video sample time, or the time an animation frame
starts at, the sum of the display delays before it; 0 for a still).

The accepted media types, decoder pins and tools of each operation are
internal.

The same inputs, arguments and tool binaries give the same output bytes;
callers pass policy values (quality, sizes, ceilings) in explicitly.

## The sandbox

Every tool the `Processor` runs goes through a helper: the program's own
executable, started again with a marker in its environment. `media.Init()`
detects the marker, runs the helper and never returns; in any other process it
returns at once and changes nothing. The helper

- sets `no_new_privs` and these limits (soft and hard), each above the
  largest value measured for a tool within the caps: CPU 210 s, address space
  8 GiB, file size 5 GiB, open files 1024, and tasks the user's current count
  plus 512;
- installs a seccomp filter, in pure Go, that makes `socket` and `socketpair`
  fail with `EPERM`, kills a process using another system-call ABI (32-bit
  or x32), and allows everything else;
- becomes a child subreaper, runs the tool as its child, and after the tool
  exits kills and reaps every process left under it, orphans included. A tool
  killed by a signal is reported as an error. Cancelling the context closes
  the helper's lifeline, on which it kills the tool and its descendants; a
  killed helper takes the tool with it.

The GIF compositor is one of these tools: the helper starts the program's own
executable as it, with a marker that `Init` recognises, so it runs under the
same limits and filter and its CPU and peak memory count in `Usage`. The
animated WebP compositor, `webp-frames`, is another, started from the tool
directory like `magick`.

The tool's environment and arguments are exactly those the `Processor`
passes, and it inherits only descriptors 0, 1 and 2: the helper marks the
rest close-on-exec (`close_range`, Linux 5.11) before starting it. Nothing
about the helper reaches it. The consumer gets no process-wide side effect.
`NewProcessor` refuses to return unless `Init` was called, and then starts the
helper once in a probe mode that tries `socket` under the filter and reads
the limits back; it requires the call denied and every limit set. The helper's rusage includes the tool's, so `Usage` is the
tool's (a 48 MP HEIC measured 919 MiB and 12.7 s of CPU directly and the
same through the helper), and the helper adds about 13 ms to a run.

The sandbox needs Linux 5.11 or later on amd64 or arm64; elsewhere, and on an
older kernel (its probe fails at `close_range`), `NewProcessor` returns an
error (the `Env` functions still work). Landlock is not used: the nodes
this was built for have none, so file confinement rests on the container.
A deployment for hostile input must supply it: a Deployment of its own with
no shared network namespace, a non-root user, a read-only root filesystem,
all capabilities dropped, `allowPrivilegeEscalation: false`, no secrets in
the container, a memory limit, scratch on a size-limited volume, and a
network policy denying egress. The measured model is one request per pod:

- a memory limit of 1536Mi per request in flight, with the `LowMemory`
  profile (see [Memory profiles](#memory-profiles));
- scratch on a disk-backed `emptyDir` (never `medium: Memory`) with a
  `sizeLimit` of about 8Gi, holding ImageMagick's temporary directory and the
  processor's scratch. ImageMagick's pixel cache is memory-mapped there, so
  its pages must be reclaimable under the memory limit;
- an `ephemeral-storage` request covering the volume;
- real disk on every node that can run the pod, since reclaiming writes
  those pages back.

Measured on a Raspberry Pi 5 (arm64), anonymous memory and total cgroup peak
for the largest inputs admitted: 8K HLG video 1270 and 1337 MiB, a 2-frame
52 MP GIF 1112 and 1932 MiB, a 52 MP 10-bit HEIC 944 and 960 MiB, and a
52 MP WebP 810 and 2660 MiB. The file-backed part is the pixel cache, which
the kernel reclaims under the limit.

The limits come from per-process peaks sampled from `/proc` while `measure`
ran its inputs on a 12-CPU x86_64 host: ImageMagick used up to 3.2 GiB of
address space (a 64 MP frame; it aborts when the limit is 3.0 GiB and works
at 3.3 GiB), 25 s of CPU (comparing a 4K video's 64 samples) and 199
descriptors; ffmpeg used 2.0 GiB, 9 s and 28 threads on 4K HEVC (decoder frame threads,
the filter pool and the scaler's threads, which follow the host's CPU
count). `RLIMIT_NPROC` counts every task of the real user, so it is set
relative to the count when the helper starts and bounds forks only where the
user has nothing else running, such as a container with its own uid; it is
not enforced for root.

## Memory profiles

A `MemoryProfile` sets ImageMagick's memory and map limits, the two between
which the pixel cache spills (memory, then map, then disk). The same profile
produces the Processor's `policy.xml` and the `-limit` flags of every magick
run the `Env` methods make, so a value means the same on both paths. Set it
with `NewProcessor(ctx, profile)` and `Env.Memory`; callers that build their
own magick runs start them with `profile.LimitArgs()`.

| Profile | memory | map | use |
| --- | --- | --- | --- |
| `HighMemory` (zero value) | 1 GiB | 2 GiB | a host with plenty of RAM |
| `LowMemory` | 512 MiB | 1 GiB | a container with a memory limit |

`LowMemory` was measured on 1536Mi Raspberry Pi 5 pods. At the `HighMemory`
limits an 80 MP image thrashed or was OOMKilled in most runs (the png8 and
lossy WebP outputs each passed once); at the `LowMemory` limits it completed,
with every lossy format taking 20 to 27 s. Caveats: the measurement is of one
request per pod, for a 2048 WebP output; avif stays near 1400 MiB of
anonymous memory whatever the profile; and PNG and lossless WebP inputs took 30 to 35 s.

## Flake outputs

Systems: `packages` and `lib` cover `x86_64-linux`, `aarch64-linux`,
`x86_64-darwin` and `aarch64-darwin`; `checks` and `devShells` are Linux only.

- `packages.<system>.<tool>` for each of `exiftool`, `file`, `ffmpeg`,
  `imagemagick`, `poppler-utils`, `pandoc`, `catdoc` and `lynx`, taken
  unmodified from this flake's pinned nixpkgs.
- `packages.<system>.default`: a directory containing every program of all
  the tools, flat (`<dir>/magick`, not `<dir>/bin/magick`).
- `lib.toolDir { system, tools }`: the same kind of directory built from a
  named subset, e.g. `tools = [ "imagemagick" "ffmpeg" ]`. An unknown name
  fails evaluation.
- `lib.ldflags { system, tools }`: the `-ldflags` list that points the `tool`
  package at that directory and records the flake revision.
- `lib.checkGoModPin { goModText, rev }` and `lib.assertGoModPin goModPath`:
  see [the pin check](#the-pin-check).
- `checks.<system>`: `test` (go test, with all the tools on `PATH` so the
  tool-dependent tests run instead of skipping; the WebP tests also use
  `cwebp` and `webpmux` from libwebp, which are on the test `PATH` only),
  `lint` (golangci-lint), `gofumpt`, and
  `pin` (the fixtures for the pin check), and `closure`, which builds a small probe binary the way a consumer would and
  asserts that it resolves `magick` and `webp-frames` from the injected
  directory, that both are in the binary's runtime closure, and that the
  libwebp `webp-frames` links is the one ImageMagick links.
- `packages.<system>.measure` and `apps.<system>.measure` (Linux): the
  measurement harness, with ImageMagick and FFmpeg from this flake.
- `devShells.<system>.default`: Go, golangci-lint, gofumpt and all the tools.

## Measuring

`internal/cmd/measure` generates the largest inputs the operations accept
(25 MiB 4K 60 fps HEVC and H.264 video, 48 and 24 MP HEIC, long GIFs and
animated WebPs) and runs each through a `Processor`, printing wall time, CPU,
peak RSS, output count and error as JSON lines and as a table. Generated
inputs are large and are not kept in the repository. The HEIC inputs can be
generated only where x265 can encode them (x86_64).

```sh
nix run .#measure -- <dir>                  # generate what is missing, then measure
nix run .#measure -- -gen-only <dir>        # only generate
nix run github:davars/winnow-media#measure -- -measure-only <dir>
```

`-measure-only` runs on another machine, such as an arm64 board, over a
directory copied from the one `-gen-only` filled. `-only <name>` runs one
input, `-repeat N` reports median wall time and CPU, and `-extra
kind:frames:WxH` (`gif`, `gifu` for full-canvas frames, `webp`) adds an
animation. `-list` prints the input names.

## Building a binary against the tools

```nix
{
  inputs.winnow-media.url = "github:davars/winnow-media";
  # Do not add: inputs.winnow-media.inputs.nixpkgs.follows = "nixpkgs";

  outputs = { self, nixpkgs, winnow-media, ... }:
    let system = "x86_64-linux"; pkgs = nixpkgs.legacyPackages.${system}; in {
      packages.${system}.default = pkgs.buildGoModule {
        pname = "myapp";
        version = "0";
        src = ./.;
        vendorHash = "...";
        ldflags = winnow-media.lib.ldflags {
          inherit system;
          tools = [ "imagemagick" "ffmpeg" "webp-frames" ];
        };
      };
    };
}
```

The injected tool directory's store path is embedded in the binary, so Nix's
reference scan includes the tools and their dependencies in the binary's
runtime closure (and in any container image built from it) without a
separate contents list.

## Rules for consumers

- **Call `media.Init()` first in `main` and in `TestMain`,** before flags,
  servers or anything with side effects: the `Processor` starts the program
  again as its sandbox helper.
- **No `nixpkgs.follows` on this input.** The tools must come from this
  flake's own locked nixpkgs; following the consumer's would change the
  binaries underneath the library.
- **One revision for code and tools.** Pin the Go module in `go.mod` and the
  tools in `flake.lock` to the same commit of this repository.

## The pin check

The Go module version in `go.mod` and the flake input in `flake.lock` must
name the same commit. `lib.assertGoModPin` reads a `go.mod` and compares the
12-hex-digit commit suffix of its `github.com/davars/winnow-media`
pseudo-version with the revision this flake was locked at. It returns `true`
on a match and throws, naming both values, on a mismatch, a missing
requirement, a tagged (non-pseudo) version, or a `replace` directive for the
module. When the input has no revision (a dirty or `path:` override) it prints
a warning and returns `true`.

Wire it into the evaluation of the package and of your CI check:

```nix
packages.${system}.default =
  assert winnow-media.lib.assertGoModPin ./go.mod;
  pkgs.buildGoModule { /* ... */ };
```

`lib.checkGoModPin { goModText, rev }` is the same comparison as a pure
function over strings; `checks.<system>.pin` exercises it over fixtures.

## Development

```sh
nix develop          # Go, golangci-lint, gofumpt and the tools
nix flake check      # test, lint, gofumpt, closure
```

CI runs `nix flake check` on x86_64 and aarch64 Linux. A weekly workflow
opens a pull request updating `flake.lock`.
