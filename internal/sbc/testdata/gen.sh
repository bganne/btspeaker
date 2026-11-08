#!/bin/sh
# Regenerates the SBC test vectors with ffmpeg (sbc encoder + decoder).
# For each vector NAME.sbc, NAME.pcm is ffmpeg's decode (s16le, interleaved).
# ffmpeg picks the channel mode from the bitrate (mono / stereo / joint
# stereo) and always uses loudness allocation, so the dual channel and SNR
# allocation vectors are derived from the vectors above (TestGenDerivedVectors
# in sbc_test.go) and only decoded by ffmpeg.
set -e
cd "$(dirname "$0")"
D=0.15

# gen NAME RATE CHANNELS BITRATE SBC_DELAY SOURCE_EXPR(lavfi, one or two inputs)
gen() {
	echo "gen $1" >&2
	name=$1 rate=$2 ch=$3 br=$4 delay=$5 shift=$6
	case $name in
	*noise*) L="anoisesrc=r=$rate:d=$D:a=0.5:seed=1"; R="anoisesrc=r=$rate:d=$D:a=0.5:seed=2" ;;
	*chirp*) L="aevalsrc=0.6*sin(2*PI*(200*t+8000*t*t)):s=$rate:d=$D"; R="aevalsrc=0.6*sin(2*PI*(300*t+5000*t*t)):s=$rate:d=$D" ;;
	*) L="aevalsrc=0.7*sin(2*PI*1000*t):s=$rate:d=$D"; R="aevalsrc=0.7*sin(2*PI*1500*t):s=$rate:d=$D" ;;
	esac
	if [ "$ch" = 1 ]; then
		ffmpeg -v error -y -f lavfi -i "$L" -ac 1 -c:a sbc -b:a "$br" -sbc_delay "$delay" -f sbc "$name.sbc"
	else
		ffmpeg -v error -y -f lavfi -i "$L" -f lavfi -i "$R" \
			-filter_complex "[0][1]amerge=inputs=2" -c:a sbc -b:a "$br" -sbc_delay "$delay" -f sbc "$name.sbc"
	fi
	ffmpeg -v error -y -i "$name.sbc" -f s16le "$name.pcm"
}

gen mono_44k_sine    44100 1 64k  0.013
gen mono_16k_chirp   16000 1 32k  0.013
gen mono_48k_noise   48000 1 128k 0.013
gen mono4_32k_chirp  32000 1 64k  0.003
gen stereo_44k_noise 44100 2 320k 0.013
gen stereo_48k_chirp 48000 2 320k 0.013
gen stereo4_44k_sine 44100 2 320k 0.001
gen joint_44k_chirp  44100 2 128k 0.013
gen joint_32k_sine   32000 2 96k  0.013
gen joint_noise_44k  44100 2 128k 0.013
gen joint4_44k_chirp 44100 2 128k 0.002
gen joint_16k_noise  16000 2 64k  0.013
# High bitpool (SBC XQ range): joint stereo, bitpool ~77 and ~105.
gen joint_xq_44k_noise 44100 2 900k  0.013
gen joint_hq_48k_chirp 48000 2 1200k 0.013

# Derived vectors (dual channel, SNR allocation): ffmpeg decodes them but
# can't encode them.
SBC_GEN_DIR=$PWD go test -count=1 -run TestGenDerivedVectors ..
for name in dual_48k_noise dualsnr_16k_chirp snr_joint_32k_sine snr_stereo4_44k_sine; do
	echo "derive $name" >&2
	ffmpeg -v error -y -i "$name.sbc" -f s16le "$name.pcm"
done
