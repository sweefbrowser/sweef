# Media playback and adaptive streaming

How `<video>`, `<audio>` and Media Source Extensions (MSE) work in Sweef, what was measured, what works on real
sites and what does not. Everything here is pure Go (`CGO_ENABLED=0`): the demuxers, the decoders, the colour
conversion and the audio output.

Measured on a 4 vCPU shared Linux VM (Go 1.26, load average about 1 while measuring), 2026-10-13. Numbers from
another machine will differ; the methods are at the end so they can be repeated.

## Architecture

```
page JS: HTMLMediaElement / MediaSource        internal/engine/media_prelude.js
   |  playerOpen / Play / Pause / Seek / Rate / Volume / ...    (commands)
   |  playerClock / playerStats                                  (reads)
   v
mediaPlayer (one per element)                  internal/engine/mediaplayer.go
   |-- audio loop: TrackStore audio samples -> AudioDecoder -> audioout.Stream      (the master clock)
   |-- video loop: TrackStore video samples -> VideoDecoder (H.264: frame threads) -> picture -> the element's Bitmap
   v
TrackStore (sorted coded frames)               internal/media/track.go
   ^-- progressive file: OpenTracks / OpenTrackSet (mp4, webm direct; mp3 indexed; wav, flac, ogg, aac decoded to PCM chunks)
   ^-- MSE: MSEBuffer = fMP4 / WebM segment parsers + coded frame processing       internal/media/mse*.go
```

* **The audio clock is the master.** `currentTime` is the position of the element's audio stream (frames the device
  has played, minus the output latency). The video loop shows the picture due at that time. With no audio track, or
  after the audio ended, a wall clock takes over. See [audio-output.md](audio-output.md) for the stream, mixer and
  tab mute.
* The JS side keeps the element state machine (readyState, events, `seeking`, `buffered`, ...); it asks the player
  for the clock and tells it what the element wants. The player never blocks the page: decoding runs on its own
  goroutines and pictures are published to the paint path (`Doc.TouchPaint`).
* A `<video>` always emits a bitmap op when painted, because its picture arrives after layout.

### Formats

| | |
|---|---|
| Containers | MP4/fMP4/M4A, WebM/Matroska, Ogg, WAV, FLAC, MP3, ADTS AAC |
| Video | H.264 (baseline to High, CABAC and CAVLC, B-frames), HEVC, VP8, VP9 (profile 0 to 3, tiles), AV1 |
| Audio | AAC (LC, HE), Opus, Vorbis, MP3, PCM; FLAC in files and `<audio>` (not in MSE) |
| MSE `isTypeSupported` | `video/mp4`, `audio/mp4`, `video/webm`, `audio/webm` with codecs from the tables above (FLAC in MSE is claimed by the type tables but has no streaming decoder); at most one video and one audio codec per SourceBuffer, a type without `codecs` is refused |

`navigator.mediaCapabilities.decodingInfo()` reports `supported` from the same tables and `smooth` from a
pixel-rate budget per codec (H.264 63 Mpx/s and VP9 63 Mpx/s = 1080p30, AV1 and HEVC 14 Mpx/s; H.264 was 28 Mpx/s =
720p30 before the frame threads below), chosen from the measurements below so that players such as YouTube, Shaka and dash.js pick a rendition the machine can play.
`powerEfficient` is always false (software decoding). `keySystemConfiguration` is never supported.

### Autoplay policy

Muted video (or volume 0, or a video without an audio track) may start without a gesture. Everything else needs
user activation, otherwise `play()` rejects with `NotAllowedError`. Activation comes from real input only: click,
mouse down/up, key, touch (mouse movement does not count). Muted `<audio>` is refused, as in Chromium. An
`AudioContext` created before activation stays `suspended`.

### MSE: what is implemented

* `MediaSource` states and events (`sourceopen`, `sourceended`, `sourceclose`), `duration` rules, `endOfStream`
  (including `network`/`decode` errors), `setLiveSeekableRange`, `isTypeSupported`.
* `SourceBuffer`: `appendBuffer` with an asynchronous update (`updatestart`, `update`, `updateend`, `error`),
  `abort`, `remove`, `changeType`, `mode` (`segments` and `sequence`), `timestampOffset`, `appendWindowStart/End`,
  `buffered`, `updating`. The segment parsers are incremental (fragments may arrive in any chunking). Init
  segments are validated against the type; a bad segment raises `error` and ends the stream with a decode error.
* Coded frame processing per the spec: timestamp offset, append window trimming, discontinuity detection, the
  random-access-point flag, overlapping frames replaced, `buffered` merged with a small gap tolerance (the first
  range starting within a second of 0 is treated as starting at 0, as Chromium does for audio priming).
* Quota and eviction: 150 MB for video and 12 MB for audio per SourceBuffer (`SWEEF_MSE_QUOTA` overrides, for tests).
  An append that does not fit evicts behind the playhead first, whole GOPs at a time; if that is not enough
  `appendBuffer` throws `QuotaExceededError`.
* Seeking inside the buffered range is immediate; seeking outside waits (`readyState` drops to `HAVE_METADATA`,
  `waiting`) and resumes when data arrives. `readyState` follows what is buffered at the playhead
  (`HAVE_CURRENT_DATA` to `HAVE_ENOUGH_DATA`) and playback stalls and resumes when the buffer runs dry and
  refills. Quality switches (a new init segment with a different configuration, in the same or a new
  SourceBuffer) reconfigure the decoder at the next sample.
* Tests build fMP4 H.264+AAC in two qualities, WebM VP9+Opus and fMP4 AV1 fixtures with ffmpeg
  (`internal/engine/testdata/media/mse/gen.sh`, a few tens of KB, committed) and assert the buffered ranges, the
  pixels of the picture shown, the PCM that reaches the audio sink, seeking, quality switch and end of stream
  (`internal/engine/mse_play_test.go`, `internal/media/mse_test.go`, `mse_decode_test.go`).

### Not implemented

* `ManagedMediaSource` and `MediaSource` in dedicated workers (`MediaSource.canConstructInDedicatedWorker` is
  false, `MediaSourceHandle` does not exist). Skipped on purpose; players fall back to window MSE.
* `SourceBuffer.audioTracks/videoTracks/textTracks`, `appendBufferAsync`/`removeAsync`, `stalled` events for MSE.
* **EME / DRM: Clear Key only.** `requestMediaKeySystemAccess('org.w3.clearkey', ...)` works and `MediaKeys`,
  `MediaKeySession`, `setMediaKeys`, the `encrypted` and `waitingforkey` events and key status reporting are real; MSE
  playback decrypts `cenc`, `cens`, `cbc1` and `cbcs` fragmented MP4 (`internal/media/cenc.go`, `internal/engine/eme.go`).
  Widevine, PlayReady and FairPlay need licensed content decryption modules and reject with `NotSupportedError`, so
  Netflix, Disney+, Prime Video, YouTube Premium music/movies and every other DRM-protected stream **cannot be played**.
  Not done: persistent sessions and licenses (`temporary` only), `load()`/`remove()`, server certificates, a robustness
  level above the empty string (there is no secure path), encrypted WebM (the `webm` init data type is accepted but the
  WebM demuxer does not decrypt), encrypted audio-only tracks were not tested end to end (the code path is shared),
  and a plain `src=` mp4 that is encrypted (only MSE is wired). Key IDs of a track whose license never arrives stall
  playback and fire `waitingforkey`; there is no `keystatuseschange` expiry because Clear Key licenses do not expire.
* Pitch-preserving time stretch: `playbackRate` resamples (the pitch changes). `preservesPitch` is accepted and
  ignored. Audio is muted for rates outside 0.25 to 4; video follows the rate.
* Picture-in-picture, Remote Playback, captions rendering from MSE text tracks, HLS (hls.js uses MSE and works in
  principle; it was not tested here), WebRTC audio (the `audioout` API is ready for it).
* The real sound card path (`oto`) is compile-checked on Linux, Windows and macOS but was never run on hardware in
  this work; every measurement here used the null sink, which keeps the audio clock running in real time.

## Decode speed

Method: `internal/media/decodebench_test.go` decodes every frame in order and reports the frames per second of the
whole run (the pictures of a decoder with frame threads are waited for) and the process CPU time per frame (all
goroutines). Clips are 20 s of synthetic `testsrc2` plus temporal noise (harder to compress than most real
content), a key frame every 2 s, B-frames for H.264; the `ffmpeg` command lines are at the end. Old and new test binaries were run alternately, five times each, on the same 4 vCPU shared VM (other
processes were running: load average 2.4 to 5 during these runs), 2026-10-14. Medians, with the range in brackets:

| Clip | before: fps | before: CPU ms/frame | after: fps | after: CPU ms/frame |
|---|---|---|---|---|
| H.264 High 720p30, 3 Mbit/s | 54 (48 to 56) | 23.5 (22.9 to 25.9) | 167 (143 to 181) | 15.4 (14.6 to 18.4) |
| H.264 High 1080p30, 6 Mbit/s | 24.8 (22.8 to 27.9) | 50.7 (47.0 to 56.9) | 97 (50 to 123) | 33.1 (29.2 to 35.7) |
| H.264 High 1080p60, 8 Mbit/s | 28.3 (23.1 to 29.6) | 46.7 (45.5 to 52.9) | 130 (66 to 138) | 27.2 (26.0 to 31.4) |
| VP9 1080p30, 4 Mbit/s | 48 (41 to 52) | 56.3 (52.5 to 63.7) | 48 (44 to 64) | 43.7 (43.6 to 53.6) |

The H.264 numbers vary a lot between runs because the frame threads take whatever cores the other processes leave
(the low runs are the ones that overlapped with other work); the CPU time per frame is steadier. Real time needs
the clip's own rate (30 or 60 fps) with headroom for the colour conversion and the page.

What was optimised, from profiles:

* **H.264 frame threads** (`internal/media/h264/threads.go`). The calling goroutine reads the NAL units, parses the
  slice headers, builds the reference lists and does the reference marking; the macroblock data of each progressive
  4:2:0 frame goes to a goroutine of its own (up to one per core, at most four). That goroutine decodes the
  macroblocks, deblocks each row as soon as the row below it is decoded (intra prediction of that row still needs the
  unfiltered samples) and publishes how many luma rows of the picture are final. A picture that refers to it waits only
  for the rows its motion vectors reach (`mcRect`; the colocated macroblock row for direct prediction), not for
  the whole picture. Pictures that cannot be threaded (interlaced, MBAFF, slice groups, data partitioning, SP/SI,
  separate colour planes, other chroma formats) wait for everything in flight and decode on the calling goroutine
  as before. The pictures are bit-identical to the plain decoder's on every H.264 stream in the repository
  (`TestH264FrameThreadsMatchSerial` with 2, 3 and 4 threads; also under `-race`, also on streams with random bit
  flips, `TestH264FrameThreadsDamagedStreams`). `DecodeFrame` returns the frame before its last rows are done;
  `yuv.Frame.Wait` (called by `ToYCbCr`, `RGBA` and the player before it shows a picture) blocks until they are.
* **Deblocking**: boundary strengths from packed motion records and partition shape (a 16x16 macroblock has no
  motion edges inside), luma and chroma sample filters specialised for the edge direction on row slices without
  bounds checks (`deblock_fast.go`; the old filter is still the path for MBAFF, 4:2:2 and 4:4:4 and the reference in
  `TestH264FastDeblockMatchesReference`). The wavefront deblocking of the plain decoder and of VP9 now sleeps on a
  condition variable after a short spin (`internal/media/wavefront`) instead of yielding in a loop, which burned the
  CPU the other rows needed whenever the machine was busy (46% of the VP9 profile on a loaded machine).
* **Motion compensation**: one loop per quarter-sample position with all taps hoisted and no bounds checks (the
  row slices are shifted views of one length), a single prediction without weights (or with the identity weight
  x264 sends for unchanged planes) written straight into the picture, fixed-width row copies as 64-bit moves on
  amd64 and arm64 (a 32-byte array assignment compiles to a call to `memmove`), bi-prediction averaging on slices
  of one length. `TestMCMatchesReference` compares every kernel with the straightforward version for all
  fractions, block sizes, 8 and 10 bit and picture edges.
* **CABAC**: the offset register is kept scaled with 32 bits read ahead, so a renormalisation is a subtraction,
  and each context transition is one table lookup (`cabac.go`, `TestCABACMatchesReference`); motion write-back
  by rows. Together a few percent.
* **Display conversion**: H.264 and VP9 pictures are no longer narrowed to an `image.YCbCr` when they are decoded; they
  are converted to RGBA when (and only when) they are shown, straight from the decoder's planes, two rows per
  chroma row, with a table clamp (`internal/media/yuvrgba.go`, equal to the old two-step conversion on every
  picture of the corpus). A picture the player skips is never converted.
* **VP9**: loop filter per edge segment with the thresholds hoisted and the 8- and 16-sample filters written out
  (`TestFilterSegmentMatchesReference`), inter prediction written into the frame, no bounds checks in the
  horizontal filter, the blocking wavefront above. CPU time per frame 56 to 44 ms; the frame rate is bound by the
  serial parts (VP9 has tile columns and a parallel loop filter but no frame threads: backward probability
  adaptation makes the frames depend on each other's entropy decoding), 48 fps against the 30 that 1080p30 needs.

What is still slow or unmeasured:

* The kernels are scalar Go (no SIMD, no assembly: there was no arm64 machine or emulator to test it on, and the
  scalar code is the same on every architecture except the row copies). Vector motion compensation and
  deblocking would take another fifth or so off the H.264 decoding CPU, and a vector colour conversion most of
  the 3.5 ms per 1080p picture that is left.
* The decoders keep 16-bit samples for every bit depth, which doubles the memory traffic of the 8-bit streams that
  make up almost everything on the web. Copying the unchanged blocks of a static scene (skipped macroblocks) is now
  one of the largest items of the H.264 profile.
* Frame threads need progressive 4:2:0 frames; interlaced, MBAFF and the other formats decode on one goroutine
  (with the wavefront deblocking filter). VP9, AV1 and HEVC have no frame threads.
* The numbers above are for four vCPUs. The `mediaCapabilities` budget for H.264 (63 Mpx/s) assumes that; on two
  cores 1080p30 was not measured. 1080p60 is smooth only while the VM is not busy.
* The clips are synthetic. They are harder to compress than most video but have less motion than a film; real
  content shifts work from skipped macroblocks to fractional-sample prediction.

## Display path: colour conversion and scaling

Decoding is not the only per-frame cost: the picture has to be converted to RGBA and drawn scaled into the
element's box. Per 1080p picture (`go test -bench . ./internal/raster ./internal/media`, 4 vCPU, load average about 2,
2026-10-14, three runs each):

| Step | before | after |
|---|---|---|
| Plane conversion to RGBA (`BenchmarkConvert1080p`), 4 goroutines | 7.6 to 8.8 ms (copy to YCbCr, then convert) | 3.4 to 3.8 ms (straight from the decoder's planes) |
| the same on one core | 16 ms | 8 ms |
| Drawing the RGBA frame into a 1920x1080 box (`BenchmarkDrawVideoFrame*`) | 28 to 30 ms | 0.55 to 0.58 ms |
| into a 1280x720 box | 19 to 22 ms | 2.5 to 3.4 ms |
| into a 640x360 box | 6.5 to 7.8 ms | 0.5 to 0.8 ms |

The old drawing path copied the frame into a surface, built a coverage mask for the rectangle, sampled every pixel
through the general image shader with floating-point maths and blended it over the page. A decoded video frame is
opaque and drawn once per frame into a pixel aligned box, so `raster.Context.DrawImage` now has a path for that
case (`image_fast.go`): the frame is used where it is (`Surface.Opaque`, `FromRGBAOpaque`; the engine's frames
implement `OpaqueFrame`), the per-column and per-row parts of the bilinear sample are computed once, and the
samples are stored straight into the destination (a pixel for pixel copy when the box has the video's size). The
arithmetic is the shader's, so the pixels are identical (`TestDrawImageFastMatchesGeneral`, which compares the two
paths for up and down scaling, clips, partial source rectangles, device scales, and the cases that must take the
general path). The page background no longer goes through the blend loop either: an opaque colour over everything
replaces the pixels (`fillRowSolid`); that loop was 17% of the CPU in a profile of fullscreen playback.

Through the whole page (`SWEEF_BENCH_PAINT=WxH`: the video fills a page of that size and the test paints it
incrementally, 60 times a second, the way the window does), the time of one page paint with a 1080p picture:

| Box | before: median / p95 | after: median / p95 |
|---|---|---|
| 1280x720 | 39.8 / 69.4 and 50.7 / 104.0 ms | 9.3 / 15.4 and 11.9 / 32.0 ms |
| 1920x1080 | 60.3 / 95.3 and 86.7 / 159.4 ms | 8.1 / 14.1 and 8.2 / 18.9 ms |

(two runs each, load average 3 to 12; the "after" paint includes the conversion of the picture). The paint at
1920x1080 is one conversion, a copy of the frame into the page and the copy of the damaged area for the window.

## Playback test: 20 s through the whole page path

`TestPlaybackQuality` plays a muted looping clip in a real tab for 20 s (decoder, picture delivery, the page's
`getVideoPlaybackQuality()`). The first table is from 2026-10-13 at load average about 1 and is kept for
reference. The second is from 2026-10-14: the old and the new binary alternately, same clips, the **load average
of the shared VM noted for each run (3 to 15, other processes were running; the VM has 4 vCPUs)**, which is why
the old numbers are worse than the 2026-10-13 ones for the same code:

| Clip | frames reported | dropped | picture interval median / p95 / max | picture lateness p95 / max | CPU (one core = 100%) |
|---|---|---|---|---|---|
| H.264 720p30 | 600 | 6 (1.0%) | 33.3 / 35.7 / 157 ms | 3 / 179 ms | 71% |
| VP9 720p30 | 601 | 2 (0.3%) | 33.4 / 35.7 / 93 ms | 3 / 46 ms | 83% |
| H.264 1080p30 | 597 | 52 (8.7%) | 33.7 / 55.0 / 222 ms | 365 / 520 ms | 129% |
| VP9 1080p30 | 601 | 3 (0.5%) | 33.3 / 36.2 / 104 ms | 6 / 78 ms | 159% |

Dropped frames of every run (load average at the start in brackets), no page paint unless noted:

| Clip | before | after |
|---|---|---|
| H.264 720p30 | 21.4% (10.1), 1.7% (7.0) | 1.0% (15.4), 0.3% (5.9) |
| H.264 1080p30 | 21.4% (3.6), 52.9% (4.9), 43.4% (5.7) | 0.5% (3.6), 0.3% (7.0), 12.2% (7.0) |
| H.264 1080p60 | 66.6% (11.5), 59.1% (5.6) | 35.6% (14.0), 1.8% (4.3) |
| H.264 1080p30, painted into 1280x720 | 59.2% (10.0), 39.8% (5.1) | 7.0% (12.4), 0.0% (3.6) |
| H.264 1080p30, painted into 1920x1080 | 68.0% (11.2), 40.0% (5.5) | 0.0% (10.9), 0.0% (3.0) |
| VP9 1080p30 | 54.8% (14.0), 2.5% (5.6) | 1.8% (8.9), 0.5% (2.6) |

An earlier pair of runs, load average 3 to 4, 1080p30: 8.9% and 8.4% before (the 8.7% above), 1.0% and 0.0% after.
CPU of 1080p30 without painting: 108 to 131% of a core before, 95 to 101% after. With the page painted at 60 Hz
(decoding, conversion, paint and the rest of the page) it was 186 and 245% (1280x720 box) and 157 and 238%
(1920x1080) before, 182 and 194% and 156 and 170% after: not lower, because the old code was dropping 40 to 68%
of the frames and so did less work per second than playing the clip takes. The 1080p60 run at load 4.3 reported 59.7 frames per second with a picture interval of 17.5 ms median and
18.7 ms p95: smooth. The runs that dropped most overlapped with other work on the VM (load 10 to 15), the
dropped frames and lateness grow with the load for old and new code alike.

* **H.264 1080p30 is real time on this machine now** (and 1080p60 when the VM is not busy); before, it was not (it
  decoded at 25 to 30 fps in isolation). The drops that remain at 720p30 and 1080p30 in the quiet runs are the ones
  when the clip loops (decoding restarts at the first key frame).
* Policy when the decoder runs late (`internal/engine/mediaplayer.go`): pictures are decoded ahead of the clock into a
  short queue (so a key frame that takes three frame times no longer makes the following pictures late; before
  this, 720p30 H.264 with B-frames dropped 40% of its frames); a picture that is already late and that nothing refers
  to (a non-reference H.264 B-frame) is skipped without being decoded; a late picture that is decoded anyway is
  still shown (moving video behind the sound beats a frozen frame); when the clock is more than 250 ms ahead of
  the decoder, decoding continues from the newest key frame that is already due, which cuts the delay to that key
  frame's age and never freezes the picture. Skipped pictures count as dropped in `getVideoPlaybackQuality()`,
  which is what quality-adapting players read. With frame threads `Decode` returns a picture whose last rows are
  still being reconstructed; the player waits for it on its own goroutine before it shows it, so the paint path
  never blocks on a decode.
* Earlier versions of this policy froze the picture for up to a GOP when they jumped to a *future* key frame
  (1080p H.264: 92% dropped, 2 s freezes); the numbers above are for the current one.

`getVideoPlaybackQuality()`: `totalVideoFrames` counts the frames due since the element started (in presentation
order, so B-frame reordering does not double count), `droppedVideoFrames` those that were skipped, `corruptedVideoFrames`
the access units the decoder rejected, `creationTime` the element's creation time.

## Real sites

Checked with `bench/mediasite` (the tool prints what the player probes, what is appended to which SourceBuffer, the
element's state every second, console errors and failed requests). Dates are when I ran it; sites change.

| Site | 2026-10-13 | Notes |
|---|---|---|
| dash.js reference sample (`reference.dashif.org`, Big Buck Bunny DASH, H.264 + AAC) | **plays**, picture and sound path active | After a click: `currentTime` advances 1.00 s per second, 480x270 to 540p renditions, 1115 frames in 37 s, 1 dropped, buffer grows to 52 s, no console errors. Without a click `play()` is refused with `NotAllowedError` like in Chromium (the sample autoplays unmuted). |
| Shaka Player 4.3.5, minimal page (angel-one DASH, VP9 720x576 + Opus WebM) | **plays** | `isBrowserSupported()` true, manifest parsed, 942 frames in 37.7 s, 0 dropped, buffer eviction behind the playhead works. Needed the fixes listed below (XML documents, EME interfaces, `playbackRate = 0`). |
| Shaka Player official demo page (`shaka-project.github.io/shaka-player/demo`) | **page runs, playback not exercised** | The UI builds (needed SVG interfaces and the URL fix); the "custom asset" flow needs UI interaction that I did not script. Locale `en-US` 404 is normal. |
| MDN `<video>` page | **loads, nothing to play in the page itself** | The examples are in an iframe (`interactive-examples.mdn.mozilla.net`) whose HTML is a template filled in by the editor script. A `<video>` pointed at the same `flower.webm` from MDN's server plays (960x540, 150 frames in 5.06 s, 0 dropped, ends). |
| YouTube embed (`/embed/aqz-KE-bpKQ`, Big Buck Bunny 60fps 4K) | **does not play** | See below. |
| YouTube watch page (`/watch?v=aqz-KE-bpKQ`) | **blocked** | Google answers with HTTP 429 "unusual traffic" (`google.com/sorry`) for this network's address, to `curl` as well as to the engine. Not an engine problem; it could not be tested from here. |
| Netflix, Disney+, Prime Video, YouTube Premium music/movies | **cannot work** | They need Widevine/PlayReady, which are not available; only Clear Key EME is implemented. |

YouTube embed in detail. Without a `Referer` the embedded player refuses to start ("Video player configuration
error", a YouTube policy for embeds); with one (`-referer https://example.com/`) it initialises, the BotGuard
attestation script runs (it needed a JavaScript heap above 64 MiB, the default is now 1 GiB, raised again because large reference pages need more), `GenerateIT` returns
200, `/youtubei/v1/player` returns 200 with a gzip-compressed request body (it uses `CompressionStream`), the player
creates a MediaSource, probes 36 types, asks `decodingInfo` 17 times and adds an `audio/webm; codecs="opus"` and a
`video/webm; codecs="vp09.00.51.08..."` SourceBuffer. Then it POSTs its SABR requests to `googlevideo.com`
(`application/vnd.yt-ump`); the server answers with 1.1 KB (a redirect to another host) and 154 bytes (a context
update) and no media, and the player ends in "Video unavailable". I could not find a cause in the engine. The most
likely ones are the proof-of-origin token that SABR streaming requires from web clients (the attestation runs but
its output may not satisfy the server) and the address of this network. Nothing was appended to a SourceBuffer, so
the MSE path was not reached on YouTube.

### What the real sites broke (fixed, each with a test)

* `fetch`/`XMLHttpRequest` dropped **every custom request header** (a JS object reaches Go as `*quickjs.Object`,
  not a map) and sent `ArrayBuffer`, typed array and `Blob` bodies through a UTF-8 round trip, corrupting gzip and
  protobuf payloads; `Response.body` and `clone()` did the same to binary responses (`TestFetchSendsRequestHeaders`).
* `CompressionStream`/`DecompressionStream` (gzip, deflate, deflate-raw) were missing, and `ReadableStream`,
  `WritableStream` and `TransformStream` were stubs that ignored the transformer: replaced by real implementations,
  `Response` accepts a stream as a body.
* No microtask checkpoint ran between classic scripts, so a script that set up state in a promise callback was not
  ready for the next `<script>` (the YouTube embed's `writeEmbed()`).
* `DOMParser`/`responseXML` for XML types returned an HTML tree (tag names lowercased, no namespaces). They now give a
  namespace-aware XML document with `parsererror` handling, and the nodes pass `instanceof Element` (Shaka filters
  a manifest's `childNodes` with it) (`TestDOMParserXML`).
* `SVGPathElement` and the other SVG element interfaces, and the EME interfaces, did not exist (a `ReferenceError`
  in Shaka's UI and polyfills).
* `new URL("https:../x", "https://h/a/b/")` must be relative (Shaka writes locale URLs that way); it produced
  `https://../x`.
* `video.playbackRate = 0` threw (it is valid).
* Inline event handler attributes (`onclick="..."`) never ran, only `addEventListener` and the `on*` properties did.
* The default JavaScript heap is 1 GiB (it was 64 MiB, then 256 MiB; MDN's compatibility tables need about 340 MiB).

### Known problems found on the way, not fixed

* A CSS `src: url(...) format('woff2')` list on YouTube's embed page is parsed so that one font URL ends in
  `)format('woff2'` (a 404).
* Web Audio `AudioContext.currentTime` is driven by the wall clock, not by the audio device.

## Reproducing

```
# decode speed of a clip
SWEEF_BENCH_VIDEO=clip.mp4 SWEEF_BENCH_FRAMES=450 go test -run TestDecodeSpeed -v ./internal/media

# the committed 1080p High profile clip (60 frames, CABAC, B-frames, 8x8 transform) from the first access unit
go test -run XXX -bench 'H264Decode1080p|Convert1080p' -benchtime 6x ./internal/media
go test -run XXX -bench DrawVideoFrame ./internal/raster

# the same clip with 0 (the plain decoder), 2, 3, 4 frame threads (the player uses one per core, at most 4)
SWEEF_H264_THREADS=0 SWEEF_BENCH_VIDEO=clip.mp4 SWEEF_BENCH_FRAMES=450 go test -run TestDecodeSpeed -v ./internal/media

# a hash of every picture, to check that a change altered no sample (compare the line before and after)
SWEEF_HASH_VIDEO=clip.mp4 go test -run TestFrameHashes -v ./internal/media

# 20 s through the page: dropped frames, picture intervals, lateness, CPU
SWEEF_AUDIO=null SWEEF_BENCH_VIDEO=clip.mp4 SWEEF_BENCH_SECONDS=20 go test -run TestPlaybackQuality -v ./internal/engine
# ... and with the page painted 60 times a second, the video filling a page of that size
SWEEF_BENCH_PAINT=1920x1080 SWEEF_AUDIO=null SWEEF_BENCH_VIDEO=clip.mp4 SWEEF_BENCH_SECONDS=20 go test -run TestPlaybackQuality -v ./internal/engine

# a real site (needs the network; real sites change)
go run ./bench/mediasite -url 'https://reference.dashif.org/dash.js/latest/samples/getting-started/auto-load-single-video-src.html' -seconds 40 -click video
go run ./bench/mediasite -url 'https://www.youtube.com/embed/aqz-KE-bpKQ' -referer https://example.com/ -seconds 40 -click .ytmCuedOverlayPlayButton
go run ./bench/mediasite -serve page.html -seconds 40      # your own page, served locally
```

`bench/mediasite` flags: `-ua`, `-referer`, `-js-mem`, `-eval`, `-eval-end`, `-play`, `-click`; `MEDIASITE_NET=300` prints
the network log, `MEDIASITE_CONSOLE=900` widens console lines, `MEDIASITE_TIMELINE=1` prints the script timeline.

The benchmark clips (`testsrc2` with `noise=alls=12:allf=t`, 20 s, `-g 60`):

```
ffmpeg -f lavfi -i "testsrc2=size=1920x1080:rate=30,noise=alls=12:allf=t" -t 20 -c:v libx264 -preset medium -profile:v high -b:v 6M -pix_fmt yuv420p -g 60 h264_1080.mp4
ffmpeg -f lavfi -i "testsrc2=size=1920x1080:rate=30,noise=alls=12:allf=t" -t 20 -c:v libvpx-vp9 -b:v 4M -deadline good -cpu-used 4 -row-mt 1 -tile-columns 2 -g 60 vp9_1080.webm
```

(720p: `-b:v 3M` for H.264, `-b:v 2M` for VP9; the 1080p60 clip: `rate=60`, `-b:v 8M`, `-g 120`.) The committed
fixtures are small: `internal/media/testdata/h264_1080p_high.mp4` (1080p, High profile, CABAC, B-frames, 8x8
transform, 60 frames, with ffmpeg's MD5 of every picture, checked by `TestNativeVideoFixtures`; made with
`testsrc2=size=1920x1080:rate=30,noise=alls=8:allf=t`, `-c:v libx264 -preset medium -profile:v high -b:v 4M
-maxrate 6M -bufsize 8M -g 60 -bf 3 -refs 3`), `internal/engine/testdata/media/mse/`
(fMP4 H.264+AAC in two qualities, WebM VP9+Opus, fMP4 AV1; `gen.sh` regenerates them) and
`h264_wave_320x192.mp4` (B-frames, 20x12 macroblocks, used for the deblocking and playback tests).
