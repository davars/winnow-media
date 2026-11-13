# Measurements

The record behind the numbers in the `media` package: what was measured, how,
and what each figure justified. The constants in `media/limits.go`,
`media/sandbox_linux.go` and `media/op_video.go` link here. Figures that are
projections, not measurements, are marked as such.

## How to read this

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
- Address space 8 GiB: 2.4 times the worst peak (3.2 GiB), leaving room for
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
- The helper's rusage includes the tool's, so `Usage` is the tool's: a 48 MP
  HEIC measured 919 MiB and 12.7 s of CPU directly and the same through the
  helper. The helper adds about 13 ms to a run.

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
| 52 MP WebP | 810 MiB | 2660 MiB |

The file-backed part is the pixel cache, which the kernel reclaims under the
limit.

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

Still WebP, decoded by `webp-frames still` with libwebp's scaler:

- libwebp's scaling bounds memory only for lossy images without alpha. 268 MP
  of lossless, an 82 KB file, needs about 1 GB to decode at 1024; lossy with
  alpha about 370 MB; lossy without alpha 7 MB. The scaling is a gain in time
  and memory for those, not a cap relaxation, which is why a WebP's stored
  size is held to the still caps at the header.
- Pixels differ from ImageMagick's own decode and resize by under 1 level of
  255 on average, because libwebp's scaler and upsampling are not its filter.

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
