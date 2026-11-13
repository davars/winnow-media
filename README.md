# winnow-media

A Go library that turns an untrusted image, animation or video file into
upright, sized, sRGB images, and a Nix flake that pins the programs it runs
(ImageMagick, FFmpeg and `webp-frames`). It reads the image and video types
that
[`media.Types`](https://pkg.go.dev/github.com/davars/winnow-media/media#Types)
lists, among them JPEG, PNG, GIF, WebP, HEIC, AVIF, JPEG XL, TIFF, MP4,
QuickTime, WebM and Matroska, and writes JPEG, WebP and PNG. Licensed under
AGPL-3.0.

- [`media`](https://pkg.go.dev/github.com/davars/winnow-media/media) is the
  library. It admits a file by its content, decodes it with external tools
  under limits and in a sandbox, and writes the stills, frames or animations
  asked for.
- [`tool`](https://pkg.go.dev/github.com/davars/winnow-media/tool) finds those
  tools: in a directory fixed at link time, or on `PATH`.
- The flake builds the tools and links a binary to them; see
  [Setup](#setup).

It builds on Linux and macOS, and its sandbox needs Linux; see
[Deploying](#deploying).

## Using it

```go
ctx := context.Background()
p, err := media.NewProcessor(ctx, media.Config{})
if err != nil {
	log.Fatal(err)
}
defer p.Close()

// Open admits the file by its bytes and decodes nothing.
src, _, err := p.Open(ctx, "photo.heic", media.OpenOptions{})
if err != nil {
	log.Fatal(err)
}
defer src.Close()
info := src.Info()
fmt.Printf("%s %s, %dx%d\n", info.Kind, info.Type, info.Width, info.Height)

// Encode writes into an empty directory: here the first frame or image,
// upright and in sRGB, as a WebP of at most 1024 pixels a side.
dir, err := os.MkdirTemp("", "out")
if err != nil {
	log.Fatal(err)
}
images, _, err := src.Encode(ctx, dir, media.Select{},
	media.Encoding{Format: media.WebP, MaxEdge: 1024})
if err != nil {
	log.Fatal(err)
}
for _, img := range images {
	fmt.Printf("%s: %dx%d\n", filepath.Join(dir, img.Name), img.Width, img.Height)
}
```

That is the body of the package's `Example`, which the gate compiles. The
[package documentation](https://pkg.go.dev/github.com/davars/winnow-media/media)
is the reference, and its other
[examples](https://pkg.go.dev/github.com/davars/winnow-media/media#pkg-examples)
(source: [`media/example_test.go`](media/example_test.go)) cover several
encodings from one decode, samples of a video, an animation to one animated
WebP, regions of a kept decode, naming a type, a configuration for hostile
input, and handling errors.

| Call | Does |
|---|---|
| `NewProcessor` | Checks the tools, the sandbox and the generated ImageMagick policy, and fails rather than run without them. |
| `Processor.Open` | Admits one file by its bytes, type and size, without decoding a pixel, and returns a `Source`. |
| `Source.Info` | Describes the file: type, `Kind` (still, animation or video), size, orientation, and the facts of its kind. |
| `Source.Encode` | Writes the frames (`All`, `Even`, `Indices`, `Times`, `Consecutive`) and `Region` of a `Select` into an empty directory, once per `Encoding`: a file per frame, or one animated WebP. |
| `Source.KeptBytes` | Reports the disk a Source holds for `OpenOptions.KeepDecode`. |
| `Source.Close`, `Processor.Close` | Release a Source's file and scratch; closing the Processor closes its Sources. |

## What you can rely on

Each point is stated in full in the package documentation.

- **[Admission](https://pkg.go.dev/github.com/davars/winnow-media/media#hdr-Admission).**
  A file's type comes from its bytes, never its name, and must be one the
  `Processor` was configured for. Sizes are refused from headers, before
  pixels are decoded, against `Config.Caps`.
- **[Hostile input](https://pkg.go.dev/github.com/davars/winnow-media/media#hdr-Hostile_input).**
  The content cannot select a decoder outside `Config.Types`, and a tool
  cannot open a socket, gain privileges or leave a process behind. The sandbox
  does not confine files or resident memory: a deployment supplies that, and
  sets `Config.Types` and `Config.Caps`.
- **[Lifecycle](https://pkg.go.dev/github.com/davars/winnow-media/media#hdr-Lifecycle_and_concurrency).**
  Everything is safe for concurrent use. A failed or cancelled `Encode`
  leaves its directory empty and the `Source` usable.
- **[Errors](https://pkg.go.dev/github.com/davars/winnow-media/media#hdr-Errors).**
  Six sentinels and `ToolError` say what went wrong and whether a retry can
  succeed. Error text is not part of the contract.
- **[Determinism](https://pkg.go.dev/github.com/davars/winnow-media/media#hdr-Determinism).**
  The same input, request, library version and tool binaries give the same
  bytes on one architecture.

## Versioning

From v1 the module follows Go's compatibility rules. The
[Compatibility](https://pkg.go.dev/github.com/davars/winnow-media/media#hdr-Compatibility)
section of the package documentation says what that covers and what it does
not: notably, output bytes can change with any library version or tool
update, and the set of types can grow. The signatures of the flake's `lib`
functions and the flat layout of `packages.<system>.default` are covered too.

## Setup

The module needs Go 1.26 or later. `NewProcessor` needs four programs,
whatever types are configured: `magick` (ImageMagick 7), `ffmpeg`, `ffprobe`
and `webp-frames`.

### With Nix

The flake provides the tools at the versions the library is tested with, and
links a binary to them:

```nix
{
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-25.11";
  inputs.winnow-media.url = "github:davars/winnow-media";
  # Do not add: inputs.winnow-media.inputs.nixpkgs.follows = "nixpkgs";

  outputs = { self, nixpkgs, winnow-media, ... }:
    let system = "x86_64-linux"; pkgs = nixpkgs.legacyPackages.${system}; in {
      packages.${system}.default =
        assert winnow-media.lib.assertGoModPin ./go.mod;
        pkgs.buildGo126Module {
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

The tool directory's store path is embedded in the binary, so the tools are in
its runtime closure, and in any container image built from it.

- **No `nixpkgs.follows` on this input.** The tools must come from this
  flake's own locked nixpkgs; following yours would change the binaries under
  the library.
- **One revision for code and tools.** `go.mod` and `flake.lock` must name the
  same commit of this repository; [the pin check](#the-pin-check) enforces it.

### Without Nix

A binary built without the flake's `ldflags` finds the tools on `PATH`. You
provide ImageMagick 7, FFmpeg, and `webp-frames`, which no distribution ships:
build it from a checkout of this repository, against the libwebp your
ImageMagick uses and with its development headers.

```sh
cc -O2 -o webp-frames tools/webp-frames/webp-frames.c -lwebpmux -lwebpdemux -lwebp
```

- **No other ImageMagick policy.** `NewProcessor` generates the policy the
  tools run under and fails if a `policy.xml` in ImageMagick's own
  configuration directory, such as `/etc/ImageMagick-7`, adds any rule to it.
  Remove or empty that file; `magick -list policy` shows what is in effect.
- **Only the flake's tools are tested.** With others, a type that
  `media.Types()` lists can fail at `Open` because the build lacks its
  decoder, and output bytes are your build's.

### Deploying

- The sandbox needs Linux 5.11 or later on amd64 or arm64, a mounted `/proc`,
  and permission to install a seccomp filter, which container runtimes'
  default profiles give; it needs no capability, user namespace or root.
  Elsewhere the library runs only with `Config.Unsandboxed`, which is not for
  hostile input.
- For hostile input, the container and configuration to supply are listed
  under
  [Hostile input](https://pkg.go.dev/github.com/davars/winnow-media/media#hdr-Hostile_input).
- Give each process a `Config.Dir` of its own, on a disk, not in memory, with
  a size limit, and empty it at startup: a process that ends without
  `Processor.Close` leaves a `media-processor-*` directory there.
- Nothing in the library limits how many calls run at once.
  [Sizing](https://pkg.go.dev/github.com/davars/winnow-media/media#hdr-Sizing)
  says what each concurrent call needs, and
  [the measurements](docs/measurements.md) record what one deployment
  measured.
- Importing `media` is all the setup a program needs. The library starts your
  binary again as a helper, so keep package `init` functions free of side
  effects (see
  [Helper processes](https://pkg.go.dev/github.com/davars/winnow-media/media#hdr-Helper_processes)).

### Testing your code

Tests that run the real tools need them on `PATH`:
`nix develop github:davars/winnow-media` is a shell that has them, and
`packages.<system>.default` is a directory of them to put on `PATH`. The
package documentation's
[Testing](https://pkg.go.dev/github.com/davars/winnow-media/media#hdr-Testing)
section says how to structure the tests.

## Flake outputs

`packages` and `lib` cover `x86_64-linux`, `aarch64-linux`, `x86_64-darwin`
and `aarch64-darwin`; `checks`, `apps` and `devShells` are Linux only.

- `packages.<system>.<tool>` for `ffmpeg`, `imagemagick` and `webp-frames`.
  `ffmpeg` is nixpkgs' `ffmpeg-headless` (`ffmpeg` and `ffprobe`),
  `imagemagick` is nixpkgs' build, and `webp-frames` is built here against the
  libwebp that ImageMagick links.
- `packages.<system>.default`: a directory holding every program of those
  tools, flat (`<dir>/magick`, not `<dir>/bin/magick`).
- `lib.toolDir { system, tools }`: the same kind of directory for the named
  tools. An unknown name fails evaluation.
- `lib.ldflags { system, tools }`: the list of strings for `buildGoModule`'s
  `ldflags` that points the `tool` package at that directory and records the
  flake revision. `media` needs all three tools.
- `lib.version`: the release version, without the `v`.
- `lib.assertGoModPin goModPath` and `lib.checkGoModPin { goModText, rev }`:
  see [the pin check](#the-pin-check).
- `checks.<system>`: the gate CI runs (`test`, `lint`, `gofumpt`, `pin`,
  `closure`).
- `packages.<system>.measure` and `apps.<system>.measure` (Linux): the
  measurement harness; see [the measurements](docs/measurements.md#how-to-measure).
- `devShells.<system>.default`: Go, the linters, the tools and the programs
  the tests use.

## The pin check

`lib.assertGoModPin ./go.mod` fails evaluation unless the
`github.com/davars/winnow-media` version in `go.mod` and the flake input in
`flake.lock` name the same commit. It accepts:

- a pseudo-version whose 12-character commit suffix matches the locked revision;
- a tagged version equal to `v` plus `lib.version`.

It throws, naming both values, on a mismatch, a missing requirement, any other
tag, or a `replace` directive for the module. When the input has no revision
(a dirty tree or a `path:` override) it prints a warning and passes.

`lib.checkGoModPin { goModText, rev }` is the same comparison as a pure
function over strings.

## Development

```sh
nix develop          # Go, golangci-lint, gofumpt, the tools
nix flake check      # test, lint, gofumpt, pin, closure
```

CI runs `nix flake check` on x86_64 and aarch64 Linux; macOS is not tested. A
weekly workflow opens a pull request updating `flake.lock`, which changes the
tool binaries and so can change output bytes.

To release version X.Y.Z: set `version` in `flake.nix` to `X.Y.Z`, tag that
commit `vX.Y.Z`, and in the next commit move `version` to a `-dev` value, so
that no untagged commit passes the pin check as a tag.
