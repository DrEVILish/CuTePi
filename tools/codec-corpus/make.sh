#!/bin/bash
# make.sh [OUTDIR] — generate CuTePi's codec test corpus: short video, audio
# and image files in many codecs and containers, made with the ffmpeg and
# GStreamer already on the Pi (nothing is installed). Files are named
# <kind>_<codec>_<details>.<ext>. Existing files are kept (delete to remake).
# Default OUTDIR: /root/cutepi-testmedia (outside the media pool, so the
# corpus never appears in the app until a test uploads it).
set -u
OUT=${1:-/root/cutepi-testmedia}
mkdir -p "$OUT/video" "$OUT/audio" "$OUT/image"
D=5 # seconds per clip
FF="ffmpeg -hide_banner -loglevel error -y"
ok=0; fail=0; failed=()
mk() { # mk <outfile> <ffmpeg args...>
	local f=$1; shift
	[ -s "$f" ] && { ok=$((ok+1)); return; }
	if $FF "$@" "$f" </dev/null && [ -s "$f" ]; then ok=$((ok+1)); echo "made $(basename "$f")"
	else fail=$((fail+1)); failed+=("$(basename "$f")"); rm -f "$f"; echo "FAILED $(basename "$f")"; fi
}
# Sources: moving test card with a frame counter, and a tone.
v() { echo "-f lavfi -i testsrc2=size=$1:rate=$2,drawtext=text=%{n}:fontsize=$(( ${1%x*} / 16 )):x=40:y=40:fontcolor=white:box=1:boxcolor=black -t $D"; }
A="-f lavfi -i sine=frequency=1000:sample_rate=48000 -t $D"

V=$OUT/video
# --- hardware-decodable on the Pi 4 (H.264 up to 1080p60; HEVC up to 4K)
mk $V/video_h264_1080p30_aac.mp4          $(v 1920x1080 30) $A -c:v libx264 -preset veryfast -pix_fmt yuv420p -c:a aac
mk $V/video_h264_1080p60_aac.mp4          $(v 1920x1080 60) $A -c:v libx264 -preset veryfast -pix_fmt yuv420p -c:a aac
mk $V/video_h264_720p50_mp3.mkv           $(v 1280x720 50)  $A -c:v libx264 -preset veryfast -pix_fmt yuv420p -c:a libmp3lame
mk $V/video_h264_1080p25_interlaced.ts    $(v 1920x1080 25) $A -c:v libx264 -preset veryfast -pix_fmt yuv420p -flags +ildct+ilme -x264opts tff=1 -c:a ac3
mk $V/video_h264_high10_1080p30.mkv       $(v 1920x1080 30) $A -c:v libx264 -preset veryfast -pix_fmt yuv420p10le -c:a aac
mk $V/video_hevc_1080p30_aac.mp4          $(v 1920x1080 30) $A -c:v libx265 -preset ultrafast -x265-params log-level=error -tag:v hvc1 -pix_fmt yuv420p -c:a aac
mk $V/video_hevc_1080p60_aac.mp4          $(v 1920x1080 60) $A -c:v libx265 -preset ultrafast -x265-params log-level=error -tag:v hvc1 -pix_fmt yuv420p -c:a aac
mk $V/video_hevc_main10_1080p30.mkv       $(v 1920x1080 30) $A -c:v libx265 -preset ultrafast -x265-params log-level=error -pix_fmt yuv420p10le -c:a aac
mk $V/video_hevc_2160p30_aac.mp4          $(v 3840x2160 30) $A -c:v libx265 -preset ultrafast -x265-params log-level=error -tag:v hvc1 -pix_fmt yuv420p -c:a aac
# --- software-decoded codecs
mk $V/video_vp9_1080p30_opus.webm         $(v 1920x1080 30) $A -c:v libvpx-vp9 -deadline realtime -cpu-used 8 -row-mt 1 -b:v 4M -c:a libopus
mk $V/video_av1_1080p30_opus.mkv          $(v 1920x1080 30) $A -c:v libsvtav1 -preset 12 -crf 35 -c:a libopus
mk $V/video_mpeg2_1080i25_ac3.mpg         $(v 1920x1080 25) $A -c:v mpeg2video -b:v 15M -flags +ildct+ilme -top 1 -c:a ac3
mk $V/video_mpeg4asp_720p30_mp3.avi       $(v 1280x720 30)  $A -c:v mpeg4 -q:v 3 -c:a libmp3lame
mk $V/video_prores422hq_1080p25_pcm.mov   $(v 1920x1080 25) $A -c:v prores_ks -profile:v 3 -c:a pcm_s24le
mk $V/video_prores4444_1080p25_alpha.mov  $(v 1920x1080 25) -c:v prores_ks -profile:v 4 -pix_fmt yuva444p10le
mk $V/video_dnxhr_hq_1080p25_pcm.mov      $(v 1920x1080 25) $A -c:v dnxhd -profile:v dnxhr_hq -pix_fmt yuv422p -c:a pcm_s16le
mk $V/video_mjpeg_1080p30_pcm.avi         $(v 1920x1080 30) $A -c:v mjpeg -q:v 3 -pix_fmt yuvj420p -c:a pcm_s16le
mk $V/video_theora_720p30_vorbis.ogv      $(v 1280x720 30)  $A -c:v libtheora -q:v 7 -c:a libvorbis
mk $V/video_wmv2_720p30_wma.wmv           $(v 1280x720 30)  $A -c:v wmv2 -b:v 4M -c:a wmav1
mk $V/video_ffv1_1080p25_flac.mkv         $(v 1920x1080 25) $A -c:v ffv1 -c:a flac
mk $V/video_qtrle_720p25_animation.mov    $(v 1280x720 25) -c:v qtrle
mk $V/video_hap_1080p25.mov               $(v 1920x1080 25) -c:v hap
mk $V/video_h264_vertical_1080x1920.mp4   $(v 1080x1920 30) $A -c:v libx264 -preset veryfast -pix_fmt yuv420p -c:a aac
# VP8: ffmpeg here has no libvpx VP8 encoder; GStreamer's vp8enc does.
f=$V/video_vp8_720p30_vorbis.webm
if [ ! -s "$f" ]; then
	if gst-launch-1.0 -q videotestsrc pattern=ball num-buffers=$((D*30)) ! video/x-raw,width=1280,height=720,framerate=30/1 ! vp8enc deadline=1 ! queue ! webmmux name=m ! filesink location="$f" \
		audiotestsrc num-buffers=$((D*48000/1024)) ! audioconvert ! vorbisenc ! queue ! m. >/dev/null 2>&1 && [ -s "$f" ]; then ok=$((ok+1)); echo "made $(basename "$f")"
	else fail=$((fail+1)); failed+=("$(basename "$f")"); rm -f "$f"; echo "FAILED $(basename "$f")"; fi
else ok=$((ok+1)); fi

AU=$OUT/audio
mk $AU/audio_aac_48k_stereo.m4a         $A -c:a aac -b:a 192k -ac 2
mk $AU/audio_mp3_44k1_stereo.mp3        $A -ar 44100 -c:a libmp3lame -b:a 192k -ac 2
mk $AU/audio_opus_48k_stereo.opus       $A -c:a libopus -b:a 128k -ac 2
mk $AU/audio_vorbis_48k_stereo.ogg      $A -c:a libvorbis -q:a 5 -ac 2
mk $AU/audio_flac_96k_24bit.flac        -f lavfi -i sine=frequency=1000:sample_rate=96000 -t $D -c:a flac -sample_fmt s32 -ac 2
mk $AU/audio_pcm_s16_48k.wav            $A -c:a pcm_s16le -ac 2
mk $AU/audio_pcm_s24_96k.wav            -f lavfi -i sine=frequency=1000:sample_rate=96000 -t $D -c:a pcm_s24le -ac 2
mk $AU/audio_ac3_5.1.ac3                -f lavfi -i sine=frequency=440:sample_rate=48000 -t $D -af "pan=5.1|c0=c0|c1=c0|c2=c0|c3=c0|c4=c0|c5=c0" -c:a ac3
mk $AU/audio_eac3_5.1.eac3              -f lavfi -i sine=frequency=440:sample_rate=48000 -t $D -af "pan=5.1|c0=c0|c1=c0|c2=c0|c3=c0|c4=c0|c5=c0" -c:a eac3
mk $AU/audio_alac_44k1.m4a              -f lavfi -i sine=frequency=1000:sample_rate=44100 -t $D -c:a alac -ac 2
mk $AU/audio_pcm_mono_22k05.aiff        -f lavfi -i sine=frequency=1000:sample_rate=22050 -t $D -c:a pcm_s16be -ac 1
mk $AU/audio_wma_44k1.wma               $A -ar 44100 -c:a wmav1 -ac 2

I=$OUT/image
P="-f lavfi -i testsrc2=size=1920x1080:rate=1 -frames:v 1"
mk $I/image_jpeg_1920x1080.jpg          $P -q:v 2
mk $I/image_jpeg_4000x3000.jpg          -f lavfi -i testsrc2=size=4000x3000:rate=1 -frames:v 1 -q:v 2
mk $I/image_jpeg_progressive.jpg        $P -q:v 2 -huffman optimal
mk $I/image_png_1920x1080.png           $P
mk $I/image_png_rgba_transparent.png    -f lavfi -i "testsrc2=size=1920x1080:rate=1,format=rgba,colorchannelmixer=aa=0.5" -frames:v 1
mk $I/image_png_16bit.png               $P -pix_fmt rgb48be
mk $I/image_webp_1920x1080.webp         $P -c:v libwebp -quality 90
mk $I/image_webp_lossless.webp          $P -c:v libwebp -lossless 1
mk $I/image_gif_1920x1080.gif           $P
mk $I/image_gif_animated.gif            -f lavfi -i testsrc2=size=640x360:rate=10 -t 3
mk $I/image_bmp_1920x1080.bmp           $P
mk $I/image_tiff_1920x1080.tiff         $P
mk $I/image_jpeg2000_1920x1080.jp2      $P -c:v libopenjpeg
mk $I/image_avif_1920x1080.avif         $P -c:v libaom-av1 -still-picture 1 -cpu-used 8
mk $I/image_jxl_1920x1080.jxl           $P -c:v libjxl
mk $I/image_png_portrait_1080x1920.png  -f lavfi -i testsrc2=size=1080x1920:rate=1 -frames:v 1

echo "corpus: $ok present, $fail failed${failed[*]:+ (${failed[*]})} in $OUT"
