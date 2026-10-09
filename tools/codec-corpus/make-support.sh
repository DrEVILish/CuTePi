#!/bin/bash
# make-support.sh [OUTDIR] — the codec SUPPORT set: one test file per codec
# and container, at the support criterion (README "Codec support"): video at
# 1080p60, 8 s long (room for a 1 s fade in, steady play, a 1 s fade out),
# each video codec in both .mov and .mkv wherever the container can carry
# it; every audio codec in .mov and .mkv; 1080p stills in every image
# format; transparency (alpha) in every video codec and image format that
# carries it; animated GIF, APNG and WebP. Made with the ffmpeg/GStreamer already on the Pi. Existing files
# are kept. Default OUTDIR: /root/cutepi-testmedia/support.
set -u
OUT=${1:-/root/cutepi-testmedia/support}
mkdir -p "$OUT/video" "$OUT/audio" "$OUT/image"
D=8
FF="ffmpeg -hide_banner -loglevel error -y"
ok=0; fail=0; failed=()
mk() { # mk <outfile> <ffmpeg args...>
	local f=$1; shift
	[ -s "$f" ] && { ok=$((ok+1)); return; }
	if $FF "$@" "$f" </dev/null && [ -s "$f" ]; then ok=$((ok+1)); echo "made $(basename "$f")"
	else fail=$((fail+1)); failed+=("$(basename "$f")"); rm -f "$f"; echo "FAILED $(basename "$f")"; fi
}
# 1080p60 moving test card with a frame counter (every frame differs) + tone.
SRC="-f lavfi -i testsrc2=size=1920x1080:rate=60,drawtext=text=%{n}:fontsize=120:x=40:y=40:fontcolor=white:box=1:boxcolor=black -f lavfi -i sine=frequency=1000:sample_rate=48000 -t $D"
V=$OUT/video
# video <name> <containers> <video args> [audio args]: name -> support_<name>.<ext>
video() {
	local name=$1 exts=$2 vargs=$3 aargs=${4:--c:a aac}
	for ext in $exts; do
		mk "$V/${name}.$ext" $SRC $vargs $aargs
	done
}
video h264_high         "mov mkv" "-c:v libx264 -preset veryfast -pix_fmt yuv420p"
video h264_high10       "mov mkv" "-c:v libx264 -preset veryfast -pix_fmt yuv420p10le"
video hevc_main         "mov mkv" "-c:v libx265 -preset ultrafast -x265-params log-level=error -tag:v hvc1 -pix_fmt yuv420p"
video hevc_main10       "mov mkv" "-c:v libx265 -preset ultrafast -x265-params log-level=error -tag:v hvc1 -pix_fmt yuv420p10le"
video vp9               "mkv"     "-c:v libvpx-vp9 -deadline realtime -cpu-used 8 -row-mt 1 -b:v 8M" "-c:a libopus"
video av1               "mkv"     "-c:v libsvtav1 -preset 12 -crf 35" "-c:a libopus"
video mpeg2             "mov mkv" "-c:v mpeg2video -b:v 25M" "-c:a ac3"
video mpeg4_asp         "mov mkv" "-c:v mpeg4 -q:v 3" "-c:a libmp3lame"
video mjpeg             "mov mkv" "-c:v mjpeg -q:v 3 -pix_fmt yuvj420p" "-c:a pcm_s16le"
video prores_proxy      "mov mkv" "-c:v prores_ks -profile:v 0" "-c:a pcm_s16le"
video prores_lt         "mov mkv" "-c:v prores_ks -profile:v 1" "-c:a pcm_s16le"
video prores_422        "mov mkv" "-c:v prores_ks -profile:v 2" "-c:a pcm_s16le"
video prores_422hq      "mov mkv" "-c:v prores_ks -profile:v 3" "-c:a pcm_s16le"
video prores_4444       "mov mkv" "-c:v prores_ks -profile:v 4 -pix_fmt yuva444p10le" "-c:a pcm_s16le"
video prores_4444xq     "mov mkv" "-c:v prores_ks -profile:v 5 -pix_fmt yuv444p10le" "-c:a pcm_s16le"
video dnxhr_hq          "mov mkv" "-c:v dnxhd -profile:v dnxhr_hq -pix_fmt yuv422p" "-c:a pcm_s16le"
video dnxhr_lb          "mov mkv" "-c:v dnxhd -profile:v dnxhr_lb -pix_fmt yuv422p" "-c:a pcm_s16le"
video cineform_422      "mov mkv" "-c:v cfhd -quality film1 -pix_fmt yuv422p10le" "-c:a pcm_s16le"
video cineform_rgb12    "mov mkv" "-c:v cfhd -quality film1 -pix_fmt gbrp12le" "-c:a pcm_s16le"
video cineform_rgba12   "mov mkv" "-c:v cfhd -quality film1 -pix_fmt gbrap12le" "-c:a pcm_s16le"
video ffv1              "mkv"     "-c:v ffv1" "-c:a flac"
video qtrle             "mov mkv" "-c:v qtrle" "-c:a pcm_s16le"
video hap               "mov mkv" "-c:v hap" "-c:a pcm_s16le"
video theora            "mkv"     "-c:v libtheora -q:v 7" "-c:a libvorbis"   # MOV cannot carry Theora
# VP9, AV1 and FFV1 above are MKV only: ffmpeg's QuickTime writer refuses them in .mov
# (they travel in MP4/WebM/MKV), as it does Opus and FLAC audio below.
video wmv2              "mkv"     "-c:v wmv2 -b:v 20M" "-c:a wmav2"          # MOV cannot carry WMV
# VP8: no libvpx VP8 encoder in this ffmpeg; GStreamer's vp8enc into MKV (MOV cannot carry VP8).
f=$V/vp8.mkv
if [ ! -s "$f" ]; then
	if gst-launch-1.0 -q videotestsrc pattern=ball num-buffers=$((D*60)) ! video/x-raw,width=1920,height=1080,framerate=60/1 ! \
		vp8enc deadline=1 cpu-used=16 target-bitrate=12000000 threads=4 ! queue ! matroskamux name=m ! filesink location="$f" \
		audiotestsrc num-buffers=$((D*48000/1024)) ! audioconvert ! vorbisenc ! queue ! m. >/dev/null 2>&1 && [ -s "$f" ]
	then ok=$((ok+1)); echo "made vp8.mkv"; else fail=$((fail+1)); failed+=(vp8.mkv); rm -f "$f"; echo "FAILED vp8.mkv"; fi
else ok=$((ok+1)); fi

# Video with an alpha channel (name "<codec>_alpha"): the moving test card
# merged with a fixed mask (MASK) — opaque on the left third, a ramp to fully
# transparent across the middle third, transparent on the right third — so the
# file carries real transparency, not an opaque alpha plane, and the wall has to
# show a straight (non-premultiplied) alpha edge and gradient correctly. Every
# codec here that can carry alpha, in each of MOV and MKV that can carry it.
MASK=$OUT/alpha_mask.png
[ -s "$MASK" ] || $FF -f lavfi -i color=black:size=1920x1080 -frames:v 1 \
	-vf "format=gray,geq=lum='clip((1280-X)*255/640,0,255)'" "$MASK" </dev/null
alpha() { # alpha <name> <containers> <video args> [audio args] -> video/<name>_alpha.<ext>
	local name=$1 exts=$2 vargs=$3 aargs=${4:--c:a pcm_s16le}
	for ext in $exts; do
		mk "$V/${name}_alpha.$ext" $SRC -loop 1 -framerate 60 -i "$MASK" -t $D \
			-filter_complex "[2:v]format=gray[m];[0:v]format=rgba[c];[c][m]alphamerge[v]" -map "[v]" -map 1:a \
			$vargs $aargs
	done
}
alpha prores_4444 "mov mkv" "-c:v prores_ks -profile:v 4 -pix_fmt yuva444p10le -alpha_bits 16"
alpha qtrle       "mov mkv" "-c:v qtrle -pix_fmt argb"
alpha png         "mov mkv" "-c:v png -pix_fmt rgba"
alpha cineform    "mov mkv" "-c:v cfhd -quality film1 -pix_fmt gbrap12le"
alpha hap         "mov mkv" "-c:v hap -format hap_alpha"
# CineForm as the CineForm SDK writes it (tools/codec-corpus/cfhdenc.c, on the SDK library CuTePi decodes with):
# FFmpeg's cfhd encoder writes streams the SDK rejects in part, so the corpus has both. Needs the library built
# (deploy/build-cineform.sh). cfsdk <name> <422|444|4444> -> video/<name>.{mov,mkv}
CFHDENC=${CFHDENC:-/tmp/cutepi-cfhdenc}
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
if [ ! -x "$CFHDENC" ] && [ -f "$ROOT/third_party/cineform-sdk/libcutepi-cfhd.so" ]; then
	cc -O2 -o "$CFHDENC" "$ROOT/tools/codec-corpus/cfhdenc.c" -I "$ROOT/third_party/cineform-sdk/Common" \
		$(pkg-config --cflags --libs gstreamer-app-1.0) -L "$ROOT/third_party/cineform-sdk" -lcutepi-cfhd \
		-Wl,-rpath,"$ROOT/third_party/cineform-sdk" || true
fi
cfsdk() {
	local name=$1 fmt=$2 pix=yuyv422 vf=""
	[ -x "$CFHDENC" ] || { echo "SKIPPED $name (no cfhdenc: build the CineForm library first)"; return; }
	[ "$fmt" = 422 ] || pix=bgra
	local tmp=$V/.${name}.video.mov
	if [ ! -s "$V/$name.mov" ] || [ ! -s "$V/$name.mkv" ]; then
		if [ "$fmt" = 4444 ]; then
			$FF $SRC -loop 1 -framerate 60 -i "$MASK" -t $D -filter_complex "[2:v]format=gray[m];[0:v]format=rgba[c];[c][m]alphamerge[v]" \
				-map "[v]" -f rawvideo -pix_fmt $pix - </dev/null | "$CFHDENC" 1920 1080 60 $fmt "$tmp" film1
		else
			$FF $SRC -map 0:v -f rawvideo -pix_fmt $pix - </dev/null | "$CFHDENC" 1920 1080 60 $fmt "$tmp" film1
		fi
	fi
	for ext in mov mkv; do
		mk "$V/$name.$ext" -i "$tmp" -f lavfi -i sine=frequency=1000:sample_rate=48000 -t $D -map 0:v -map 1:a -c:v copy -c:a pcm_s16le
	done
	rm -f "$tmp"
}
cfsdk cineform_sdk_422 422
cfsdk cineform_sdk_444 444
cfsdk cineform_sdk_alpha 4444
alpha ffv1        "mkv"     "-c:v ffv1 -pix_fmt yuva420p" "-c:a flac"
alpha vp9         "mkv"     "-c:v libvpx-vp9 -deadline realtime -cpu-used 8 -row-mt 1 -b:v 8M -pix_fmt yuva420p" "-c:a libopus"

A=$OUT/audio
TONE="-f lavfi -i sine=frequency=1000:sample_rate=48000 -t $D -ac 2"
for ext in mov mkv; do
	mk $A/aac.$ext        $TONE -c:a aac -b:a 192k
	mk $A/mp3.$ext        $TONE -c:a libmp3lame -b:a 192k
	mk $A/pcm_s16.$ext    $TONE -c:a pcm_s16le
	mk $A/pcm_s24.$ext    $TONE -c:a pcm_s24le
	mk $A/ac3_5.1.$ext    -f lavfi -i sine=frequency=440:sample_rate=48000 -t $D -af "pan=5.1|c0=c0|c1=c0|c2=c0|c3=c0|c4=c0|c5=c0" -c:a ac3
	mk $A/eac3_5.1.$ext   -f lavfi -i sine=frequency=440:sample_rate=48000 -t $D -af "pan=5.1|c0=c0|c1=c0|c2=c0|c3=c0|c4=c0|c5=c0" -c:a eac3
	mk $A/alac.$ext       $TONE -c:a alac
done
mk $A/opus.mkv        $TONE -c:a libopus -b:a 128k  # MOV writer refuses Opus
mk $A/flac.mkv        $TONE -c:a flac               # MOV writer refuses FLAC
mk $A/vorbis.mkv      $TONE -c:a libvorbis  # MOV cannot carry Vorbis
mk $A/wma.mkv         $TONE -c:a wmav2      # MOV cannot carry WMA

I=$OUT/image
P="-f lavfi -i testsrc2=size=1920x1080:rate=1 -frames:v 1"
mk $I/jpeg.jpg            $P -q:v 2
mk $I/png.png             $P
mk $I/webp.webp           $P -c:v libwebp -quality 90
mk $I/gif.gif             $P
mk $I/bmp.bmp             $P
mk $I/tiff.tiff           $P
mk $I/jpeg2000.jp2        $P -c:v libopenjpeg
mk $I/avif.avif           $P -c:v libaom-av1 -still-picture 1 -cpu-used 8
mk $I/jpegxl.jxl          $P -c:v libjxl
# Transparent stills: the test card through the same mask as the alpha videos
# (GIF transparency is on/off per pixel, so its ramp is cut at 50 %).
AP="-f lavfi -i testsrc2=size=1920x1080:rate=1:duration=1 -i $MASK -frames:v 1"
AM="[1:v]format=gray[m];[0:v]format=rgba[c];[c][m]alphamerge"
mk $I/png_alpha.png       $AP -filter_complex "$AM" -pix_fmt rgba
mk $I/tiff_alpha.tiff     $AP -filter_complex "$AM" -pix_fmt rgba
mk $I/webp_alpha.webp     $AP -filter_complex "$AM" -c:v libwebp -quality 90 -pix_fmt yuva420p
# gif <outfile> <fps> <alpha 0|1>: two passes, palette to a file first. (One
# pass with split → palettegen buffers every 1080p frame until the end of the
# stream; with an endless source that ran the Pi out of memory.) Sources are
# bounded with duration=, never by an output -t alone.
gif() {
	local f=$1 fps=$2 a=$3 n=$4 pal=$OUT/.palette.png
	[ -s "$f" ] && { ok=$((ok+1)); return; }
	local src="testsrc2=size=1920x1080:rate=$fps:duration=$n"
	[ "$n" != 1 ] && src="$src,drawtext=text=%{n}:fontsize=120:x=40:y=40:fontcolor=white:box=1:boxcolor=black"
	local in=(-f lavfi -i "$src") pre="[0:v]format=rgba" pg="palettegen=stats_mode=diff" pu="paletteuse=dither=none"
	if [ "$a" = 1 ]; then
		in+=(-loop 1 -framerate "$fps" -t "$n" -i "$MASK")
		pre="[1:v]format=gray[m];[0:v]format=rgba[c];[c][m]alphamerge"
		pg="$pg:reserve_transparent=1"; pu="$pu:alpha_threshold=128"
	fi
	if $FF "${in[@]}" -filter_complex "$pre,$pg" -frames:v 1 -update 1 "$pal" </dev/null &&
		$FF "${in[@]}" -i "$pal" -filter_complex "$pre[v];[v][$([ "$a" = 1 ] && echo 2 || echo 1):v]$pu" -loop 0 "$f" </dev/null && [ -s "$f" ]
	then ok=$((ok+1)); echo "made $(basename "$f")"
	else fail=$((fail+1)); failed+=("$(basename "$f")"); rm -f "$f"; echo "FAILED $(basename "$f")"; fi
	rm -f "$pal"
}
gif $I/gif_alpha.gif 1 1 1
# Animated images, 8 s, looping forever, played at the file's own frame timing.
# GIF stores frame delays in 1/100 s, so 60 fps cannot be written: 25 fps
# (4/100 s, exact) is the common GIF rate, 50 fps (2/100 s) the fastest. APNG
# and animated WebP at 30 fps. "_alpha": a transparent area, as above.
gif $I/gif_anim.gif        25 0 $D
gif $I/gif_anim_alpha.gif  25 1 $D
gif $I/gif_anim_50fps.gif  50 0 $D
AN30="-f lavfi -i testsrc2=size=1920x1080:rate=30:duration=$D,drawtext=text=%{n}:fontsize=120:x=40:y=40:fontcolor=white:box=1:boxcolor=black"
mk $I/apng_anim.apng      $AN30 -c:v apng -plays 0
mk $I/webp_anim_30fps.webp $AN30 -c:v libwebp_anim -quality 80 -loop 0

echo "support set: $ok present, $fail failed${failed[*]:+ (${failed[*]})} in $OUT"
