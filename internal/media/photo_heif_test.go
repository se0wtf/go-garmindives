package media

import (
	"context"
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func checkHEIFQuadrants(t *testing.T, photo image.Image, width, height int) {
	t.Helper()
	if photo.Bounds().Dx() != width || photo.Bounds().Dy() != height {
		t.Fatalf("HEIF was cropped or rotated incorrectly: %v, expected %dx%d", photo.Bounds(), width, height)
	}
	for _, point := range []struct{ x, y, r, g, b int }{
		{width / 4, height / 4, 20, 20, 230}, {3 * width / 4, height / 4, 230, 20, 20},
		{width / 4, 3 * height / 4, 230, 230, 20}, {3 * width / 4, 3 * height / 4, 20, 230, 20},
	} {
		r, g, b, _ := photo.At(point.x, point.y).RGBA()
		if abs(int(r>>8)-point.r) > 25 || abs(int(g>>8)-point.g) > 25 || abs(int(b>>8)-point.b) > 25 {
			t.Fatalf("missing/misplaced HEIF quadrant at %d,%d: RGB=%d,%d,%d", point.x, point.y, r>>8, g>>8, b>>8)
		}
	}
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func TestHEIFGridInspectionConversionAndFallback(t *testing.T) {
	processor := NewProcessor()
	if processor.HEIF == "" || processor.FFmpeg == "" {
		t.Skip("install libheif and FFmpeg")
	}
	for _, tool := range []string{"cjxl", "djxl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("install JPEG XL tools")
		}
	}
	input, err := os.Open("testdata/grid-rotated.heic")
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	if !IsHEIF(input) {
		t.Fatal("HEIF signature not detected")
	}
	probe, err := processor.Inspect(context.Background(), input)
	if err != nil || probe.Width != 2048 || probe.Height != 3072 {
		t.Fatalf("primary HEIF image: %+v %v", probe, err)
	}
	dir := t.TempDir()
	for _, test := range []struct {
		name          string
		options       PhotoOptions
		width, height int
	}{
		{"master", PhotoOptions{SmallSide: 1080, Distance: 1.5, Effort: 7}, 1080, 1620},
		{"thumbnail", PhotoOptions{SmallSide: 480, Distance: 2, Effort: 8, Preview: true}, 480, 720},
	} {
		t.Run(test.name, func(t *testing.T) {
			output, err := os.Create(filepath.Join(dir, test.name+".jxl"))
			if err != nil {
				t.Fatal(err)
			}
			defer output.Close()
			probe, err := processor.Photo(context.Background(), input, output, test.options)
			if err != nil || probe.Width != test.width || probe.Height != test.height {
				t.Fatalf("HEIF output: %+v %v", probe, err)
			}
			decoded := filepath.Join(dir, test.name+".png")
			if err := decodeJPEGXL(context.Background(), output, decoded); err != nil {
				t.Fatal(err)
			}
			file, err := os.Open(decoded)
			if err != nil {
				t.Fatal(err)
			}
			photo, err := png.Decode(file)
			file.Close()
			if err != nil {
				t.Fatal(err)
			}
			checkHEIFQuadrants(t, photo, test.width, test.height)
		})
	}
	// Small retained HEICs use this direct JPEG path in browsers as well.
	fallback, err := os.Create(filepath.Join(dir, "fallback.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	defer fallback.Close()
	if err := processor.JPEG(context.Background(), input, fallback); err != nil {
		t.Fatal(err)
	}
	fallback.Seek(0, 0)
	photo, err := jpeg.Decode(fallback)
	if err != nil {
		t.Fatal(err)
	}
	checkHEIFQuadrants(t, photo, 2048, 3072)
}

func TestHEIFMissingDecoderNeverFallsBackToTile(t *testing.T) {
	processor := NewProcessor()
	processor.HEIF = ""
	t.Setenv("PATH", "")
	input, err := os.Open("testdata/grid-rotated.heic")
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	if _, err := processor.Inspect(context.Background(), input); err == nil || !strings.Contains(err.Error(), "heif-convert/heif-dec") {
		t.Fatalf("missing HEIF decoder: %v", err)
	}
}
