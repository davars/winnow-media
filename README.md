# winnow-media

A Go library and Nix flake for running external media tools (ImageMagick,
FFmpeg, Poppler, pandoc and friends) from a pinned, reproducible tool
closure. Licensed under AGPL-3.0.

This repository currently contains the tool closure, the Go package that
finds it, and the `media` package that builds the tools' command lines.
Hardening and recipes are coming.

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
  ([]byte, error)`, and receives bare tool names (`"magick"`, `"ffmpeg"`).
  `Stage` is a `Stager` that creates an empty scratch file for a pattern and
  returns its path and a cleanup function.
- Face-detection source encodes: `Env.ProbeFrames` reads each scene's delay and
  ICC flag from the headers; `Ladder` lists the fallback rungs (frames are
  spent before resolution); `Env.EncodeRung` encodes one rung as a WebP, still
  or animation, and returns the frame mapping the artifact holds, with the
  colour, orientation, no-upscale and resource-limit arguments applied.
  `Env.IdentifyArtifact` is the post-encode scene and size check.
- `Env.RunMagickWithCoderFallback` retries a failed ImageMagick decode with the
  coder `CoderForMIME` names for formats it cannot sniff from an extensionless
  path.
- `Env.GenerateVideoThumbnails` (and `VideoThumbnailArgs`) samples up to fifteen
  evenly spaced frames of a video stream into two animated WebP previews.
- `LimitArgs` and `DeleteExpr` are the argument fragments consumers reuse for
  their own invocations over these artifacts.

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
