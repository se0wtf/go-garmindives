package media

import (
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestJPEGXLPhotoEncodingAndJPEGFallback(t *testing.T) {
	for _, tool := range []string{"ffmpeg", "ffprobe", "cjxl", "djxl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("install FFmpeg and JPEG XL tools for photo integration tests")
		}
	}
	directory := t.TempDir()
	input, err := os.Create(filepath.Join(directory, "source.png"))
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	pixels := image.NewRGBA(image.Rect(0, 0, 2400, 2200))
	for y := 0; y < 2200; y++ {
		for x := 0; x < 2400; x++ {
			pixels.SetRGBA(x, y, color.RGBA{R: uint8(x % 255), G: uint8(y % 255), B: 90, A: 255})
		}
	}
	if err := png.Encode(input, pixels); err != nil {
		t.Fatal(err)
	}
	processor := NewProcessor()
	encoded, err := os.Create(filepath.Join(directory, "master.jxl"))
	if err != nil {
		t.Fatal(err)
	}
	defer encoded.Close()
	probe, err := processor.Photo(context.Background(), input, encoded, PhotoOptions{SmallSide: 1080, Distance: 1.5, Effort: 7})
	if err != nil {
		t.Fatal(err)
	}
	if min(probe.Width, probe.Height) != 1080 || !IsJPEGXL(encoded) {
		t.Fatalf("wrong master: %+v", probe)
	}
	inspected, err := processor.Inspect(context.Background(), encoded)
	if err != nil || inspected.Width != probe.Width || inspected.Height != probe.Height {
		t.Fatalf("JXL inspection failed: %+v %v", inspected, err)
	}
	thumb, err := os.Create(filepath.Join(directory, "thumb.jxl"))
	if err != nil {
		t.Fatal(err)
	}
	defer thumb.Close()
	preview, err := processor.Photo(context.Background(), encoded, thumb, PhotoOptions{SmallSide: 480, Distance: 2, Effort: 8, Preview: true})
	if err != nil || min(preview.Width, preview.Height) != 480 {
		t.Fatalf("wrong thumbnail: %+v %v", preview, err)
	}
	fallback, err := os.Create(filepath.Join(directory, "fallback.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	defer fallback.Close()
	if err := processor.JPEG(context.Background(), encoded, fallback); err != nil {
		t.Fatal(err)
	}
	if _, err := fallback.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	decoded, err := jpeg.Decode(fallback)
	if err != nil || decoded.Bounds().Dx() != probe.Width || decoded.Bounds().Dy() != probe.Height {
		t.Fatalf("wrong fallback: %v", err)
	}
	// A second pass targeting a larger size must not upscale an existing thumbnail.
	kept, err := os.Create(filepath.Join(directory, "kept.jxl"))
	if err != nil {
		t.Fatal(err)
	}
	defer kept.Close()
	unchanged, err := processor.Photo(context.Background(), thumb, kept, PhotoOptions{SmallSide: 1080, Distance: 1.5, Effort: 7})
	if err != nil || unchanged.Width != preview.Width || unchanged.Height != preview.Height {
		t.Fatalf("photo was upscaled: %+v %v", unchanged, err)
	}
}
