# winnow-media

A Go library and Nix flake for running external media tools (ImageMagick,
FFmpeg, Poppler, pandoc and friends) from a pinned, reproducible tool
closure. Licensed under AGPL-3.0.

This repository currently contains the tool closure, the Go package that
finds it, and the `media` package that builds the tools' command lines.
Hardening is coming.

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
lines and interprets their output. It does not run programs itself: the caller
supplies an `Env`, so it decides how tools are resolved, limited and
cancelled, and where scratch files live.

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

An operation turns one staged input file into upright, sized sRGB images in
an output directory. Each is a method of `Env` and needs only `Env.Run`:

- `Env.Still(ctx, Source, Encoding, dir) (Image, error)` accepts `image/heic`
  and `image/heif` (ImageMagick coder `heic`) and writes `still.<ext>`.
- `Env.AnimationFrames(ctx, Source, Encoding, maxFrames, dir) ([]Image, error)`
  accepts `image/gif` (coder `gif`) and `image/webp` (coder `webp`) and writes
  `frame-000.<ext>`, `frame-001.<ext>`, and so on. `maxFrames` (0 for 16,
  otherwise 1-64) bounds the evenly spaced frames sampled from the composited
  animation; a frame identical to the one before it after resizing is dropped,
  and `Image.Frame` records the source frame each file came from.
- `Env.VideoFrames(ctx, Source, Encoding, maxFrames, dir) ([]Image, error)`
  accepts `video/mp4` and `video/quicktime` (ffmpeg demuxer `mov`) and
  `video/webm` and `video/x-matroska` (demuxer `matroska`) and writes
  `frame-000.<ext>`, `frame-001.<ext>`, and so on. `maxFrames` is as for
  `AnimationFrames`. It runs `ffprobe`, `ffmpeg` and `magick`.
  - ffprobe picks the first video stream that is not an attached picture and
    reads its size, display rotation, duration (the stream's, else the
    container's), frame count and colour transfer. Output is upright, so a
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
    sample as PNG to a hidden scratch file in `dir`. One magick run encodes
    them to the encoding's format and quality with metadata stripped; a sample
    identical to the one before it (by ImageMagick signature) is dropped. The
    scratch files are removed before `VideoFrames` returns.
  - `Image.TimeMS` is the seek target in ms; the file shows the first frame at
    or after it. A seeked sample has no known frame index, so `Image.Frame` is
    always 0.

`Source` is the staged file (`Path`) and the media type the caller asserts for
it (`Type`); the type selects the decoder pin. A type the operation does not
accept gives an error wrapping `ErrUnsupportedType`.

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
range, an unaccepted type or a non-empty `dir` is an error with nothing
written. On any later failure the operation removes what it wrote and `dir` is
empty again. Each `Image` has the file's `Name` relative to `dir`, its `Width`
and `Height`, and `Frame` (the animation frame index) and `TimeMS` (the video
sample time; 0 for operations that do not sample a video).

The accepted media types, decoder pins and tools of each operation are
internal.

The same inputs, arguments and tool binaries give the same output bytes;
callers pass policy values (quality, sizes, ceilings) in explicitly.

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
- `devShells.<system>.default`: Go, golangci-lint, gofumpt and all the tools.

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
