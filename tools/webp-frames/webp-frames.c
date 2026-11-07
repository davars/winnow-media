/*
 * webp-frames: write selected, composited frames of an animated WebP.
 *
 *   webp-frames INPUT OUTDIR IN_FLIGHT WIDTH HEIGHT FRAMES INDEX[,INDEX...] TIMEOUT_MS
 *
 * INPUT is decoded with libwebp's animation decoder, which composites each
 * frame onto the canvas (blending and disposal included) and holds one canvas
 * and one saved copy, however many frames the file has. WIDTH, HEIGHT and
 * FRAMES are what the caller read from the file's header; the file must agree.
 * INDEX lists the frames to write, strictly ascending, each below FRAMES.
 *
 * Each selected frame is written to OUTDIR as .sample-NNN.pam, NNN counting
 * the written frames from 0: a full-canvas PAM image with straight (not
 * premultiplied) alpha, of tuple type RGB_ALPHA. If every frame of the file
 * is opaque and covers the whole canvas, no pixel can be transparent and the
 * tuple type is RGB instead, which ImageMagick resamples as it does a still
 * without alpha. Canvas that no frame covers is transparent, as libwebp and
 * browsers have it. The file is written as
 * .sample-NNN.pam.tmp and renamed, so a reader never sees a partial one. Then
 * a line "NNN P A" is printed on standard output and flushed, P being the id 1
 * when the file carries an ICC profile and 0 otherwise, and A being 1 when the
 * file declares alpha (the VP8X alpha flag, or a frame with an alpha channel)
 * and 0 otherwise. A is what the file says, not whether a pixel is
 * transparent: it is the same for every frame. The profile, if any, is written
 * to OUTDIR/.profile-1.icc before the first frame.
 *
 * Before writing frame NNN the tool waits until .sample-(NNN-IN_FLIGHT).pam
 * no longer exists, so the reader's deleting the frames it has consumed bounds
 * how many are on disk. If that takes longer than TIMEOUT_MS the tool fails. Decoding stops after the last selected frame.
 *
 * Any failure prints a message on standard error and exits 1.
 */
#include <errno.h>
#include <limits.h>
#include <stdarg.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <time.h>
#include <unistd.h>

#include <webp/decode.h>
#include <webp/demux.h>
#include <webp/mux_types.h>

#define MAX_FILE_BYTES ((size_t)256 << 20)
#define MAX_CANVAS_PIXELS ((uint64_t)1 << 28)
#define MAX_FRAMES 100000
#define PROFILE_NAME ".profile-1.icc"

static void fail(const char *fmt, ...) __attribute__((noreturn, format(printf, 1, 2)));

static void fail(const char *fmt, ...) {
  va_list ap;
  fputs("webp-frames: ", stderr);
  va_start(ap, fmt);
  vfprintf(stderr, fmt, ap);
  va_end(ap);
  fputc('\n', stderr);
  exit(1);
}

/* parse_count parses s as a decimal integer in [min, max]. */
static long parse_count(const char *what, const char *s, long min, long max) {
  char *end;
  long v;
  if (s[0] < '0' || s[0] > '9') fail("%s %s is not a count", what, s);
  errno = 0;
  v = strtol(s, &end, 10);
  if (errno != 0 || *end != '\0') fail("%s %s is not a count", what, s);
  if (v < min || v > max) fail("%s %s is outside %ld..%ld", what, s, min, max);
  return v;
}

/* read_file reads the whole of path, refusing one over MAX_FILE_BYTES. */
static uint8_t *read_file(const char *path, size_t *size) {
  FILE *f = fopen(path, "rb");
  struct stat st;
  uint8_t *buf;
  if (f == NULL) fail("opening %s: %s", path, strerror(errno));
  if (fstat(fileno(f), &st) != 0) fail("stat %s: %s", path, strerror(errno));
  if (!S_ISREG(st.st_mode)) fail("%s is not a regular file", path);
  if (st.st_size <= 0 || (uint64_t)st.st_size > MAX_FILE_BYTES) {
    fail("%s is %lld bytes, want 1..%zu", path, (long long)st.st_size, MAX_FILE_BYTES);
  }
  *size = (size_t)st.st_size;
  buf = malloc(*size);
  if (buf == NULL) fail("out of memory reading %s", path);
  if (fread(buf, 1, *size, f) != *size) fail("reading %s: short read", path);
  fclose(f);
  return buf;
}

static void join(char *out, size_t cap, const char *dir, const char *name) {
  if (snprintf(out, cap, "%s/%s", dir, name) >= (int)cap) fail("path too long: %s/%s", dir, name);
}

/* write_pam writes the RGBA canvas to tmp as a PAM, with its alpha channel
 * unless alpha is 0, then renames it to final. row is w*3 bytes of scratch. */
static void write_pam(const char *tmp, const char *final, const uint8_t *rgba, int w, int h, int alpha, uint8_t *row) {
  FILE *f = fopen(tmp, "wb");
  int ok;
  if (f == NULL) fail("creating %s: %s", tmp, strerror(errno));
  ok = fprintf(f, "P7\nWIDTH %d\nHEIGHT %d\nDEPTH %d\nMAXVAL 255\nTUPLTYPE %s\nENDHDR\n", w, h, alpha ? 4 : 3,
               alpha ? "RGB_ALPHA" : "RGB") >= 0;
  if (alpha) {
    ok = ok && fwrite(rgba, 1, (size_t)w * (size_t)h * 4, f) == (size_t)w * (size_t)h * 4;
  } else {
    for (int y = 0; ok && y < h; y++) {
      const uint8_t *in = rgba + (size_t)y * (size_t)w * 4;
      for (int x = 0; x < w; x++) memcpy(row + 3 * (size_t)x, in + 4 * (size_t)x, 3);
      ok = fwrite(row, 1, (size_t)w * 3, f) == (size_t)w * 3;
    }
  }
  if (!ok || fflush(f) != 0) {
    unlink(tmp);
    fail("writing %s: %s", tmp, strerror(errno));
  }
  if (fclose(f) != 0) {
    unlink(tmp);
    fail("writing %s: %s", tmp, strerror(errno));
  }
  if (rename(tmp, final) != 0) {
    unlink(tmp);
    fail("renaming %s: %s", tmp, strerror(errno));
  }
}

/* write_profile writes the file's ICC profile and reports whether it has one. */
static int write_profile(WebPAnimDecoder *dec, const char *dir) {
  const WebPDemuxer *demux = WebPAnimDecoderGetDemuxer(dec);
  WebPChunkIterator it;
  char path[PATH_MAX];
  FILE *f;
  int ok;
  if (demux == NULL) fail("no demuxer");
  if (!(WebPDemuxGetI(demux, WEBP_FF_FORMAT_FLAGS) & ICCP_FLAG)) return 0;
  if (!WebPDemuxGetChunk(demux, "ICCP", 1, &it)) return 0;
  join(path, sizeof path, dir, PROFILE_NAME);
  f = fopen(path, "wb");
  if (f == NULL) fail("creating %s: %s", path, strerror(errno));
  ok = fwrite(it.chunk.bytes, 1, it.chunk.size, f) == it.chunk.size;
  ok = fclose(f) == 0 && ok;
  WebPDemuxReleaseChunkIterator(&it);
  if (!ok) fail("writing %s: %s", path, strerror(errno));
  return 1;
}

/* is_opaque reports whether every frame is opaque and covers the whole canvas,
 * so that no composited pixel is transparent. */
static int is_opaque(WebPAnimDecoder *dec, int width, int height) {
  const WebPDemuxer *demux = WebPAnimDecoderGetDemuxer(dec);
  WebPIterator it;
  int opaque = 1;
  if (demux == NULL) fail("no demuxer");
  if (!WebPDemuxGetFrame(demux, 1, &it)) fail("reading the frame list");
  do {
    if (it.has_alpha || it.x_offset != 0 || it.y_offset != 0 || it.width != width || it.height != height) opaque = 0;
  } while (opaque && WebPDemuxNextFrame(&it));
  WebPDemuxReleaseIterator(&it);
  return opaque;
}

/* declares_alpha reports whether the file declares alpha: the VP8X alpha flag,
 * or any frame with an alpha channel. */
static int declares_alpha(WebPAnimDecoder *dec) {
  const WebPDemuxer *demux = WebPAnimDecoderGetDemuxer(dec);
  WebPIterator it;
  int declared;
  if (demux == NULL) fail("no demuxer");
  declared = (WebPDemuxGetI(demux, WEBP_FF_FORMAT_FLAGS) & ALPHA_FLAG) != 0;
  if (!declared && WebPDemuxGetFrame(demux, 1, &it)) {
    do {
      if (it.has_alpha) declared = 1;
    } while (!declared && WebPDemuxNextFrame(&it));
    WebPDemuxReleaseIterator(&it);
  }
  return declared;
}

static long long now_ms(void) {
  struct timespec ts;
  if (clock_gettime(CLOCK_MONOTONIC, &ts) != 0) fail("reading the clock: %s", strerror(errno));
  return (long long)ts.tv_sec * 1000 + ts.tv_nsec / 1000000;
}

/* wait_consumed waits until path, frame number n, no longer exists, and fails
 * if that takes more than timeout_ms. */
static void wait_consumed(const char *path, long n, long timeout_ms) {
  const struct timespec pause = {0, 2 * 1000 * 1000};
  const long long deadline = now_ms() + timeout_ms;
  struct stat st;
  while (stat(path, &st) == 0) {
    if (now_ms() > deadline) fail("frame %ld not consumed in %ld ms", n, timeout_ms);
    nanosleep(&pause, NULL);
  }
  if (errno != ENOENT) fail("checking %s: %s", path, strerror(errno));
}

int main(int argc, char **argv) {
  const char *input, *dir;
  long in_flight, width, height, frames, timeout_ms;
  long *selected;
  size_t count = 0, next = 0;
  char *list, *tok, *save = NULL;
  uint8_t *data;
  size_t size;
  WebPAnimDecoderOptions opts;
  WebPAnimDecoder *dec;
  WebPAnimInfo info;
  WebPData wd;
  int profiled, alpha, declared;
  uint8_t *row;
  long ordinal = 0;

  if (argc != 9) {
    fail("usage: webp-frames INPUT OUTDIR IN_FLIGHT WIDTH HEIGHT FRAMES INDEX[,INDEX...] TIMEOUT_MS (%d arguments)",
         argc - 1);
  }
  input = argv[1];
  dir = argv[2];
  in_flight = parse_count("in-flight count", argv[3], 1, 1 << 20);
  width = parse_count("width", argv[4], 1, 16777216);
  height = parse_count("height", argv[5], 1, 16777216);
  frames = parse_count("frame count", argv[6], 1, MAX_FRAMES);
  timeout_ms = parse_count("timeout", argv[8], 1, 24L * 3600 * 1000);
  if ((uint64_t)width * (uint64_t)height > MAX_CANVAS_PIXELS) fail("canvas %ldx%ld is too large", width, height);

  selected = calloc((size_t)frames, sizeof *selected);
  list = strdup(argv[7]);
  if (selected == NULL || list == NULL) fail("out of memory");
  for (tok = strtok_r(list, ",", &save); tok != NULL; tok = strtok_r(NULL, ",", &save)) {
    long v = parse_count("selected frame", tok, 0, frames - 1);
    if (count > 0 && v <= selected[count - 1]) fail("selected frames are not strictly ascending at %ld", v);
    selected[count++] = v;
  }
  if (count == 0 || argv[7][0] == ',' || argv[7][strlen(argv[7]) - 1] == ',' || strstr(argv[7], ",,") != NULL) {
    fail("selected frames %s are not a list", argv[7]);
  }

  data = read_file(input, &size);
  if (!WebPAnimDecoderOptionsInit(&opts)) fail("incompatible libwebp version");
  opts.color_mode = MODE_RGBA;
  opts.use_threads = 0;
  wd.bytes = data;
  wd.size = size;
  dec = WebPAnimDecoderNew(&wd, &opts);
  if (dec == NULL) fail("%s is not a decodable animated WebP", input);
  if (!WebPAnimDecoderGetInfo(dec, &info)) fail("reading the animation's header");
  if (info.canvas_width != (uint32_t)width || info.canvas_height != (uint32_t)height ||
      info.frame_count != (uint32_t)frames) {
    fail("file holds %u frames of %ux%u, the header read found %ld of %ldx%ld", info.frame_count, info.canvas_width,
         info.canvas_height, frames, width, height);
  }
  profiled = write_profile(dec, dir);
  alpha = !is_opaque(dec, (int)width, (int)height);
  declared = declares_alpha(dec);
  row = malloc((size_t)width * 3);
  if (row == NULL) fail("out of memory");

  for (long i = 0; next < count; i++) {
    uint8_t *canvas;
    int timestamp;
    if (!WebPAnimDecoderHasMoreFrames(dec)) fail("file ended after %ld of %ld frames", i, frames);
    if (!WebPAnimDecoderGetNext(dec, &canvas, &timestamp)) fail("decoding frame %ld failed", i);
    if (i != selected[next]) continue;
    char tmp[PATH_MAX], final[PATH_MAX], name[64], older[PATH_MAX];
    if (ordinal >= in_flight) {
      snprintf(name, sizeof name, ".sample-%03ld.pam", ordinal - in_flight);
      join(older, sizeof older, dir, name);
      wait_consumed(older, ordinal - in_flight, timeout_ms);
    }
    snprintf(name, sizeof name, ".sample-%03ld.pam", ordinal);
    join(final, sizeof final, dir, name);
    snprintf(name, sizeof name, ".sample-%03ld.pam.tmp", ordinal);
    join(tmp, sizeof tmp, dir, name);
    write_pam(tmp, final, canvas, (int)width, (int)height, alpha, row);
    if (printf("%ld %d %d\n", ordinal, profiled, declared) < 0 || fflush(stdout) != 0) fail("writing standard output");
    ordinal++;
    next++;
  }
  WebPAnimDecoderDelete(dec);
  free(row);
  free(data);
  free(selected);
  free(list);
  return 0;
}
