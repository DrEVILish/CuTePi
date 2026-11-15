# Codec evidence for the CuTePi design review

Reference supplied by the owner on 2026-11-15: [RaspberryPi Codecs](t3-thread://v2/9083cb1a-e6ff-40c7-869a-a43f5d9bcad6/ce8e274b-211f-4b54-9214-4dcc1a96eeaa).

The thread contains decoder optimisation, throughput and picture-quality work. Use its final corrections alongside the underlying reports; earlier replies contain superseded results. This index records historical evidence, not a fresh benchmark or a claim about the currently deployed CuTePi binary.

## Source and integration boundary

The reference repository is [raspberry-pi-video-codec-performance](https://github.com/DrEVILish/raspberry-pi-video-codec-performance). Its last recorded implementation commit in the thread is [f3a5632](https://github.com/DrEVILish/raspberry-pi-video-codec-performance/commit/f3a5632), with the updated README and CineForm SDK optimisation. The final Sol review approved that commit; it checked patch application and shell syntax without rerunning builds, frame comparisons or benchmarks.

CuTePi carries its own decoder copies and chooses decoders at runtime. The historical service measurement selected avdec_cfhd rather than the standalone cutepicfhddec. Before importing a fix or quoting a result for CuTePi, establish the source/SDK revision, build flags, plugin selection, output format, runtime profile and actual selected decoder. Upstream code or a standalone benchmark does not establish deployment.

## Findings to carry into the review

- GIF, AV1/AVIF, MJPEG and PNG worker changes have measured throughput gains on particular clips. The thread records same-session alternating comparisons and pixel checks; these are evidence for those test cases, not universal codec limits.
- AV1/AVIF dav1d frame delay increased from two to four for throughput. The author explicitly identified possible extra startup delay. Assess buffering against the first-output contract before adopting that setting.
- PNG's expanded output buffering adds memory pressure. Include native buffers and simultaneous sources in admission and soak qualification.
- CineForm's final SDK optimisation preserves per-frame checksums on the reported corpus and improves transparent output throughput. Transparent performance was corrected after earlier measurements had dropped alpha. The final optimisation was not qualified for seeking in a player or on x86.
- CineForm RGBA's format-detection fix was checked on the Pi and preserves transparency. RGB 12-bit remains an open quality issue: conversion to 8-bit YUY2 loses colour detail. The attempted RGB output was slower and incorrect. Do not mark RGB quality resolved.
- Bob matches the reference with the correct explicit field order; it does not automatically follow the stream's field order.
- GIF with transparency matched after applying alpha. AVIF matched libavif on sampled frames, including alpha. These checks do not establish every-frame coverage for every format.
- H.264 hardware versus software has small nonzero pixel differences whose cause remains unexplained. The 10-bit packer also differs from videoconvert. Preserve these qualifications rather than calling all outputs bit-identical.

## Measurement limits and next use

Most speed measurements decoded into a discard sink; some used fakevideosink. Neither proves actual CuTePi scanout, audible output, GO latency, A/V alignment or zero missing AV Sync Test frames. Do not combine results from different clips, decoder choices, sinks or hardware as interchangeable limits, and do not extrapolate Pi4 results to Pi5.

The thread corrected an AVIF short-frame result caused by its harness, excluded an unchecked implausible HEVC throughput result, and identified overlapping CineForm/CuTePi workloads that invalidated timing passes. Future qualification needs an exclusive agreed hardware slot, bounded measurement overhead and verified frame counts. The thread also records benchmark-generation crashes and a profiling-package installation contrary to the Pi's tooling restrictions; it is not permission to repeat those actions. Any cleanup requires checking present state and coordinating the playback slot.

Use this evidence in the open decisions for [physical output acceptance](../.scratch/design-review/issues/02-output-and-av-acceptance.md), [capability and background budgets](../.scratch/design-review/issues/08-capability-and-background-budget.md), and [runtime/release qualification](../.scratch/design-review/issues/11-runtime-and-release-gates.md). Cold/unprepared GO policy and numerical acceptance criteria remain owner decisions.
