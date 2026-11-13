# winnow-media

A Go library that turns an untrusted image, animation or video file into
upright, sized, sRGB images, and a Nix flake that pins the programs it runs
(ImageMagick, FFmpeg and `webp-frames`). Licensed under AGPL-3.0.

- [`media`](https://pkg.go.dev/github.com/davars/winnow-media/media) is the
  library. It admits a file by its content, decodes it with external tools
  under limits and in a sandbox, and writes the stills, frames or animations
  asked for.
- [`tool`](https://pkg.go.dev/github.com/davars/winnow-media/tool) finds those
  tools: in a directory fixed at link time, or on `PATH`.
- The flake builds the tools and links a binary to them; see
  [Setup](#setup).

It builds on Linux and macOS. The sandbox needs Linux 5.11 or later on amd64
or arm64; elsewhere the library runs only with `Config.Unsandboxed`, which is
not for hostile input.

## Using it

The [package documentation](https://pkg.go.dev/github.com/davars/winnow-media/media)
is the reference. Its
[examples](https://pkg.go.dev/github.com/davars/winnow-media/media#pkg-examples)
(source: [`media/example_test.go`](media/example_test.go)) cover opening a
file and reading its `Info`, one still to two encodings, samples of a video,
an animation to one animated WebP, regions of a kept decode, a configuration
for hostile input, and handling errors.

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
  The content cannot choose its decoder, and a tool cannot open a socket, gain
  privileges or leave a process behind. The sandbox does not confine files or
  resident memory: a deployment supplies that, and sets `Config.Types` and
  `Config.Caps`, since the zero `Config` accepts everything the library can
  read.
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

`NewProcessor` needs four programs, whatever types are configured: `magick`
(ImageMagick 7), `ffmpeg`, `ffprobe` and `webp-frames`.

### With Nix

The flake provides the tools at the versions the library is tested with, and
links a binary to them:

```nix
{
  inputs.winnow-media.url = "github:davars/winnow-media";
  # Do not add: inputs.winnow-media.inputs.nixpkgs.follows = "nixpkgs";

  outputs = { self, nixpkgs, winnow-media, ... }:
    let system = "x86_64-linux"; pkgs = nixpkgs.legacyPackages.${system}; in {
      packages.${system}.default =
        assert winnow-media.lib.assertGoModPin ./go.mod;
        pkgs.buildGoModule {
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
build it from [`tools/webp-frames`](tools/webp-frames/webp-frames.c) against
the libwebp your ImageMagick uses.

```sh
cc -O2 -o webp-frames tools/webp-frames/webp-frames.c -lwebpmux -lwebpdemux -lwebp
```

Only the flake's tools are tested. With others, a type that `media.Types()`
lists can fail at `Open` because the build lacks its decoder, and output bytes
are your build's.

### Deploying

- The sandbox needs Linux 5.11 or later on amd64 or arm64, a mounted `/proc`,
  and permission to install a seccomp filter, which container runtimes'
  default profiles give. It needs no capability, user namespace or root.
- For hostile input, run as an unprivileged user of its own in a container
  with a read-only root filesystem, a memory limit, no network and no secrets.
- Put `Config.Dir` on a disk, not in memory, with a size limit.
- Nothing in the library limits how many calls run at once. The package
  documentation's
  [Sizing](https://pkg.go.dev/github.com/davars/winnow-media/media#hdr-Sizing)
  section says what each concurrent call needs;
  [the measurements](docs/measurements.md) record what one deployment
  measured.
- A process that ends without `Processor.Close` leaves a `media-processor-*`
  directory under `Config.Dir`; remove stale ones at startup.
- Importing `media` is all the setup a program needs. Each tool runs under a
  helper that is your own binary started again, so keep package `init`
  functions free of side effects (see
  [Hostile input](https://pkg.go.dev/github.com/davars/winnow-media/media#hdr-Hostile_input)).

### Testing your code

`Processor` and `Source` are concrete types: put the calls you use behind a
small interface of your own and fake it in unit tests. Tests that run the real
tools need them on `PATH`; `nix develop github:davars/winnow-media` is a shell
that has them, and `packages.<system>.default` is a directory of them to add
to your own.

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
- `packages.<system>.measure` and `apps.<system>.measure`: the measurement
  harness; see [the measurements](docs/measurements.md#how-to-measure).
- `devShells.<system>.default`: Go, the linters, the tools and the programs
  the tests use.

## The pin check

`lib.assertGoModPin ./go.mod` fails evaluation unless the
`github.com/davars/winnow-media` version in `go.mod` and the flake input in
`flake.lock` name the same commit. It accepts:

- a pseudo-version whose 12-digit commit suffix matches the locked revision;
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
