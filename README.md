# winnow-media

A Go library and Nix flake for running external media tools (ImageMagick,
FFmpeg, Poppler, pandoc and friends) from a pinned, reproducible tool
closure. Licensed under AGPL-3.0.

The module has two packages and a flake:

- `github.com/davars/winnow-media/tool` resolves tool names to executable
  paths: from a directory injected at link time, or from `PATH` in development
  builds. `tool.FlakeRevision()` returns the flake revision injected the same
  way.
- `github.com/davars/winnow-media/media` builds ImageMagick and FFmpeg command
  lines and runs them on untrusted input inside a sandbox (resource limits, a
  system-call filter and cleanup of leftover processes), to turn one input file
  into upright, sized sRGB images.
- The flake builds the tool closure the `tool` package finds; see
  [Flake outputs](#flake-outputs).

## Example

```go
package main

import (
	"context"
	"log"
	"os"

	"github.com/davars/winnow-media/media"
)

func main() {
	ctx := context.Background()
	p, err := media.NewProcessor(ctx, media.Config{
		Types: []string{"video/mp4", "image/jpeg"}, // nil: every type of media.Types()
		Caps:  media.Caps{MaxInputBytes: 64 << 20, MaxFramePixels: 100_000_000},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer p.Close()

	src, _, err := p.Open(ctx, "clip.mp4", media.OpenOptions{})
	if err != nil {
		log.Fatal(err) // test with errors.Is for ErrUnsupportedType, ErrTooLarge
	}
	defer src.Close()
	info := src.Info()
	log.Printf("%s %dx%d, %d ms", info.Kind, info.Width, info.Height, info.DurationMS)

	dir, err := os.MkdirTemp("", "frames") // must exist and be empty
	if err != nil {
		log.Fatal(err)
	}
	images, usage, err := src.Encode(ctx, dir, media.Select{Frames: media.Even(8)},
		media.Encoding{Format: media.WebP, MaxEdge: 1024})
	if err != nil {
		log.Fatal(err)
	}
	for _, img := range images {
		log.Printf("%s %dx%d shows %+v", img.Name, img.Width, img.Height, img.Frames)
	}
	log.Printf("cpu %s, peak rss %d bytes", usage.CPU, usage.MaxRSS)
}
```

## Operations

A `Processor` opens one untrusted input file as a `Source`; the `Source` is
read as often as the caller likes. Each call returns a `Usage` (CPU time and
peak resident set size over every tool run of the call, also on failure). Each
output is sized and encoded by an `Encoding` (format JPEG, WebP or PNG,
quality, longest side, pixel area, `Opaque` to flatten alpha onto white), is upright, is converted to sRGB when the
source carries a profile, and carries no metadata.

| Call | Does |
|---|---|
| `Processor.Open` | Applies the gate and the caps and reads the headers, or probes a video, without decoding a pixel. `OpenOptions.Type` names the type of a file whose bytes name none (TGA, ICO, CUR, G3, DNG). `OpenOptions.Reuse` keeps a still's decode between `Encode` calls; `Source.KeptBytes` reports the disk it holds. |
| `Source.Info` | What is known of the file: type, kind (still, animation, video), size, orientation (and whether `OpenOptions` gave it), frame count and delays, and for a video the duration, rate, rotation, HDR, codec and audio. |
| `Source.Encode` | Writes the frames (`All`, `Even`, `Indices`, `Times`, `Run`) and region of a `Select`, one file per frame for each `Encoding`, or one animated WebP when `Animate` is set. |
| `Source.Close`, `Processor.Close` | Remove a Source's scratch; closing the Processor closes its Sources. |

A `Source` is checked against the file it was opened on before every `Encode`.
Its calls are serialised, and an animation or video is not decoded until
asked: the contracts, and what a Source keeps between calls, are in the doc
comments and the [package documentation](media/env.go).

Deprecated wrappers over these: `Still`, `AnimationFrames`,
`AnimationFramesAt`, `VideoFrames`, `VideoFramesAt`, `VideoFrameRun`,
`ProbeAnimation` and `ProbeVideo`. They write `still.<ext>` and
`frame-NNN.<ext>`.

Below the `Processor` is a decode-once layer for callers that run the tools
themselves: an `Env{Run, Stage, Scratch}` supplies the `Runner`, so the caller
decides how tools are resolved, limited and cancelled. `Env.Open` reads a
file's headers, applies the caps and selects its frames; `EnvSource.Encode`
writes one WebP, PNG or JPEG per `Encoding` from that single decode, and
`EnvSource.EncodeFrames` writes one file per frame. `Env.Open` applies the caps
only: the gate, sandbox and policy below belong to the `Processor`.

- Decode once: `Open`, `Encode`, `EncodeFrames`.
- Fallback ladder: `Ladder`, `EncodeRung`, `ProbeFrames`, `IdentifyArtifact`.
- Fragments for callers that build their own magick runs: `LimitArgs`,
  `DeleteExpr`, `RunMagickWithCoderFallback`.

## Guarantees

For a `Processor`, stated in full in the
[package documentation](media/env.go)
(`go doc github.com/davars/winnow-media/media`):

- **Gate.** The input must be a regular file within `Caps.MaxInputBytes`. It is
  opened once; the checks and Go's reads use that descriptor, and on Linux so
  do the tools' (`/dev/fd/3`, not the path), so a file put at the path
  afterwards is never read. Elsewhere the tools read through a link to the
  path, which the per-`Encode` identity check guards. Its type comes from its
  bytes, never its name, must be in `Config.Types`, and the decoder is pinned to
  it. The coder allow-list of the generated policy is built from `Types`, so
  content cannot choose another.
- **Caps.** Input size, pixel counts and frame count are bounded by
  `Config.Caps`, and side length, stream count and duration by constants (see
  the `Caps` doc comment); a refusal wraps `ErrTooLarge`.
- **Sandbox.** Every tool runs through a helper (the program's own executable,
  started again) that sets `no_new_privs`, resource limits and a seccomp filter
  denying sockets, and kills what the tool leaves behind. It needs Linux 5.11
  or later on amd64 or arm64; elsewhere `NewProcessor` fails unless
  `Config.Unsandboxed` runs the tools directly.
- **Policy.** The tool's environment is fixed and ImageMagick runs under a
  generated policy that `NewProcessor` reads back and refuses unless it is the
  only one in effect.
- **Determinism.** The same inputs, arguments and tool binaries give the same
  output bytes.

The sandbox does not confine files: a deployment for hostile input supplies
that (see the package documentation).

## Types and caps

`Types()` lists the media types the library has a pinned decoder for: those
ImageMagick reads with a built-in coder, and video: MP4, QuickTime, WebM,
Matroska, AVI, ASF (WMV), MPEG program, elementary and transport streams, and
FLV (the gate describes how a video is named in the
[package documentation](media/env.go)). SVG, MVG, MSL, PostScript, EPS, PDF
and text are never among them. TGA, ICO, G3 and DNG are in the list but are not
identified from content: `OpenOptions.Type` names them.
`Config.Types` narrows the set; nil is all of it. `Config.Caps` sets the limits
that turn input away. A zero `Caps` is the library's ceilings, not values
measured for any host, so a deployment for hostile input sets its own. The
`Caps` doc comment lists them; [the measurements](docs/measurements.md) record
the values a small deployment chose.

## Resources

`Config.Resources` bounds the memory, map, disk, threads and time of the tools
a `Processor` runs; the defaults and what each field reaches are in the
`Resources` doc comment. `LowMemory` is measured for one request per 1536Mi
pod (see [the measurements](docs/measurements.md#memory-profiles)), and
`MemoryProfile` and `LimitArgs()` remain for callers that use an `Env`.

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
  `cwebp` and `webpmux` from libwebp, on the test `PATH` only), `lint`
  (golangci-lint), `gofumpt`, `pin` (the fixtures for the pin check) and
  `closure`, which builds a small probe binary the way a consumer would and
  asserts that it resolves `magick` and `webp-frames` from the injected
  directory, that both are in the binary's runtime closure, and that the
  libwebp `webp-frames` links is the one ImageMagick links.
- `packages.<system>.measure` and `apps.<system>.measure` (Linux): the
  measurement harness, with ImageMagick and FFmpeg from this flake. Its usage
  is in [the measurements](docs/measurements.md#method).
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

- **Importing `media` is the setup.** Its package `init` runs the importing
  binary as the sandbox helper or frame compositor when a `WINNOW_MEDIA_*`
  marker is set in its environment, and does nothing otherwise; no call in
  `main` or `TestMain` is needed.
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
