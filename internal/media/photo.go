package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

type PhotoOptions struct {
	SmallSide int
	Distance  float64
	Effort    int
	Preview   bool
	ThumbnailOptions
}

type PhotoProcessor interface {
	Photo(context.Context, *os.File, *os.File, PhotoOptions) (Probe, error)
	JPEG(context.Context, *os.File, *os.File) error
}

func IsJPEGXL(input *os.File) bool {
	var signature [12]byte
	n, _ := input.ReadAt(signature[:], 0)
	return n >= 2 && signature[0] == 0xff && signature[1] == 0x0a || n == 12 && bytes.Equal(signature[:], []byte{0, 0, 0, 12, 'J', 'X', 'L', ' ', 13, 10, 135, 10})
}

func runPhotoTool(ctx context.Context, executable string, input *os.File, args ...string) error {
	if executable == "" {
		return errors.New("required photo tool is not installed")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.WaitDelay = 2 * time.Second
	if input != nil {
		if _, err := input.Seek(0, io.SeekStart); err != nil {
			return err
		}
		cmd.ExtraFiles = []*os.File{input}
	}
	stderr := &boundedOutput{limit: 4096}
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s failed: %w: %s", filepath.Base(executable), err, stderr.String())
	}
	return nil
}

func decodeJPEGXL(ctx context.Context, input *os.File, output string) error {
	tool, err := exec.LookPath("djxl")
	if err != nil {
		return errors.New("djxl is required for JPEG XL photos; install jpeg-xl/libjxl-tools")
	}
	return runPhotoTool(ctx, tool, input, "/dev/fd/3", output, "--num_threads=1")
}

func photoProbe(path, codec string) (Probe, error) {
	file, err := os.Open(path)
	if err != nil {
		return Probe{}, err
	}
	defer file.Close()
	config, err := png.DecodeConfig(file)
	if err != nil {
		return Probe{}, fmt.Errorf("verify decoded photo: %w", err)
	}
	if config.Width <= 0 || config.Height <= 0 {
		return Probe{}, errors.New("decoded photo has invalid dimensions")
	}
	probe := Probe{Width: config.Width, Height: config.Height}
	probe.JSON, err = json.Marshal(map[string]any{"streams": []map[string]any{{"codec_type": "video", "codec_name": codec, "width": probe.Width, "height": probe.Height}}})
	return probe, err
}

func (p *ffmpegProcessor) inspectJPEGXL(ctx context.Context, input *os.File) (Probe, error) {
	dir, err := os.MkdirTemp("", "garmindives-photo-")
	if err != nil {
		return Probe{}, err
	}
	defer os.RemoveAll(dir)
	decoded := filepath.Join(dir, "decoded.png")
	if err := decodeJPEGXL(ctx, input, decoded); err != nil {
		return Probe{}, err
	}
	return photoProbe(decoded, "jpegxl")
}

func (p *ffmpegProcessor) renderPhoto(ctx context.Context, input *os.File, dir string, options PhotoOptions) (string, error) {
	if IsJPEGXL(input) {
		decoded := filepath.Join(dir, "input.png")
		if err := decodeJPEGXL(ctx, input, decoded); err != nil {
			return "", err
		}
		file, err := os.Open(decoded)
		if err != nil {
			return "", err
		}
		defer file.Close()
		input = file
	} else if IsHEIF(input) {
		decoded := filepath.Join(dir, "heif.png")
		if err := p.decodeHEIF(ctx, input, decoded); err != nil {
			return "", err
		}
		file, err := os.Open(decoded)
		if err != nil {
			return "", err
		}
		defer file.Close()
		input = file
	}
	ffmpeg := p.FFmpeg
	if ffmpeg == "" {
		ffmpeg, _ = exec.LookPath("ffmpeg")
	}
	filter := ""
	if options.Preview {
		switch options.StereoLayout {
		case "side_by_side":
			filter = "crop=iw/2:ih:0:0,"
		case "top_bottom":
			filter = "crop=iw:ih/2:0:0,"
		}
		if options.Projection == "360" && options.ProjectionFormat == "equirectangular" {
			probe, err := p.Inspect(ctx, input)
			if err != nil {
				return "", err
			}
			height := max(1, min(options.SmallSide, min(probe.Width, probe.Height)))
			filter += fmt.Sprintf("v360=input=equirect:output=flat:w=%d:h=%d:h_fov=90:v_fov=60,", max(1, (height*16+4)/9), height)
		}
	}
	if options.SmallSide > 0 {
		filter += fmt.Sprintf("scale=w='max(1,round(iw*min(1,%d/min(iw,ih))))':h='max(1,round(ih*min(1,%d/min(iw,ih))))'", options.SmallSide, options.SmallSide)
	} else {
		filter += "null"
	}
	output := filepath.Join(dir, "resized.png")
	err := runPhotoTool(ctx, ffmpeg, input, "-hide_banner", "-loglevel", "error", "-nostdin", "-i", "/dev/fd/3", "-map", "0:v:0", "-an", "-sn", "-dn", "-vf", filter, "-frames:v", "1", "-threads", "1", "-c:v", "png", "-y", output)
	return output, err
}

func (p *ffmpegProcessor) Photo(ctx context.Context, input, output *os.File, options PhotoOptions) (Probe, error) {
	cjxl, err := exec.LookPath("cjxl")
	if err != nil {
		return Probe{}, errors.New("cjxl is required for photo processing; install jpeg-xl/libjxl-tools")
	}
	dir, err := os.MkdirTemp("", "garmindives-photo-")
	if err != nil {
		return Probe{}, err
	}
	defer os.RemoveAll(dir)
	resized, err := p.renderPhoto(ctx, input, dir, options)
	if err != nil {
		return Probe{}, err
	}
	encoded := filepath.Join(dir, "output.jxl")
	if err := runPhotoTool(ctx, cjxl, nil, resized, encoded, "-d", strconv.FormatFloat(options.Distance, 'f', -1, 64), "-e", strconv.Itoa(options.Effort), "--num_threads=1"); err != nil {
		return Probe{}, err
	}
	file, err := os.Open(encoded)
	if err != nil {
		return Probe{}, err
	}
	defer file.Close()
	verified := filepath.Join(dir, "verified.png")
	if err := decodeJPEGXL(ctx, file, verified); err != nil {
		return Probe{}, err
	}
	probe, err := photoProbe(verified, "jpegxl")
	if err != nil {
		return Probe{}, err
	}
	expected, err := photoProbe(resized, "png")
	if err != nil || probe.Width != expected.Width || probe.Height != expected.Height {
		return Probe{}, errors.New("encoded photo dimensions failed verification")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return Probe{}, err
	}
	if _, err := io.Copy(output, file); err != nil {
		return Probe{}, err
	}
	if err := output.Sync(); err != nil {
		return Probe{}, err
	}
	return probe, nil
}

func (p *ffmpegProcessor) JPEG(ctx context.Context, input, output *os.File) error {
	dir, err := os.MkdirTemp("", "garmindives-fallback-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	decoded := filepath.Join(dir, "decoded.png")
	if IsJPEGXL(input) {
		err = decodeJPEGXL(ctx, input, decoded)
	} else {
		decoded, err = p.renderPhoto(ctx, input, dir, PhotoOptions{})
	}
	if err != nil {
		return err
	}
	file, err := os.Open(decoded)
	if err != nil {
		return err
	}
	defer file.Close()
	source, err := png.Decode(file)
	if err != nil {
		return err
	}
	canvas := image.NewRGBA(source.Bounds())
	draw.Draw(canvas, canvas.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(canvas, canvas.Bounds(), source, source.Bounds().Min, draw.Over)
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := jpeg.Encode(output, canvas, &jpeg.Options{Quality: 90}); err != nil {
		return err
	}
	return output.Sync()
}
