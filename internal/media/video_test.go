package media

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestVideoDisplaySizeAndConformance(t *testing.T) {
	for _, test := range []struct {
		name  string
		probe Probe
		w, h  int
	}{
		{"landscape", Probe{Width: 3840, Height: 2160}, 1920, 1080},
		{"portrait", Probe{Width: 2160, Height: 3840}, 1080, 1920},
		{"rotated", Probe{Width: 3840, Height: 2160, Rotation: 90}, 1080, 1920},
		{"small", Probe{Width: 640, Height: 360}, 640, 360},
		{"panorama", Probe{Width: 5760, Height: 2880}, 1920, 960},
		{"anamorphic", Probe{Width: 720, Height: 576, SampleAspect: 64.0 / 45}, 1024, 576},
		{"odd", Probe{Width: 641, Height: 361}, 640, 360},
	} {
		t.Run(test.name, func(t *testing.T) {
			w, h := VideoSize(test.probe)
			if w != test.w || h != test.h {
				t.Fatalf("size %dx%d, want %dx%d", w, h, test.w, test.h)
			}
		})
	}
	base := Probe{Width: 1280, Height: 720, SampleAspect: 1, VideoCodec: "h264", PixelFormat: "yuv420p", VideoStreams: 1, Container: "mov,mp4,m4a,3gp,3g2,mj2", AudioCodecs: []string{"aac"}}
	if !VideoConforms(base, "clip.MP4") {
		t.Fatal("compatible MP4 needs conversion")
	}
	for _, mutate := range []func(*Probe){func(p *Probe) { p.VideoCodec = "hevc" }, func(p *Probe) { p.PixelFormat = "yuv420p10le" }, func(p *Probe) { p.Rotation = 90 }, func(p *Probe) { p.Width = 3840; p.Height = 2160 }, func(p *Probe) { p.AudioCodecs = []string{"pcm_s16le"} }, func(p *Probe) { p.VideoStreams = 2 }, func(p *Probe) { p.ColorTransfer = "smpte2084" }, func(p *Probe) { p.Interlaced = true }} {
		p := base
		mutate(&p)
		if VideoConforms(p, "clip.mp4") {
			t.Fatalf("incompatible video accepted: %+v", p)
		}
	}
	if VideoConforms(base, "clip.mov") {
		t.Fatal("MOV needs MP4 remux")
	}
}

func videoFixture(t *testing.T, name string, args ...string) string {
	t.Helper()
	p := NewProcessor()
	if p.FFmpeg == "" || p.FFprobe == "" {
		t.Skip("install FFmpeg")
	}
	path := filepath.Join(t.TempDir(), name)
	command := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-threads", "1", "-filter_threads", "1"}
	command = append(command, args...)
	command = append(command, "-threads", "1", path)
	if output, err := exec.Command(p.FFmpeg, command...).CombinedOutput(); err != nil {
		t.Fatalf("video fixture: %s %v", output, err)
	}
	return path
}

func TestVideoFFmpegNormalization(t *testing.T) {
	for _, test := range []struct {
		name, ext, size, codec, pixfmt string
		audio                          bool
		w, h                           int
	}{
		{"4K", "mp4", "3840x2160", "libx264", "yuv420p", false, 1920, 1080},
		{"portrait", "mp4", "1080x2160", "libx264", "yuv420p", false, 960, 1920},
		{"hevc", "mp4", "320x180", "libx265", "yuv420p", false, 320, 180},
		{"remux with audio", "mov", "320x180", "libx264", "yuv420p", true, 320, 180},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := []string{"-f", "lavfi", "-i", "color=c=blue:s=" + test.size + ":r=5"}
			if test.audio {
				args = append(args, "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-c:a", "pcm_s16le")
			}
			args = append(args, "-t", "0.4", "-c:v", test.codec, "-preset", "ultrafast", "-pix_fmt", test.pixfmt)
			if test.codec == "libx265" {
				args = append(args, "-x265-params", "pools=none:frame-threads=1:log-level=error")
			}
			path := videoFixture(t, "source."+test.ext, args...)
			input, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer input.Close()
			p := NewProcessor()
			source, err := p.Inspect(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			out, err := os.CreateTemp(t.TempDir(), "converted-")
			if err != nil {
				t.Fatal(err)
			}
			defer out.Close()
			result, err := p.Video(context.Background(), input, out, source)
			if err != nil || result.Width != test.w || result.Height != test.h || !VideoConforms(result, "video.mp4") {
				t.Fatalf("encoded: %+v %v", result, err)
			}
			if test.audio && (len(result.AudioCodecs) != 1 || result.AudioCodecs[0] != "aac") {
				t.Fatal("audio not converted to AAC")
			}
			body, _ := os.ReadFile(out.Name())
			moov, mdat := bytes.Index(body, []byte("moov")), bytes.Index(body, []byte("mdat"))
			if moov < 0 || mdat < 0 || moov > mdat {
				t.Fatal("MP4 is not faststart")
			}
		})
	}
}

func TestVideoFFmpegRotationAndHDR(t *testing.T) {
	t.Run("rotation", func(t *testing.T) {
		base := videoFixture(t, "base.mp4", "-f", "lavfi", "-i", "color=c=red:s=320x180:r=5", "-t", "0.4", "-c:v", "libx264", "-pix_fmt", "yuv420p")
		rotated := videoFixture(t, "rotated.mp4", "-display_rotation", "90", "-i", base, "-c", "copy")
		input, _ := os.Open(rotated)
		defer input.Close()
		out, _ := os.CreateTemp(t.TempDir(), "out-")
		defer out.Close()
		p := NewProcessor()
		source, err := p.Inspect(context.Background(), input)
		if err != nil || source.Rotation == 0 {
			t.Fatalf("rotation fixture: %+v %v", source, err)
		}
		result, err := p.Video(context.Background(), input, out, source)
		if err != nil || result.Width != 180 || result.Height != 320 || result.Rotation != 0 {
			t.Fatalf("rotation result: %+v %v", result, err)
		}
	})
	t.Run("HDR", func(t *testing.T) {
		p := NewProcessor()
		if p.FFmpeg == "" {
			t.Skip("install FFmpeg")
		}
		filters, _ := exec.Command(p.FFmpeg, "-hide_banner", "-filters").CombinedOutput()
		if !strings.Contains(string(filters), " zscale ") {
			t.Skip("FFmpeg needs zscale; available in Docker")
		}
		for _, transfer := range []string{"smpte2084", "arib-std-b67"} {
			t.Run(transfer, func(t *testing.T) {
				path := videoFixture(t, "hdr.mp4", "-f", "lavfi", "-i", "color=c=white:s=160x90:r=5", "-t", "0.4", "-vf", "format=yuv420p10le,setparams=color_primaries=bt2020:color_trc="+transfer+":colorspace=bt2020nc:range=limited", "-c:v", "libx265", "-preset", "ultrafast", "-x265-params", "pools=none:frame-threads=1:log-level=error:colorprim=bt2020:transfer="+transfer+":colormatrix=bt2020nc")
				input, _ := os.Open(path)
				defer input.Close()
				out, _ := os.CreateTemp(t.TempDir(), "out-")
				defer out.Close()
				source, err := p.Inspect(context.Background(), input)
				if err != nil || !videoHDR(source) {
					t.Fatalf("HDR fixture: %+v %v", source, err)
				}
				result, err := p.Video(context.Background(), input, out, source)
				if err != nil || result.ColorTransfer != "bt709" || !VideoConforms(result, "out.mp4") {
					t.Fatalf("HDR result: %+v %v", result, err)
				}
			})
		}
	})
}

func TestExifToolVideoCaptureAndResolveExports(t *testing.T) {
	reader := NewCaptureReader()
	if reader.Path == "" {
		t.Skip("install ExifTool")
	}
	path := videoFixture(t, "GoPro.mp4", "-f", "lavfi", "-i", "color=c=blue:s=160x90:r=5", "-t", "0.4", "-c:v", "libx264", "-pix_fmt", "yuv420p")
	write := func(args ...string) {
		t.Helper()
		args = append([]string{"-config", "", "-overwrite_original"}, args...)
		args = append(args, path)
		if output, err := exec.Command(reader.Path, args...).CombinedOutput(); err != nil {
			t.Fatalf("write video metadata: %s %v", output, err)
		}
	}
	read := func() *CaptureTime {
		t.Helper()
		input, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer input.Close()
		capture, err := reader.ReadCapture(context.Background(), input)
		if err != nil {
			t.Fatal(err)
		}
		return capture
	}
	write("-QuickTime:CreateDate=2026:08:15 10:30:00", "-ItemList:Encoder=", "-UserData:Encoder=GoPro")
	if capture := read(); capture == nil || capture.LocalTime != "2026-08-15T10:30:00" || capture.Offset != "" {
		metadata, _ := exec.Command(reader.Path, "-G1", "-a", "-s", "-QuickTime:CreateDate", "-Encoder", "-Software", "-SoftwareVersion", path).CombinedOutput()
		t.Fatalf("GoPro local date: %+v; metadata: %s", capture, metadata)
	}
	write("-UserData:Encoder=Blackmagic Design DaVinci Resolve Studio")
	if capture := read(); capture != nil {
		t.Fatalf("export date used as capture: %+v", capture)
	}
	write("-Keys:CreationDate=2026:08:15 10:30:00+11:00")
	if capture := read(); capture == nil || capture.LocalTime != "2026-08-15T10:30:00" || capture.Offset != "+11:00" {
		t.Fatalf("restored capture: %+v", capture)
	}
}
