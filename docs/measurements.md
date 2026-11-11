# Measurements

The caps, ceilings and limits of the `media` package were set from
measurements of the largest inputs they admit. This file records the numbers,
how they were taken and what each justified. The constants that state them
(`media/limits.go`, `media/sandbox_linux.go`, `media/op_video.go`) link here.
Figures that are projections, not measurements, are marked as such.

## Method

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

Two hosts were used. The reference host is a 12-CPU x86_64 machine. The
Raspberry Pi 5 figures come from the three Raspberry Pi 5 nodes of the first
consumer's service, which runs in its low-memory configuration; the decision
and its measurements are also in that consumer's repository, in
`docs/plans/archive/2026-11-07_unified-pipeline.md` of `davars/winnow`.

## Caps and ceilings

On the reference host, with `measure`:

- A 25 MiB 4K 60 fps video, 10-bit HEVC (HLG or PQ) or H.264, takes about
  2.5 minutes with 64 samples and uses up to 2.7 times its wall time in CPU,
  since the decoder and filters are bound to 4 threads. Its peak, 2.8 GiB, is
  ImageMagick encoding the samples; ffmpeg stays near 300 MiB.
- A 48 MP HEIC takes 8 s and 0.9 GiB.
- The limit of 64 samples per call is the largest count the caps were
  measured with. A caller with a time budget chooses a smaller count from the
  fields of `ProbeVideo`.

On the three Raspberry Pi 5 nodes, the still caps (a side of 16384 and 52
million pixels):

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
- The caps `maxReturnedPixels` (64 million) and `maxDecodedPixels` (900
  million) keep an animation within a 30 s budget per call on a Raspberry Pi
  5. The worst admitted GIF, 225 frames of 2000x2000 with 16 returned (64 MP
  returned, 900 MP decoded), took 27.8 s there, and 16 frames of 2000x2000
  alone took 20.2 s. A returned megapixel costs about 0.28 s and a decoded
  one about 0.01 s.

ImageMagick policy ceilings:

- The memory and map ceilings are those of the memory profile (1 and 2 GiB for
  `HighMemory`); the disk ceiling is 4 GiB whatever the profile.
- The time ceiling is at least four times the longest ImageMagick run
  measured to complete (36 s). The ffmpeg allocation limit is four times what
  an 8K frame needs (1 GiB; a 4K HDR frame needs 32 MiB, one float plane of the
  tone-mapping filters).
- The library sets no deadline on a whole operation: a caller that wants one
  passes a context.

Raspberry Pi 5 against the reference host: at worst about 4 times. CPU was 4.1
times for 4K HEVC decode, and wall time 4.0 times for two 8000x8000 GIF frames
(a disk-bound run); peak memory was the same. A 4K HEVC video takes 8 minutes
on the Raspberry Pi 5.

Decoder threads: an 8K 10-bit HLG source peaked at 1811 MiB with 4 ffmpeg
threads and 1280 MiB with 2, for 28% more time. Sources above 4096x2160 are
therefore sampled with 2 threads and smaller ones with 4. The HEVC decoder
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
- `RLIMIT_NPROC` counts every task of the real user, so it is set relative to
  the count when the helper starts and bounds forks only where the user has
  nothing else running, such as a container with its own uid; it is not
  enforced for root.
- The helper's rusage includes the tool's, so `Usage` is the tool's: a 48 MP
  HEIC measured 919 MiB and 12.7 s of CPU directly and the same through the
  helper. The helper adds about 13 ms to a run.

## Deployment model

The measured model is one request per pod:

- a memory limit of 1536Mi per request in flight, with the `LowMemory`
  profile;
- scratch on a disk-backed `emptyDir` (never `medium: Memory`) with a
  `sizeLimit` of about 8Gi, holding ImageMagick's temporary directory and the
  processor's scratch. ImageMagick's pixel cache is memory-mapped there, so
  its pages must be reclaimable under the memory limit;
- an `ephemeral-storage` request covering the volume;
- real disk on every node that can run the pod, since reclaiming writes those
  pages back.

Landlock is not used because the Raspberry Pi 5 nodes this was built for have
none; file confinement rests on the container, as the package documentation
requires of a deployment.

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

`LowMemory` was measured on 1536Mi Raspberry Pi 5 pods. At the `HighMemory`
limits an 80 MP image thrashed or was OOMKilled in most runs (the png8 and
lossy WebP outputs each passed once); at the `LowMemory` limits it completed,
with every lossy format taking 20 to 27 s. Caveats: the measurement is of one
request per pod, for a 2048 WebP output; avif stays near 1400 MiB of anonymous
memory whatever the profile; and PNG and lossless WebP inputs took 30 to 35 s.

## Decoding

Reduced decode of a JPEG (`jpeg:size`), a worked example of how the still caps
judge it: a 73 MP JPEG (7308x10000) is admitted when the largest target
reduces it to at most six eighths (a target up to 3750 px) and refused at
seven eighths (about 55.9 MP) and where nothing reduces it. A 16382x3628 JPEG
is admitted at targets of 2048 and below.

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
- The select path against the seek path is measured in the comment of
  `selectMaxPixels` in `media/op_video.go`.
