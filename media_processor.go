package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type mediaProbe struct {
	Width, Height    int
	Duration         float64
	StereoLayout     string
	Projection       string
	ProjectionFormat string
	JSON             json.RawMessage
}

type mediaThumbnailOptions struct {
	StereoLayout     string
	Projection       string
	ProjectionFormat string
}

type mediaProcessor interface {
	Inspect(context.Context, *os.File) (mediaProbe, error)
	Thumbnail(context.Context, *os.File, *os.File, mediaThumbnailOptions) error
}

type ffmpegProcessor struct {
	ffmpeg, ffprobe string
}

func newMediaProcessor() *ffmpegProcessor {
	ffmpeg, _ := exec.LookPath("ffmpeg")
	ffprobe, _ := exec.LookPath("ffprobe")
	return &ffmpegProcessor{ffmpeg: ffmpeg, ffprobe: ffprobe}
}

// External tools receive an already opened file, confined by os.Root. The seekable
// descriptor also works for MOV/MP4 files whose metadata is at the end of the file.
// /dev/fd and ExtraFiles are supported on the Linux/macOS targets of this app.
func mediaCommand(ctx context.Context, executable string, input *os.File, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.ExtraFiles = []*os.File{input}
	cmd.WaitDelay = 2 * time.Second
	return cmd
}

type boundedOutput struct {
	bytes.Buffer
	limit int
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) > b.limit-b.Len() {
		p = p[:b.limit-b.Len()]
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}

func (p *ffmpegProcessor) Inspect(ctx context.Context, input *os.File) (mediaProbe, error) {
	ffprobe := p.ffprobe
	if ffprobe == "" {
		ffprobe, _ = exec.LookPath("ffprobe")
	}
	if ffprobe == "" {
		return mediaProbe{}, errors.New("ffprobe is required to inspect media; install FFmpeg and rescan")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if _, err := input.Seek(0, io.SeekStart); err != nil {
		return mediaProbe{}, err
	}
	output := &boundedOutput{limit: 2 << 20}
	stderr := &boundedOutput{limit: 4096}
	cmd := mediaCommand(ctx, ffprobe, input, "-v", "error", "-show_streams", "-show_format", "-of", "json", "/dev/fd/3")
	cmd.Stdout, cmd.Stderr = output, stderr
	if err := cmd.Run(); err != nil {
		return mediaProbe{}, fmt.Errorf("ffprobe failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return decodeMediaProbe(output.Bytes())
}

func decodeMediaProbe(data []byte) (mediaProbe, error) {
	var doc struct {
		Streams []struct {
			CodecType string            `json:"codec_type"`
			Width     int               `json:"width"`
			Height    int               `json:"height"`
			Duration  string            `json:"duration"`
			Tags      map[string]string `json:"tags"`
			SideData  []struct {
				Type         string `json:"type"`
				SideDataType string `json:"side_data_type"`
				Projection   string `json:"projection"`
			} `json:"side_data_list"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return mediaProbe{}, fmt.Errorf("decode ffprobe output: %w", err)
	}
	for _, stream := range doc.Streams {
		if stream.CodecType != "video" || stream.Width <= 0 || stream.Height <= 0 {
			continue
		}
		probe := mediaProbe{Width: stream.Width, Height: stream.Height, JSON: append(json.RawMessage{}, data...)}
		probe.Duration, _ = strconv.ParseFloat(doc.Format.Duration, 64)
		if probe.Duration == 0 {
			probe.Duration, _ = strconv.ParseFloat(stream.Duration, 64)
		}
		if math.IsNaN(probe.Duration) || math.IsInf(probe.Duration, 0) || probe.Duration < 0 {
			probe.Duration = 0
		}
		for _, data := range stream.SideData {
			if layout := detectedStereoLayout(data.Type); layout != "" {
				probe.StereoLayout = layout
			}
			if data.SideDataType == "Spherical Mapping" {
				switch data.Projection {
				case "equirectangular", "cubemap":
					probe.Projection, probe.ProjectionFormat = "360", data.Projection
				}
			}
		}
		if probe.StereoLayout == "" {
			probe.StereoLayout = detectedStereoLayout(stream.Tags["stereo_mode"])
		}
		return probe, nil
	}
	return mediaProbe{}, errors.New("file has no decodable image or video stream")
}

func detectedStereoLayout(value string) string {
	switch strings.ToLower(strings.ReplaceAll(value, "_", " ")) {
	case "side by side", "left right", "right left", "side by side (quincunx subsampling)":
		return "side_by_side"
	case "top and bottom", "top bottom", "bottom top":
		return "top_bottom"
	case "multiview", "multi view":
		return "multiview"
	}
	return ""
}

func (p *ffmpegProcessor) Thumbnail(ctx context.Context, input, output *os.File, options mediaThumbnailOptions) error {
	ffmpeg := p.ffmpeg
	if ffmpeg == "" {
		ffmpeg, _ = exec.LookPath("ffmpeg")
	}
	if ffmpeg == "" {
		return errors.New("ffmpeg is required for thumbnails; install FFmpeg and rescan")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if _, err := input.Seek(0, io.SeekStart); err != nil {
		return err
	}
	filter := ""
	switch options.StereoLayout {
	case "side_by_side":
		filter = "crop=iw/2:ih:0:0,"
	case "top_bottom":
		filter = "crop=iw:ih/2:0:0,"
	}
	filter += "thumbnail=30,"
	if options.Projection == "360" && options.ProjectionFormat == "equirectangular" {
		filter += "v360=input=equirect:output=flat:w=640:h=360:h_fov=90:v_fov=60"
	} else {
		filter += "scale=640:640:force_original_aspect_ratio=decrease"
	}
	cmd := mediaCommand(ctx, ffmpeg, input, "-hide_banner", "-loglevel", "error", "-nostdin",
		"-i", "/dev/fd/3", "-map", "0:v:0", "-an", "-sn", "-dn", "-vf", filter,
		"-frames:v", "1", "-threads", "1", "-c:v", "mjpeg", "-q:v", "3", "-f", "image2pipe", "pipe:1")
	stderr := &boundedOutput{limit: 4096}
	cmd.Stdout, cmd.Stderr = output, stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg thumbnail failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	info, err := output.Stat()
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		return errors.New("ffmpeg produced an empty thumbnail")
	}
	return nil
}
