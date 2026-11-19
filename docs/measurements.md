# Measurements

The record behind the numbers in the `media` package: what was measured, how,
and what each figure justified. The constants in `media/` link here. Figures
that are projections, not measurements, are marked as such.

## How to read this

The figures were measured on the nixos-25.11 tool closure (ImageMagick
7.1.2-18, ffmpeg 8.0, libheif 1.20.2, Go 1.26.1), not re-measured on the
nixos-26.05 closure the flake now pins.

The numbers are of two sorts.

**The library's constants** hold for every `Processor`: the ceilings and fixed
limits (16384 pixels a side, 2000 animation frames, 64 video samples), the
sandbox's limits, ffmpeg's thread and allocation bounds, and the thresholds
that choose how a file is decoded. They were set on the reference host, a
12-CPU x86_64 machine, at the default `Resources`.

**One deployment's sizing** is an example, not a default. It is for a small
host: a Raspberry Pi 5 (4-core arm64) running one call at a time in a
container with a 1536 MiB memory limit. The library applies none of these
values; a zero `Caps` is the ceilings in the `Caps` doc comment.

| Setting | Small-host value |
|---|---|
| `Caps.MaxInputBytes` | 25 MiB (a requirement of that deployment, not a measurement) |
| `Caps.MaxFramePixels` | 52,000,000 |
| `Caps.MaxReturnedPixels` | 64,000,000 |
| `Caps.MaxDecodedPixels` | 900,000,000 |
| `Caps.MaxAnimationFrames` | 2000 |
| `Resources.Memory`, `Map` | 512 MiB, 1 GiB (the "low-memory limits" below) |

Every time and memory figure below names the host it is from. Take the small
host's values only for a host like it; for any other, measure.

## How to measure

`internal/cmd/measure` generates the largest inputs the small-host caps admit
(25 MiB 4K 60 fps HEVC and H.264 video, 48 and 24 MP HEIC, long GIFs and
animated WebPs) and runs each through a `Processor` with the zero `Config`,
printing wall time, CPU, peak RSS, output count and error as JSON lines and as
a table. Generated inputs are large and are not kept in the repository. The
HEIC inputs can be generated only where x265 can encode them (x86_64).

```sh
nix run .#measure -- <dir>                  # generate what is missing, then measure
nix run .#measure -- -gen-only <dir>        # only generate
nix run github:davars/winnow-media#measure -- -measure-only <dir>
```

The rendering inputs (PDFs, an SVG and a candidate set for `ExtractPreview`)
are generated the same way and listed by `-list`.

`internal/cmd/transcodebench` runs one `Source.Transcode` of a file the caller
supplies, every setting and limit a flag, and prints its wall time, CPU, peak
RSS and output size as a JSON line; see Transcoding.

`-measure-only` runs on another machine, such as an arm64 board, over a
directory copied from the one `-gen-only` filled. `-only <name>` runs one
input, `-repeat N` reports median wall time and CPU, and `-extra
kind:frames:WxH` (`gif`, `gifu` for full-canvas frames, `webp`) adds an
animation. `-list` prints the input names.

To size a host of your own, run the harness there and compare each input's
wall time and peak RSS with your budget for one call. The harness cannot set
`Caps` or `Resources`: to measure a configuration, run your own program's
calls and read the `Usage` each returns, and your container's memory peak,
which also counts the file-backed pixel cache.

## Caps and ceilings

On the reference host, with `measure`:

- A 25 MiB 4K 60 fps video, 10-bit HEVC (HLG or PQ) or H.264, takes about
  2.5 minutes with 64 samples and uses up to 2.7 times its wall time in CPU,
  since the decoder and filters are bound to 4 threads. Its peak, 2.8 GiB, is
  ImageMagick encoding the samples; ffmpeg stays near 300 MiB.
- A 48 MP HEIC takes 8 s and 0.9 GiB.
- The limit of 64 samples per call is the largest count the caps were
  measured with. A caller with a time budget chooses a smaller count from the
  fields of `Source.Info`.

On the small host, the still values (a side of 16384 and 52 million pixels):

- 52 million pixels sits just above the largest phone formats: 48 MP (8064x6048,
  48.8 MP) and 50 MP sensors (at most 8192x6144, 50.3 MP). Every format
  admitted at this size completes within the 1536 MiB memory limit.
- Every format passes at 52 MP with at least 34% time and 39% memory
  headroom, and a HEIC panorama at 52 MP takes about 25 s (projected from
  28.7 s at 59.6 MP).
- 80 MP was rejected, since AVIF (1399 MiB at 80 MP) and HEIC panoramas would
  need exceptions.

The cost of that choice, which is the memory and time of ImageMagick's
full-size pixel cache that the reduced decode never built:

| Input (8000x6500 unless stated) | Before: time / peak memory | After: time / peak memory |
|---|---|---|
| lossy | 1.05 s / 92 MiB | 2.90 s / 821 MiB |
| lossy with alpha | 1.84 s / 267 MiB | 4.52 s / 1020 MiB |
| lossless | 3.15 s / 296 MiB | 5.00 s / 987 MiB |
| 16383x16383 lossless, about 82 KB | 1.4 s / 1.09 GB | 9.2 s (19 s CPU) / 2.17 GB |

Measured on x86_64 (12 CPUs, not idle) on 2026-11-18: `Open` and two `Encode`
calls at `MaxEdge` 1024 and 256, zero `Config`, with no `KeepDecode`. "Before"
is the release v1.1.0 route and "After" this one.

- With `KeepDecode` the new route is equal or faster than the old (lossy,
  same host: 2.93 s against 3.90 s), because the first decode is shared by
  every encode.
- On a Raspberry Pi 5 (tin, aarch64, 2026-11-18), `KeepDecode` was even with or
  faster than the old route, with a peak of 1013 MiB under a 1536 MiB limit.
  Without it the new route was 3 to 10 times slower. A 52 MP WebP with alpha
  took about 21 s on both.
- Output bytes of a still WebP without `KeepDecode` are now ImageMagick's own
  full-size decode and resize, so they differ from the reduced decode's. A
  consumer that caches outputs by recipe revision must bump its revision.

Animations:

- ImageMagick keeps 16 bytes per pixel, so animations set the frame and pixel
  caps: it holds every composited frame beside the decoded ones. A GIF of
  full-canvas frames read by ImageMagick completed up to 185 MP in all and
  failed at 194 MP, while a single 81 MP frame completed and a 90 MP one did
  not. The pixel caps sit below those points.
- The header read holds about 62 KiB per frame: 10000 frames of a GIF peaked
  at 624 MiB in ImageMagick. This is why the frame count is capped at 2000, a
  structural bound below the policy's list length of 2048.
- A frame composites onto the frames before it, so every frame up to the last
  selected is decoded, and a one-colour GIF under 25 MiB can declare tens of
  gigapixels; decoding therefore has a bound of its own, the decoded pixels.
- The returned (64 million) and decoded (900 million) pixel caps keep an
  animation within a 30 s budget per call on a Raspberry Pi
  5. The worst admitted GIF, 225 frames of 2000x2000 with 16 returned (64 MP
  returned, 900 MP decoded), took 27.8 s there, and 16 frames of 2000x2000
  alone took 20.2 s. A returned megapixel costs about 0.28 s and a decoded
  one about 0.01 s.

ImageMagick policy ceilings:

- The memory, map and disk ceilings are those of the Processor's `Resources`
  (1, 2 and 4 GiB by default, the limits of the high-memory measurements).
- The time ceiling is at least four times the longest ImageMagick run
  measured to complete (36 s). The ffmpeg allocation limit is four times what
  an 8K frame needs (1 GiB; a 4K HDR frame needs 32 MiB, one float plane of the
  tone-mapping filters).

Raspberry Pi 5 against the reference host: at worst about 4 times. CPU was 4.1
times for 4K HEVC decode, and wall time 4.0 times for two 8000x8000 GIF frames
(a disk-bound run); peak memory was the same. A 4K HEVC video takes 8 minutes
on the Raspberry Pi 5.

Decoder threads: an 8K 10-bit HLG source peaked at 1811 MiB with 4 ffmpeg
threads and 1280 MiB with 2, for 28% more time. Sources with more pixels than
4096x2160 are therefore sampled with 2 threads and the rest with 4. The HEVC decoder
holds about one frame per thread plus references.

## Sandbox limits

Each limit sits above the largest value measured for any tool run within the
caps, from per-process peaks sampled from `/proc` while `measure` ran its
inputs on the reference host:

- ImageMagick used up to 3.2 GiB of address space (a 64 MP frame; it aborts
  when the limit is 3.0 GiB and works at 3.3 GiB), 25 s of CPU (comparing a
  4K video's 64 samples) and 199 descriptors (a 64-frame animation).
- ffmpeg used 2.0 GiB, 9 s and 28 threads on 4K HEVC (decoder frame threads,
  the filter pool and the scaler's threads, which follow the host's CPU
  count).

The values chosen, from those peaks:

- CPU: the limit is `Resources.CPUTime`, by default `TimeLimit` times
  `Threads`, 1200 s. That is a backstop against a runaway tool, not a tuned
  bound: the heaviest single process measured used 25 s. A bound derived from
  the measurement would be about 210 s (25 s, times 4.1 for the worst CPU ratio
  of the small host, times a margin of 2); a deployment that wants one sets
  `CPUTime`.
- CPU enforcement: the helper enforces `CPUTime` itself. Every 50 ms it reads
  the tool's CPU time, over all its threads, on the clock the kernel checks
  `RLIMIT_CPU` against (PROF: tick-sampled user plus system time), and kills
  the tool once that reaches `CPUTime`. The kernel's own limit is a backstop
  behind it, at `CPUTime` plus a tenth of it or 1 s, whichever is more: 1320 s
  at the defaults. A kill counts as the CPU limit (`ErrTimeLimit`) when the
  helper made it, or when the tool was killed with its PROF clock at or past
  the backstop. The margin is sized from how far PROF runs ahead of the
  scheduler's run time (SCHED), which `Usage.CPU` reports: about 1.3% under
  load on the reference host, 4.8% on 2 loaded CPUs, and 7 to 9% on CI
  virtual machines (implied, not measured directly).
- Address space 8 GiB: 2.5 times the worst peak (3.2 GiB), leaving room for
  the per-thread arenas that grow with a host's CPU count. The limit is the
  Processor's `Memory` plus `Map` plus 5 GiB, 8 GiB at the defaults; the file
  size limit is `Disk` plus a quarter, 5 GiB at the defaults.
- Open files 1024: five times the 199 measured.
- Tasks: 512 beyond the user's current count, against 28 measured in one tool
  (ffmpeg).
- `RLIMIT_NPROC` counts every task of the real user, so it is set relative to
  the count when the helper starts and bounds forks only where the user has
  nothing else running, such as a container with its own uid; it is not
  enforced for root.
- The helper measures `Usage` from the tool's own rusage and that of the
  descendants it reaps, not from its own, which starts at the calling
  program's peak resident set. A 48 MP HEIC measured the same peak and CPU
  time directly and through the helper. The helper adds about 13 ms to a run.

## The small host's deployment

One call at a time per container, with:

- a memory limit of 1536 MiB and `Resources` of 512 MiB memory and 1 GiB map
  (the low-memory limits);
- `Config.Dir` on a disk-backed volume, never a memory-backed one, with a size
  limit of about 8 GiB. ImageMagick's pixel cache is memory-mapped there, so
  its pages must be reclaimable under the memory limit, and reclaiming writes
  them back to that disk.

Anonymous memory and total cgroup peak on a Raspberry Pi 5 (arm64), for the
largest inputs admitted:

| Input | Anonymous | Cgroup peak |
|---|---|---|
| 8K HLG video | 1270 MiB | 1337 MiB |
| 2-frame 52 MP GIF | 1112 MiB | 1932 MiB |
| 52 MP 10-bit HEIC | 944 MiB | 960 MiB |
| 52 MP WebP (see below) | 810 MiB | 2660 MiB |

The file-backed part is the pixel cache, which the kernel reclaims under the
limit.

The 52 MP WebP row was measured at 9309a82 on 2026-11-06 on the same Pi 5
(tin), with the nixos-25.11 closure, when a still WebP was already a whole
ImageMagick decode (through the pre-v1 pipeline, not the current code). It
has not been re-measured.

## Tool closure

What the poppler tool adds to a binary's runtime closure, measured on the
nixpkgs revision `flake.lock` locked at the time (poppler 26.11.0, ImageMagick
7.1.2-35, ffmpeg-headless 8.1.3), as the sum of `nix path-info` NAR sizes. The
flake links only `pdfinfo` and `pdftoppm` from `poppler-utils`.

| Closure | Paths | Size |
|---|---|---|
| ImageMagick and ffmpeg together | 155 | 373.9 MiB |
| Added by `poppler-utils` | 5 | 29.3 MiB |

The five are `poppler-utils` (6.8 MiB), `poppler-data` (12.4 MiB), `nss`
(4.0 MiB), `sqlite` (5.7 MiB) and `nspr` (0.3 MiB). Everything else poppler
links is already in ImageMagick's closure, among it the `fontconfig`,
`freetype`, `libpng`, `lcms2`, `libjpeg-turbo` and `cairo` builds. These
figures are a measurement, not a check: the flake's closure check asserts only
which programs a consumer's binary resolves and that a tool it does not list
is absent.

## Rendering

Measured with `internal/cmd/measure` on the reference host (12-CPU x86_64,
Linux 6.8) at the default `Resources`, as medians of three runs, with
poppler 26.11.0 and ImageMagick 7.1.2-35 from the flake's lock. The host was
not idle: its load average was between 3 and 12. Each row is one call of the
operation, opening included, under the sandbox and the file confinement.

| Input | Operation | Wall | CPU | Peak RSS |
|---|---|---|---|---|
| 200-page vector PDF, 18.1 MiB, 1500 paths a page | `OpenDocument`, then page 0 at `LongEdge(2048)` | 3.42 s | 3.38 s | 33 MiB |
| the same | `OpenDocument`, then page 100 at `Resolution(200, 0)` | 3.69 s | 3.65 s | 35 MiB |
| one 16384 by 16384 point page holding a 16384 by 16384 RGB image, 3.0 MiB | `OpenDocument`, then `LongEdge(16384)` | 14.0 s | 14.0 s | 1809 MiB |
| SVG of 4000 by 4000 pixels with 400 blurred and shadowed shapes, 27 KiB | `RenderSVG` | 33.7 s | 52.3 s | 525 MiB |
| 16 JPEG candidates of 0.3 to 5 megapixels in a 17.7 MiB file | `ExtractPreview` | 0.56 s | 0.36 s | 26.5 MiB |

The largest page raster is a 16384 by 16384 page, 768 MiB at 3 bytes a pixel
in pdftoppm, and the run's peak is 1809 MiB with its image decoded. The
`measure` harness generates these inputs (`-list` names them).

**The page limit.** `maxDocumentPages` is 100,000: `pdfinfo -box` over a
100,000-page file of 11 MB took 1.04 s and 208 MiB, and printed 35 MB, which
`OpenDocument` holds. A document poppler accepts cannot declare more pages
than it has objects, so the limit bounds the work, not a count a caller
could be misled by. Projection: the cost grows linearly in pages.

**The candidate limit.** `maxPreviewCandidates` is 16: the row above is the
cost of 16 candidates of growing size, two header reads each, with one
scratch copy alive beside the best. A camera raw file names four kinds of
preview, so 16 is four times what one needs.

**Fonts.** A render run has no home directory, so fontconfig reads no user
cache and scans the host's font files each run. On the reference host, with 29
font files, a `pdftoppm` run of a one-page text PDF took 191 ms that way,
against 22 ms with the user's cache or with a cache directory of its own
already warm (`XDG_CACHE_HOME` set): 170 ms of CPU for each run. The library
builds no cache: the cost is small beside a render of any page that has
content (the rows above) and beside what a consumer does with a page. It is a
cost per run, so it grows with the host's font count, and a deployment
that renders many small pages on a host with many fonts should measure it.
Decision branch: if a rendering host shows more than about a second a run, add
a read-only cache built once by `NewProcessor`.

**Files a render run opens.** The confinement's lists
(`renderReadPaths`, `renderWritePaths` and `systemLibraryPaths` in
`media/sandbox.go`) come from `strace -f -e trace=openat,open,access,newfstatat,statx,readlink`
of `pdftoppm` and `magick rsvg:` run under `env -i LC_ALL=C` on a text page and
a text SVG. Outside the Nix store they opened: `/etc/fonts` (the configuration
and its `conf.d`), `/usr/share/fontconfig/conf.avail`, `/usr/share/fonts`,
`/usr/local/share/fonts`, and, only to find them absent, `/var/cache/fontconfig`
(a cache in a format of another fontconfig build, which no run used here),
`/.cache/fontconfig` and `/.config/fontconfig`. The last two are where
fontconfig looks for a home directory's files when there is no home, the
root. The confinement refuses all three, so a file there is never read; the
rest of the lists are what the tools of the package documentation's Hostile
input section need besides: `/proc`, `/sys/devices/system/cpu`, `/dev/urandom`,
`/dev/null`, `/etc/localtime`, `/etc/ld.so.cache` and `/usr/lib/locale`, read
by the C library and the OpenMP runtime. Of these, only the first group
showed in the strace; the others are the library's allowance for what a host's
C library reads, not measured. Tests run the renders confined with all of
them, on Linux 6.8.

## Transcoding

Measured on a 32-CPU x86_64 host (two Xeon E5-2640 v2, Ivy Bridge: AVX,
no AVX2; 31 GiB; Linux 6.8, Landlock ABI 4) on 2026-11-18 and 19, with
ffmpeg 8.1.3 (SVT-AV1 3.1.2, x265 4.1, libopus 1.6.1) from the flake's lock.
`internal/cmd/transcodebench` ran one `Source.Transcode` through a
`Processor` with `RenderTypes`, so the run was confined, with
`TranscodeLimits{Threads: 32, CPUTime: 7 days, WallTime: 24 h,
AddressSpace: 32 GiB, OutputBytes: 16 GiB}`. Each is set beside the same
encode by plain ffmpeg with no limits and automatic threads, its arguments
the call's encoder arguments without the input prefix, under `/usr/bin/time
-v`. The host served other work, mostly idle: its 1-minute load average,
sampled each minute for every run but the first three plain ones, was 3.5 to
7.8 during the control encodes, which make about 4.8 themselves, and 15 to 31
during the 4K encodes. Wall and CPU are those of the tool run; peak address
space was sampled every 10 s from `/proc`, so is a lower bound.

The inputs are two originals from one consumer's archive: a 148.5-minute
720x388 H.264 MP4 of 2.38 GiB with two AAC streams and chapters (the
control), and a 77-second 3840x2160 HEVC QuickTime of 480 MiB from a phone.

| Encode | Run | Wall | CPU | Peak RSS | Peak address space | Threads |
|---|---|---|---|---|---|---|
| control, x265 `medium` CRF 18, AAC 256k, MP4 `FastStart` | plain, first | 8276 s* | 39,491 s* | 672 MiB | 4.8 GiB | 92 |
| the same | plain, again | 8297 s | 39,976 s | 676 MiB | 4.8 GiB | 92 |
| the same | the call | 8375 s | 40,381 s | 698 MiB | 6.2 GiB | 140 |
| 4K, x265 `medium` CRF 18, AAC 256k, MP4 `FastStart` | plain | 1015 s | 26,991 s | 3.5 GiB | 7.9 GiB | 108 |
| the same | the call, twice | 1010 s, 1010 s | 27,019 s, 27,033 s | 4.3 GiB | 10.1 GiB | 172 |
| 4K, SVT-AV1 preset 5 CRF 18, Opus 256k, WebM | plain, no `BitExact` | 589 s | 11,790 s | 13.2 GiB | 20.3 GiB | 194 |
| the same, `BitExact` | the call, five times | 497 to 504 s | 11,168 to 11,288 s | 13.3 to 14.3 GiB | 22.5 GiB | 258 |

\* The first plain control run overlapped, for about its first 15 minutes,
another process's 12-worker preview rebuild on the same host, and its load
was not sampled; the second ran back to back with the call. The plain 4K
runs ran before load sampling began.

- **Time.** Through the call the control took 0.9% more wall time and 1.0%
  more CPU than plain ffmpeg run back to back with it: 0.94 of the video's
  duration, and 4.5 CPU-seconds per second of video. The x265 encode of a 720x388
  stream keeps about 5 of the 32 CPUs busy, whatever the threads. The 4K
  encodes took the same time through the call, or less: the AV1 encode's
  plain run is one sample, unsampled for load.
- **Threads and address space.** The call's decoder and filters run with
  `TranscodeLimits.Threads` (32 here) where plain ffmpeg chooses its own
  (16 at most for decoding), which shows as more threads and address space.
  At the default `Resources` a transcode's address space limit is 8 GiB, which
  the 4K encodes exceed (10.1 and 22.5 GiB): a caller sets
  `TranscodeLimits.AddressSpace` for them. The tasks stay under the
  sandbox's 512.
- **Resident memory.** An SVT-AV1 encode of 4K at preset 5 on this host held
  13 to 14 GiB, almost half the host's memory, so two at once would not fit;
  the x265 encode of the same file held 4.3 GiB.
- **The same streams.** Every call wrote the same streams as plain ffmpeg
  (ffmpeg's `-f streamhash`, SHA-256 per stream). The MP4 files are
  byte-identical, once the call logs at ffmpeg's default level: at `-v error`
  each differed in one byte, the log level x265 records in the stream's
  header. The 4K MP4 was checked byte for byte after that change; the
  control's MP4 was not run again, and differed before it in that byte
  alone.
- **BitExact WebM.** Two calls with `-fflags +bitexact` alone wrote the same
  streams but different files: one had 131 audio packets before the first
  video packet, the other 135. The muxer writes the packets it holds once
  they span more than `max_interleave_delta` (10 s), and the 4K AV1 encode's
  first packet came later than that. With `-max_interleave_delta 0`, which
  `BitExact` now adds, three calls wrote byte-identical files. On the
  12-CPU reference host, six plain runs of the same file were identical with
  and without the flag: the race needs a slow first packet.
- **Hosts.** The AV1 video stream was the same on both hosts; the Opus
  stream was not (libopus chooses its code paths by instruction set). x265
  sets its frame threads from the host's processor count, not the process's
  affinity: on the reference host, a 6-second 720p encode gave the same file
  under `taskset` of 1, 4 and 12 CPUs, and three different files at ffmpeg
  `-threads` 1, 2 and 4, which ffmpeg hands x265 as its frame-thread count.
  So `TranscodeLimits.Threads` does not reach the encoders.

**Files a transcode run opens.** `strace -f -e
trace=openat,open,access,newfstatat,statx,readlink` of an x265 to MP4 and an
SVT-AV1 to WebM encode under `env -i LC_ALL=C`, on the reference host, showed
outside the Nix store: `/proc/self/status`, `/proc/cpuinfo`,
`/proc/sys/vm/overcommit_memory`, `/sys/devices/system/cpu/possible` and
`online`, `/sys/devices/system/node` and `node0/cpumap` (libnuma, which x265
links, sizing its thread pools), and the Vulkan loader probing
`/run/opengl-driver`, which is refused and unused. A confined transcode run
reads the render run's list plus `/sys/devices/system/node`.

## Memory profiles

The low-memory limits were measured on the small host. At the
high-memory limits (the default `Resources`) an 80 MP image thrashed or was
OOMKilled in most runs (the png8 and lossy WebP outputs each passed once); at
the low-memory limits it completed,
with every lossy format taking 20 to 27 s. Caveats: the measurement is of one
call per container, for a 2048 WebP output; avif stays near 1400 MiB of anonymous
memory whatever the limits; and PNG and lossless WebP inputs took 30 to 35 s.

## Decoding

Reduced decode of a JPEG, a worked example of how `Caps.MaxFramePixels` (here
52 million) judges it: a 73 MP JPEG (7308x10000) is admitted when the largest
`Encoding` reduces the decode to at most six eighths (a `MaxEdge` up to 3750)
and refused at seven eighths (about 55.9 MP) and where nothing reduces it. A
16382x3628 JPEG is admitted at a `MaxEdge` of 2048 and below.

Still WebP, decoded by ImageMagick like any format that cannot be reduced:

- The decode is whole, at the stored size, which is why a WebP's stored size
  is held to the still caps at the header. libwebp's scaling bounds memory
  only for lossy images without alpha, so a reduced decode did not relax the
  cap for lossless or alpha images (268 MP of lossless, an 82 KB file, needed
  about 1 GB to decode at 1024; lossy with alpha about 370 MB; lossy without
  alpha 7 MB). It did keep memory low for lossy images without alpha, which
  now cost the full-size pixel cache too; see the cost below.
- The output is ImageMagick's own decode and resize, pixel for pixel. A
  separate decoder at a reduced size differed by under 1 level of 255 on
  average, because libwebp's scaler and upsampling are not ImageMagick's
  filter, and its orientation and profile handling had to be reproduced
  alongside ImageMagick's.

Animations:

- Memory of the GIF compositor is about one byte per pixel of every frame,
  plus one batch (at most 16 million input pixels, about 256 MiB) in
  ImageMagick. Memory of the animated WebP compositor is two canvases in the
  program plus one batch in ImageMagick, and does not grow with the number of
  frames.
- Against ImageMagick's `-coalesce`, the pixels of an animated WebP composited
  by `webp-frames` are identical except where frames are blended over earlier
  content (blending without disposal), which differ by at most 1 level of
  alpha, 1 or 2 of colour where the result is opaque and 2 where it is partly
  transparent. Canvas that no frame covers is transparent, where ImageMagick
  makes it opaque black if the file declares no alpha. The output of an opaque
  animation can differ in file shape (PNG colour type).

Video:

- The seek margin (sixteen frames, at least half a second) costs, on average,
  that many extra decoded frames per sample, and at worst one more GOP when a
  keyframe falls just inside it: one 720p keyint-300 sample took 0.07 s without
  the margin and 0.70 s with it.
- The seek runs are sequential: one process with all the seeks as separate
  inputs is faster but holds every decoder open at once (14 GB peak on 4K
  HEVC).
- `selectMaxPixels` (1e9 decoded pixels, the largest stream read once with a
  select filter rather than once per seek) was measured on x86 with 8-bit
  H.264 (veryfast, 250-frame GOPs, noisy test content), 15 and 16 samples,
  edge 1024. Select is cheaper in wall and CPU time up to about 1.4e9 pixels
  at 720p, 3.1e9 at 1080p and well beyond 4e9 (not reached) at 4K, and 5 to 20
  times cheaper on short clips, where seeking pays an ffmpeg start for every
  sample. The value is the lowest of the crossovers, rounded down, because
  shorter GOPs make seeks cheaper and 10-bit HEVC was not measured. Both paths
  are decode-bound, so their costs scale together with the decoder's speed and
  the crossover in pixels should carry to a slower machine; a slower disk or a
  startup cost that is larger relative to decoding would move it towards
  seeking.
