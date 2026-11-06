package media

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// CaptureTime preserves the camera's wall clock. An empty Offset means the
// caller must interpret it in each candidate dive's local timezone.
type CaptureTime struct {
	LocalTime string `json:"local_time"`
	Offset    string `json:"offset,omitempty"`
	Source    string `json:"source"`
}

type CaptureReader interface {
	ReadCapture(context.Context, *os.File) (*CaptureTime, error)
}
type ExifTool struct{ Path string }

func NewCaptureReader() *ExifTool { path, _ := exec.LookPath("exiftool"); return &ExifTool{Path: path} }

func (reader *ExifTool) ReadCapture(ctx context.Context, input *os.File) (*CaptureTime, error) {
	path := reader.Path
	if path == "" {
		path, _ = exec.LookPath("exiftool")
	}
	if path == "" {
		return nil, errors.New("exiftool is required to read photo capture dates")
	}
	if _, err := input.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	// Disable user configuration; read only capture-related tags, never filesystem
	// timestamps. No filenames or arguments from embedded metadata are executed.
	cmd := exec.CommandContext(ctx, path, "-config", "", "-json", "-G1", "-s",
		"-EXIF:DateTimeOriginal", "-EXIF:OffsetTimeOriginal", "-EXIF:SubSecTimeOriginal",
		"-Composite:SubSecDateTimeOriginal", "-Composite:GPSDateTime",
		"-XMP:DateTimeOriginal", "-XMP:DateCreated", "-QuickTime:CreationDate", "-QuickTime:DateTimeOriginal",
		"-QuickTime:CreateDate", "-Encoder", "-Software", "-SoftwareVersion", "/dev/fd/3")
	cmd.ExtraFiles = []*os.File{input}
	cmd.WaitDelay = 2 * time.Second
	out, stderr := &boundedOutput{limit: 64 << 10}, &boundedOutput{limit: 4096}
	cmd.Stdout, cmd.Stderr = out, stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("read capture metadata: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return decodeCaptureMetadata(out.Bytes())
}

var capturePattern = regexp.MustCompile(`^(\d{4})[:-](\d{2})[:-](\d{2})[ T](\d{2}):(\d{2}):(\d{2})(\.\d{1,9})?(Z|[+-]\d{2}:?\d{2})?$`)

func parseCapture(value, offset, source string) (*CaptureTime, error) {
	parts := capturePattern.FindStringSubmatch(strings.TrimSpace(value))
	if parts == nil {
		return nil, errors.New("invalid capture date")
	}
	wall := parts[1] + "-" + parts[2] + "-" + parts[3] + "T" + parts[4] + ":" + parts[5] + ":" + parts[6] + parts[7]
	parsed, err := time.Parse("2006-01-02T15:04:05.999999999", wall)
	if err != nil || parsed.Year() < 1 {
		return nil, errors.New("invalid capture date")
	}
	zone := parts[8]
	if zone == "" {
		zone = strings.TrimSpace(offset)
	}
	if len(zone) == 5 && (zone[0] == '+' || zone[0] == '-') {
		zone = zone[:3] + ":" + zone[3:]
	}
	if zone != "" {
		// Parse permits some nonstandard offsets; explicitly enforce minute/hour bounds.
		if zone != "Z" && (len(zone) != 6 || zone[3] != ':' || zone[1:3] > "14" || zone[4:6] > "59" || zone[1:3] == "14" && zone[4:6] != "00") {
			return nil, errors.New("invalid capture timezone offset")
		}
		if _, err := time.Parse(time.RFC3339Nano, wall+zone); err != nil {
			return nil, errors.New("invalid capture timezone offset")
		}
	}
	return &CaptureTime{LocalTime: parsed.Format("2006-01-02T15:04:05.999999999"), Offset: zone, Source: source}, nil
}

func decodeCaptureMetadata(data []byte) (*CaptureTime, error) {
	var documents []map[string]json.RawMessage
	if err := json.Unmarshal(data, &documents); err != nil || len(documents) != 1 {
		return nil, errors.New("invalid capture metadata response")
	}
	doc := documents[0]
	get := func(keys ...string) string {
		for _, key := range keys {
			var value string
			if json.Unmarshal(doc[key], &value) == nil && value != "" {
				return value
			}
		}
		return ""
	}
	if message := get("ExifTool:Error", "Error"); message != "" {
		return nil, fmt.Errorf("capture metadata error: %s", message)
	}
	var floating *CaptureTime
	for _, tag := range []string{"Composite:SubSecDateTimeOriginal", "ExifIFD:DateTimeOriginal", "XMP-exif:DateTimeOriginal", "XMP-photoshop:DateCreated", "Keys:CreationDate", "UserData:DateTimeOriginal", "QuickTime:DateTimeOriginal"} {
		value := get(tag)
		if value == "" {
			continue
		}
		offset := ""
		if tag == "ExifIFD:DateTimeOriginal" {
			offset = get("ExifIFD:OffsetTimeOriginal")
			if fraction := get("ExifIFD:SubSecTimeOriginal"); fraction != "" && !strings.Contains(value, ".") {
				value += "." + fraction
			}
		}
		capture, err := parseCapture(value, offset, tag)
		if err != nil {
			return nil, err
		}
		if capture.Offset != "" {
			return capture, nil
		}
		if floating == nil {
			floating = capture
		}
	}
	if gps := get("Composite:GPSDateTime"); gps != "" {
		return parseCapture(gps, "Z", "Composite:GPSDateTime")
	}
	if floating == nil {
		// Movie-header creation time can be the render date. Resolve and other
		// recognizable editors need an explicit original capture tag or manual
		// assignment, even if their render timestamp happens to fit a dive.
		for key := range doc {
			if strings.HasSuffix(key, ":Encoder") || strings.HasSuffix(key, ":Software") || strings.HasSuffix(key, ":SoftwareVersion") {
				value := strings.ToLower(get(key))
				for _, editor := range []string{"davinci", "resolve", "lavf", "handbrake", "premiere", "final cut"} {
					if strings.Contains(value, editor) {
						return nil, nil
					}
				}
			}
		}
		if value := get("QuickTime:CreateDate"); value != "" && !strings.HasPrefix(value, "0000:") && !strings.HasPrefix(value, "1904:01:01 00:00:00") {
			return parseCapture(value, "", "QuickTime:CreateDate")
		}
	}
	return floating, nil
}
