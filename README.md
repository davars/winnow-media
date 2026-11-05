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

- `Env{Run, Stage}`: `Run` is a `Runner`, `func(ctx, tool string, args ...string)
  (Result, error)`, and receives bare tool names (`"magick"`, `"ffmpeg"`).
  A `Result` holds the tool's `Stdout` and a `*Usage` (CPU time, peak RSS in
  bytes) that is nil when the runner cannot measure it; `UsageOf` builds one
  from an exited `*os.ProcessState`.
  `Stage` is a `Stager` that creates an empty scratch file whose name ends in
  an extension (`".webp"`) and returns its path and a cleanup function.
- Bounded WebP encodes: `Env.ProbeFrames` reads each scene's delay and
  ICC flag from the headers; `Ladder` lists the fallback rungs (frames are
  spent before resolution); `Env.EncodeRung` encodes one rung as a WebP, still
  or animation, and returns the frame mapping the artifact holds, with the
  colour, orientation, no-upscale and resource-limit arguments applied.
  `Env.IdentifyArtifact` is the post-encode scene and size check.
- `Env.RunMagickWithCoderFallback` retries a failed ImageMagick decode with the
  coder `CoderForMIME` names for formats it cannot sniff from an extensionless
  path.
- `Env.GenerateVideoThumbnails` (and `VideoThumbnailArgs`) samples up to fifteen
  evenly spaced frames of a video stream into two animated WebP thumbnails.
- `LimitArgs` and `DeleteExpr` are the argument fragments consumers reuse for
  their own invocations over these artifacts.

### Operations

An operation turns one input file into upright, sized sRGB images in an
output directory. The operations are methods of a `Processor`, which runs the
tools itself because the input is untrusted:

- `Init()` must be the first call in `main` and in `TestMain` of any program
  that creates a `Processor`; see [the sandbox](#the-sandbox).
- `NewProcessor(ctx) (*Processor, error)` resolves `magick`, `ffmpeg` and
  `ffprobe` with the `tool` package, creates a private directory with
  `os.MkdirTemp`, installs a generated ImageMagick policy there and reads it
  back with `magick -list policy` and `-list resource`. It returns an error,
  removing the directory, unless the first policy file listed is the
  installed one with exactly the rules written, no other file contributes
  a rule and no resource limit reads back above the ceiling written
  (ImageMagick may lower one, such as threads to the host's CPUs). `Close`
  removes the directory.
- `Processor.Still(ctx, path, Encoding, dir) (Image, Usage, error)` accepts
  HEIC and HEIF (ImageMagick coder `heic`) and writes `still.<ext>`.
- `Processor.AnimationFrames(ctx, path, Encoding, maxFrames, dir) ([]Image, Usage, error)`
  accepts GIF (coder `gif`) and WebP (coder `webp`) and writes
  `frame-000.<ext>`, `frame-001.<ext>`, and so on. `maxFrames` (0 for 16,
  otherwise 1-64) bounds the evenly spaced frames sampled from the composited
  animation; a frame identical to the one before it after resizing is dropped,
  and `Image.Frame` records the source frame each file came from.
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
  - Each sample is one ffmpeg run with an accurate input seek (`-ss` before
    `-i`), which decodes at most one group of pictures, so the cost depends on
    the number of samples and the GOP length, not on the duration. The runs are
    sequential: one process with all the seeks as separate inputs is faster
    but holds every decoder open at once (14 GB peak on 4K HEVC).
  - HLG and PQ video is converted to SDR before it is scaled: linearize,
    convert the primaries to BT.709, tone-map with `hable`, and return to the
    BT.709 transfer and matrix. Other video gets no colour filter.
  - ffmpeg scales once, to the size the shared bounds give, and writes each
    sample as PNG to a hidden scratch file in `dir`. Magick encodes the
    samples to the encoding's format and quality with metadata stripped, a few
    per run (at most 16 million input pixels, about 256 MiB), so ImageMagick
    never holds more than that however many samples there are; a sample
    identical to the one before it (by ImageMagick signature) is dropped. The
    scratch files are removed before `VideoFrames` returns.
  - `Image.TimeMS` is the seek target in ms; the file shows the first frame at
    or after it. A seeked sample has no known frame index, so `Image.Frame` is
    always 0.
  - Every ffprobe and ffmpeg run opens its input with `-protocol_whitelist
    file`, so a nested input cannot reach the network, and the pinned `-f`
    demuxer; for `mov` also `-enable_drefs 0`, so a track's reference to an
    external data file is not followed. Each ffmpeg run also sets
    `-threads 4` (decoder), `-filter_threads 4` and `-max_alloc` of 1 GiB.

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
pixels or has more than 64 million, when an animation has more than 2000
frames or more than 160 million pixels in all, when a video stream is wider or
taller than 16384 or longer than six hours (the stream's duration, else the
container's), or when a video container holds more than 32 streams. These
refusals wrap `ErrTooLarge`, naming the cap and the values.

**Where the numbers come from.** The caps and ceilings were set from
measurements of the largest inputs they admit, made on a 12-CPU x86_64 host
with `measure` (below). A 25 MiB 4K 60 fps video, 10-bit HEVC (HLG or PQ) or
H.264, takes about 2.5 minutes with 64 samples and uses up to 2.7 times its
wall time in CPU, since the decoder and filters are bound to 4 threads. Its
peak, 2.8 GiB, is ImageMagick encoding the samples; ffmpeg stays near 300 MiB.
A 48 MP HEIC takes 8 s and 0.9 GiB. ImageMagick keeps 16 bytes per pixel, so
animations set the frame and pixel caps: it holds every composited frame
beside the decoded ones, and a GIF of full-canvas frames completed up to
185 MP in all and failed at 194 MP, while a single 81 MP frame completed and a
90 MP one did not. The pixel caps sit below those points. ImageMagick's
memory, map and disk ceilings (1, 2 and 4 GiB) are unchanged. The time
ceiling is at least four times the longest ImageMagick run measured (36 s),
and the ffmpeg allocation limit four times what an 8K frame needs. A
Raspberry Pi 5 measured at worst about 4 times that host: 4.1 times the CPU
for 4K HEVC decode, and 4.0 times the wall time for two 8000x8000 GIF frames
(a disk-bound run); peak memory was the same. The library sets no deadline
on a whole operation: a caller that wants one passes a context, and a 4K
HEVC video takes 8 minutes on the Raspberry Pi 5.

**The policy.** Tools run through the sandbox helper, in the Processor's
directory, with an environment of exactly `MAGICK_CONFIGURE_PATH`,
`MAGICK_TEMPORARY_PATH`, `TMPDIR` and `LC_ALL=C`: no `PATH`, `HOME` or
`XDG_CONFIG_HOME`, so no configuration file of the caller's can widen the
policy. The generated `policy.xml` is derived from the coders each operation
uses. It denies every delegate and every coder, then allows the used coders
for reading, writing or both, denies `@` indirect reads and sets ceilings on
memory, map, disk, area, width, height, list length, time and threads.

`Encoding` sizes and encodes each output; zero fields take the defaults.
`Format` is `JPEG` (default), `WebP` or `PNG`. `Quality` is 1-100 (default 90),
`MaxEdge` the longest side in pixels, 16-16384 (default 2576), and `MaxArea`
the pixel count, 256-100000000 (default 3750000). The output satisfies both
bounds with one resample, is never upscaled, is upright, is converted to sRGB
when the source carries an ICC profile, has transparency flattened onto white
for JPEG, and carries no metadata. The sRGB profile is embedded in the library
and exists as a temporary dot-file in the output directory only while a tool
runs.

`dir` is an existing, empty directory the caller owns; the operation names its
own output files and moves no bytes beyond reading the source and writing
there. Every argument is validated before any tool runs, so a value out of
range, an unaccepted type, an input over a cap or a non-empty `dir` is an
error with nothing written. On any later failure the operation removes what
it wrote and `dir` is empty again. Each `Image` has the file's `Name`
relative to `dir`, its `Width` and `Height`, and `Frame` (the animation frame
index) and `TimeMS` (the video sample time; 0 for operations that do not
sample a video).

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

The tool's environment and arguments are exactly those the `Processor`
passes; nothing about the helper reaches it. The consumer gets no
process-wide side effect. `NewProcessor` refuses to return unless `Init` was
called, and then starts the helper once in a probe mode that tries `socket`
under the filter and reads the limits back; it requires the call denied and
every limit set. The helper's rusage includes the tool's, so `Usage` is the
tool's (a 48 MP HEIC measured 919 MiB and 12.7 s of CPU directly and the
same through the helper), and the helper adds about 13 ms to a run.

The sandbox needs Linux on amd64 or arm64; elsewhere `NewProcessor` returns
an error (the `Env` functions still work). Landlock is not used: the nodes
this was built for have none, so file confinement rests on the container.
A deployment for hostile input must supply it: a Deployment of its own with
no shared network namespace, a non-root user, a read-only root filesystem,
all capabilities dropped, `allowPrivilegeEscalation: false`, no secrets in
the container, a size-limited scratch volume and a memory limit, and a
network policy denying egress.

The limits come from per-process peaks sampled from `/proc` while `measure`
ran its inputs on a 12-CPU x86_64 host: ImageMagick used up to 3.2 GiB of
address space (a 64 MP frame; it aborts when the limit is 3.0 GiB and works
at 3.3 GiB), 25 s of CPU (comparing a 4K video's 64 samples) and 199
descriptors;
ffmpeg used 2.0 GiB, 9 s and 28 threads on 4K HEVC (decoder frame threads,
the filter pool and the scaler's threads, which follow the host's CPU
count). `RLIMIT_NPROC` counts every task of the real user, so it is set
relative to the count when the helper starts and bounds forks only where the
user has nothing else running, such as a container with its own uid; it is
not enforced for root.

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
  tool-dependent tests run instead of skipping), `lint` (golangci-lint), `gofumpt`, and
  `pin` (the fixtures for the pin check), and `closure`, which builds a small probe binary the way a consumer would and
  asserts that it resolves `magick` from the injected directory and that
  ImageMagick is in the binary's runtime closure.
- `packages.<system>.measure` and `apps.<system>.measure` (Linux): the
  measurement harness, with ImageMagick and FFmpeg from this flake.
- `packages.<linux system>.measure-image`: the measurement harness and its
  tools as a container image (`winnow-media-measure:latest`), for hosts without
  Nix.
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

To measure a board that runs k3s and has no Nix or docker, build the image in
CI: run the `measure-image` workflow (Actions, "Run workflow"), which builds
the aarch64 image on an arm64 runner and uploads it as an artifact kept for
seven days. On a machine with `gh`, and with `<dir>` filled by `-gen-only`
(where x265 can encode the HEIC inputs):

```sh
gh run download <run-id> -n winnow-media-measure-image
scp -r image.tar.gz <dir> node:     # the tarball and the input directory
```

On the node, `ctr` imports the gzipped tarball as it is. The image's
entrypoint is `measure`, so the arguments after the container id are the
harness's own, and the inputs are bind-mounted at `/data`:

```sh
sudo k3s ctr images import image.tar.gz
sudo k3s ctr images ls | grep winnow-media-measure   # the imported ref
sudo k3s ctr run --rm \
  --mount type=bind,src=$PWD/<dir>,dst=/data,options=rbind:ro \
  docker.io/library/winnow-media-measure:latest measure \
  -measure-only -repeat 3 /data
```

Use `-list` in place of the measuring flags to print the input names. The
image is 2.3 GB unpacked because ffmpeg's closure includes its optional
libraries.

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
          tools = [ "imagemagick" "ffmpeg" ];
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
