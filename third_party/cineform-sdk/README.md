# GoPro CineForm SDK, for CuTePi's CineForm decoder

Upstream: https://github.com/gopro/cineform-sdk at commit `11574d0295771edccadd17af14af74a539f924c7` (Apache-2.0 / MIT: LICENSE-APACHE, LICENSE-MIT,
LICENSE.txt). Only the parts the decoder and encoder library needs are vendored (Codec, Common, ConvertLib,
DecoderSDK, EncoderSDK, WarpLib).

`make && make install` (or `deploy/build-cineform.sh`) builds `libcutepi-cfhd.so` and installs it to
`/usr/local/lib/cutepi/`, where `gsp/cfhd` (the `cutepicfhddec` GStreamer element) loads it at run time.

CuTePi's changes (`cutepi.patch` against upstream for `Codec/decoder.c`, and new files):

- **AArch64** (`compat/`): the SDK is hand-optimised with SSE2 and MMX intrinsics for x86 only. On ARM the
  Makefile puts `compat/` ahead of the system headers: `emmintrin.h`, `xmmintrin.h`, `mmintrin.h` and
  `mm_malloc.h` there pull in [sse2neon](https://github.com/DLTcollab/sse2neon) (MIT; SSE/SSE2 on NEON) and
  `mmx2neon.h` (new: the ~50 64-bit MMX intrinsics the SDK uses that sse2neon lacks, on 64-bit NEON vectors with
  Intel's saturation and shift-count semantics). Built with `-fsigned-char` (x86's `char`). Single-threaded
  output is bit-identical to the x86 build for every frame compared.
- **Bounds** (`Codec/decoder.c`, DecodeFastRunsFSM16s): the threaded entropy decoder scanned for a band-end
  marker with no bound; a sample without one (FFmpeg's CineForm encoder writes such samples) ran it off the end of
  the buffer. It now stops at the end of the sample and fails it.
- **Per-frame dither** (`Codec/cutepi_rand.c`, `-Drand=cutepi_rand` for the C files, a reset at the start of
  DecodeSample): the SDK dithers 10-to-8-bit output with libc `rand()`, whose global state made output vary run
  to run and between threads. Each sample now dithers from the same per-thread generator state.

Not used: the SDK's own decoder threads. Its threaded path still differs by ±1 between runs and does not recover
from a rejected sample (queue assertion). The CuTePi element runs several single-threaded decoders on alternate
frames instead (each in its own copy of the library, `dlmopen`).
