package media

import (
	"context"
	"image"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestCaptureMetadataParsing(t *testing.T) {
	for _, test := range []struct {
		name, document, local, offset string
		missing, invalid              bool
	}{
		{"local", `[{"ExifIFD:DateTimeOriginal":"2026:08:15 11:13:14"}]`, "2026-08-15T11:13:14", "", false, false},
		{"offset", `[{"ExifIFD:DateTimeOriginal":"2026:08:15 11:13:14","ExifIFD:OffsetTimeOriginal":"+11:00","ExifIFD:SubSecTimeOriginal":"123"}]`, "2026-08-15T11:13:14.123", "+11:00", false, false},
		{"composite", `[{"Composite:SubSecDateTimeOriginal":"2026:08:15 11:13:14.123-1000"}]`, "2026-08-15T11:13:14.123", "-10:00", false, false},
		{"xmp", `[{"XMP-exif:DateTimeOriginal":"2026-08-15T11:13:14Z"}]`, "2026-08-15T11:13:14", "Z", false, false},
		{"video content date", `[{"Keys:CreationDate":"2026:08:15 11:13:14+11:00"}]`, "2026-08-15T11:13:14", "+11:00", false, false},
		{"video original date", `[{"UserData:DateTimeOriginal":"2026:08:15 11:13:14"}]`, "2026-08-15T11:13:14", "", false, false},
		{"video local header date", `[{"QuickTime:CreateDate":"2026:08:15 11:13:14"}]`, "2026-08-15T11:13:14", "", false, false},
		{"resolve export date only", `[{"QuickTime:CreateDate":"2026:08:15 11:13:14","UserData:Encoder":"Blackmagic Design DaVinci Resolve Studio"}]`, "", "", true, false},
		{"resolve with restored original", `[{"QuickTime:CreateDate":"2026:11:05 10:00:00","UserData:Encoder":"DaVinci Resolve","UserData:DateTimeOriginal":"2026:08:15 11:13:14+11:00"}]`, "2026-08-15T11:13:14", "+11:00", false, false},
		{"unset video time", `[{"QuickTime:CreateDate":"0000:00:00 00:00:00"}]`, "", "", true, false},
		{"gps", `[{"ExifIFD:DateTimeOriginal":"2026:08:15 11:13:14","Composite:GPSDateTime":"2026:08:15 00:13:14Z"}]`, "2026-08-15T00:13:14", "Z", false, false},
		{"ignore file dates", `[{"File:FileModifyDate":"2026:08:15 11:13:14Z"}]`, "", "", true, false},
		{"invalid date", `[{"ExifIFD:DateTimeOriginal":"2026:02:30 11:13:14"}]`, "", "", false, true},
		{"invalid offset", `[{"ExifIFD:DateTimeOriginal":"2026:08:15 11:13:14","ExifIFD:OffsetTimeOriginal":"+25:00"}]`, "", "", false, true},
		{"tool error", `[{"ExifTool:Error":"Not a valid image"}]`, "", "", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			capture, err := decodeCaptureMetadata([]byte(test.document))
			if test.invalid {
				if err == nil {
					t.Fatal("invalid metadata accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if test.missing {
				if capture != nil {
					t.Fatal("file timestamp used as capture date")
				}
				return
			}
			if capture == nil || capture.LocalTime != test.local || capture.Offset != test.offset {
				t.Fatalf("capture: %+v", capture)
			}
		})
	}
}

func TestExifToolCaptureJPEGAndHEIC(t *testing.T) {
	reader := NewCaptureReader()
	if reader.Path == "" {
		t.Skip("install exiftool")
	}
	for _, kind := range []string{"jpg", "heic"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "photo."+kind)
			if kind == "heic" {
				body, err := os.ReadFile("testdata/grid-small.heic")
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, body, 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				file, err := os.Create(path)
				if err != nil {
					t.Fatal(err)
				}
				err = jpeg.Encode(file, image.NewRGBA(image.Rect(0, 0, 32, 32)), nil)
				file.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command(reader.Path, "-config", "", "-overwrite_original", "-DateTimeOriginal=2026:08:15 11:13:14", "-OffsetTimeOriginal=+11:00", "-SubSecTimeOriginal=123", path)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("write synthetic capture metadata: %s %v", output, err)
			}
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			capture, err := reader.ReadCapture(context.Background(), file)
			if err != nil || capture == nil || capture.LocalTime != "2026-08-15T11:13:14.123" || capture.Offset != "+11:00" {
				t.Fatalf("capture: %+v %v", capture, err)
			}
		})
	}
}
