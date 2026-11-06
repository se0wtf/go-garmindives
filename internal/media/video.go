package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// VideoProcessor normalizes a video into a seekable, browser-compatible MP4.
// The caller publishes it only after verification and thumbnail generation.
type VideoProcessor interface {
	Video(context.Context, *os.File, *os.File, Probe) (Probe, error)
}

// VideoSize fits landscape into 1920x1080 and portrait into 1080x1920, using
// display orientation and square pixels. It never enlarges the displayed image.
func VideoSize(probe Probe) (int, int) {
	sar := probe.SampleAspect
	if sar <= 0 || math.IsNaN(sar) || math.IsInf(sar, 0) {
		sar = 1
	}
	w, h := float64(probe.Width)*sar, float64(probe.Height)
	if probe.Rotation == 90 || probe.Rotation == 270 {
		w, h = h, w
	}
	if w < 2 || h < 2 || math.IsNaN(w) || math.IsInf(w, 0) {
		return 0, 0
	}
	maxW, maxH := 1920.0, 1080.0
	if h > w {
		maxW, maxH = maxH, maxW
	}
	ratio := min(1.0, maxW/w, maxH/h)
	return int(math.Floor(w*ratio/2)) * 2, int(math.Floor(h*ratio/2)) * 2
}

func videoHDR(probe Probe) bool {
	return probe.ColorTransfer == "smpte2084" || probe.ColorTransfer == "arib-std-b67"
}

func videoCopy(probe Probe) bool {
	w, h := VideoSize(probe)
	// FFprobe may name full-range 8-bit 4:2:0 yuvj420p, even when the
	// encoder was given yuv420p. Both describe compatible H.264 samples;
	// retain the range flag rather than forcing a lossy range conversion.
	compatiblePixels := probe.PixelFormat == "yuv420p" || probe.PixelFormat == "yuvj420p"
	return probe.VideoCodec == "h264" && compatiblePixels &&
		w == probe.Width && h == probe.Height && probe.Rotation == 0 &&
		(probe.SampleAspect == 0 || probe.SampleAspect == 1) && !probe.Interlaced && !videoHDR(probe)
}

func audioCopy(probe Probe) bool {
	for _, codec := range probe.AudioCodecs {
		if codec != "aac" {
			return false
		}
	}
	return true
}

func VideoConforms(probe Probe, filename string) bool {
	ext := strings.ToLower(filepath.Ext(filename))
	return (ext == ".mp4" || ext == ".m4v") && strings.Contains(probe.Container, "mp4") &&
		probe.VideoStreams == 1 && videoCopy(probe) && audioCopy(probe)
}

func (p *ffmpegProcessor) Video(ctx context.Context, input, output *os.File, source Probe) (Probe, error) {
	ffmpeg := p.FFmpeg
	if ffmpeg == "" {
		ffmpeg, _ = exec.LookPath("ffmpeg")
	}
	if ffmpeg == "" {
		return Probe{}, errors.New("ffmpeg with libx264 is required for video conversion")
	}
	w, h := VideoSize(source)
	if w <= 0 || h <= 0 || source.Duration <= 0 {
		return Probe{}, errors.New("video has no usable dimensions or duration")
	}
	if source.VideoStreams != 1 {
		return Probe{}, errors.New("videos with multiple image streams must be exported/stitched into a single video before importing")
	}
	ctx, cancel := context.WithTimeout(ctx, 6*time.Hour)
	defer cancel()
	if _, err := input.Seek(0, io.SeekStart); err != nil {
		return Probe{}, err
	}
	if err := output.Truncate(0); err != nil {
		return Probe{}, err
	}
	if _, err := output.Seek(0, io.SeekStart); err != nil {
		return Probe{}, err
	}
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-threads", "2", "-filter_threads", "1",
		"-i", "/dev/fd/3", "-map", "0:V:0", "-map", "0:a?", "-sn", "-dn", "-map_metadata", "0", "-map_chapters", "-1"}
	if videoCopy(source) {
		args = append(args, "-c:v", "copy")
	} else {
		filter := ""
		if source.Interlaced {
			filter = "bwdif=mode=send_frame,"
		}
		if videoHDR(source) {
			// zscale is included in the Docker image. A missing filter fails without
			// publishing or deleting the HDR original on other installations.
			filter += "zscale=t=linear:npl=100,format=gbrpf32le,tonemap=tonemap=hable:desat=0,zscale=p=bt709:t=bt709:m=bt709:r=limited,"
		}
		filter += "scale=" + strconv.Itoa(w) + ":" + strconv.Itoa(h) + ":flags=lanczos,setsar=1"
		args = append(args, "-vf", filter, "-c:v", "libx264", "-preset", "medium", "-crf", "23", "-pix_fmt", "yuv420p", "-fps_mode:v", "passthrough", "-threads", "2", "-metadata:s:v:0", "rotate=0")
		if videoHDR(source) {
			args = append(args, "-color_primaries", "bt709", "-color_trc", "bt709", "-colorspace", "bt709", "-color_range", "tv")
		}
	}
	if audioCopy(source) {
		args = append(args, "-c:a", "copy")
	} else {
		args = append(args, "-c:a", "aac", "-b:a", "160k")
	}
	// A streamable fragmented MP4 writes its initialization metadata first and
	// avoids reopening a descriptor during faststart (not portable on macOS).
	args = append(args, "-movflags", "+frag_keyframe+empty_moov+default_base_moof+use_metadata_tags", "-f", "mp4", "pipe:1")
	cmd := mediaCommand(ctx, ffmpeg, input, args...)
	cmd.Stdout = output
	stderr := &boundedOutput{limit: 8192}
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return Probe{}, fmt.Errorf("video conversion failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if err := output.Sync(); err != nil {
		return Probe{}, err
	}
	result, err := p.Inspect(ctx, output)
	if err != nil {
		return Probe{}, err
	}
	if !VideoConforms(result, "video.mp4") || result.Width > w || result.Height > h || math.Abs(result.Duration-source.Duration) > max(1.0, source.Duration*0.01) || len(result.AudioCodecs) != len(source.AudioCodecs) {
		return Probe{}, fmt.Errorf("encoded video failed verification: codec=%s pixels=%s size=%dx%d rotation=%d duration=%g (source %g) audio=%v", result.VideoCodec, result.PixelFormat, result.Width, result.Height, result.Rotation, result.Duration, source.Duration, result.AudioCodecs)
	}
	return result, nil
}
