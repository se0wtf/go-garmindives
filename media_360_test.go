package main

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"strings"
	"testing"
)

func sphericalProbeJSON(t *testing.T, sideData any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"streams": []any{map[string]any{
			"codec_type": "video", "width": 1920, "height": 960,
			"side_data_list": sideData,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestMediaDetects360ProjectionFromSphericalMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, sideType, format, want string
	}{
		{"equirectangular sphere", "Spherical Mapping", "equirectangular", "360"},
		{"cubemap sphere", "Spherical Mapping", "cubemap", "360"},
		{"partial sphere", "Spherical Mapping", "tiled equirectangular", ""},
		{"unrelated side data", "Display Matrix", "equirectangular", ""},
		{"untagged wide video", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := sphericalProbeJSON(t, []any{map[string]any{
				"side_data_type": tc.sideType, "projection": tc.format,
			}})
			probe, err := decodeMediaProbe(raw)
			if err != nil || probe.Projection != tc.want {
				t.Fatalf("detected projection=%q, want=%q: %v", probe.Projection, tc.want, err)
			}
			m := mediaItem{Projection: "auto", Probe: raw}
			m.resolveProjection()
			if tc.want == "" {
				if m.EffectiveProjection != "flat" || m.ProjectionFormat != "" {
					t.Fatalf("unsupported/untagged media inferred as a sphere: %+v", m)
				}
			} else if m.EffectiveProjection != "360" || m.ProjectionFormat != tc.format {
				t.Fatalf("sphere metadata not exposed to frontend: %+v", m)
			}
		})
	}
}

func TestMedia360AutoDetectionOverridesAndRescans(t *testing.T) {
	l, processor := mediaTestLibrary(t)
	processor.probe = sphericalProbeJSON(t, []any{map[string]string{
		"side_data_type": "Spherical Mapping", "projection": "equirectangular",
	}})
	ctx := context.Background()
	m, err := l.Upload(ctx, 42, "Ocean 360.MP4", strings.NewReader("original spherical container"), nil)
	if err != nil || m.Projection != "auto" || m.EffectiveProjection != "360" || m.ProjectionFormat != "equirectangular" || processor.projection != "360" {
		t.Fatalf("360 import = %+v, %v", m, err)
	}
	originalSHA, originalKey := m.SHA256, m.ThumbnailKey
	flat := "flat"
	m, err = l.Patch(ctx, 42, m.ID, mediaPatch{Projection: &flat})
	if err != nil || m.EffectiveProjection != "flat" || processor.projection != "flat" || m.ThumbnailKey == originalKey {
		t.Fatalf("flat override did not refresh thumbnail: %+v, %v", m, err)
	}
	if _, err := l.Scan(ctx, true); err != nil {
		t.Fatal(err)
	}
	saved, err := l.store.mediaByID(ctx, 42, m.ID)
	if err != nil || saved.Projection != "flat" || saved.EffectiveProjection != "flat" || saved.SHA256 != originalSHA {
		t.Fatalf("scan lost explicit projection override: %+v, %v", saved, err)
	}
	auto := "auto"
	m, err = l.Patch(ctx, 42, m.ID, mediaPatch{Projection: &auto})
	if err != nil || m.EffectiveProjection != "360" || processor.projection != "360" || m.SHA256 != originalSHA {
		t.Fatalf("restore automatic detection = %+v, %v", m, err)
	}
	// A saved thumbnail from an older algorithm must be rebuilt even when the
	// original's size and mtime are unchanged.
	if err := l.root.Rename(".thumbnails/"+m.ThumbnailKey+".jpg", ".thumbnails/legacy.jpg"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.store.db.Exec(`UPDATE dive_media SET thumbnail_key = 'legacy' WHERE id = ?`, m.ID); err != nil {
		t.Fatal(err)
	}
	scan, err := l.Scan(ctx, false)
	if err != nil || scan.Updated != 1 || scan.Unchanged != 0 {
		t.Fatalf("old thumbnail refresh = %+v, %v", scan, err)
	}
	// Untagged footage remains flat until marked explicitly, including raw camera
	// containers whose extension alone does not describe the texture projection.
	processor.probe = nil
	untagged, err := l.Upload(ctx, 42, "raw camera.360", strings.NewReader("camera container"), nil)
	if err != nil || untagged.EffectiveProjection != "flat" {
		t.Fatalf("raw extension guessed a projection: %+v, %v", untagged, err)
	}
}

func TestMediaFFmpeg360PerspectiveThumbnailPreservesOriginal(t *testing.T) {
	p := newMediaProcessor()
	if p.ffmpeg == "" || p.ffprobe == "" {
		t.Skip("install FFmpeg for the 360-degree perspective preview test")
	}
	l, _ := mediaTestLibrary(t)
	l.processor = p
	panorama := image.NewRGBA(image.Rect(0, 0, 800, 400))
	for y := 0; y < 400; y++ {
		for x := 0; x < 800; x++ {
			c := color.RGBA{G: 230, A: 255}
			if x < 200 {
				c = color.RGBA{R: 230, A: 255}
			} else if x >= 600 {
				c = color.RGBA{B: 230, A: 255}
			}
			panorama.SetRGBA(x, y, c)
		}
	}
	var data bytes.Buffer
	if err := jpeg.Encode(&data, panorama, nil); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	m, err := l.Upload(ctx, 42, "Reef panorama.jpg", bytes.NewReader(data.Bytes()), nil)
	if err != nil || m.ThumbnailStatus != "ready" || m.EffectiveProjection != "flat" {
		t.Fatalf("untagged panorama = %+v, %v", m, err)
	}
	oldURL, originalSHA := m.ThumbnailURL, m.SHA256
	projection := "360"
	m, err = l.Patch(ctx, 42, m.ID, mediaPatch{Projection: &projection})
	if err != nil || m.ThumbnailStatus != "ready" || m.ThumbnailURL == oldURL || m.EffectiveProjection != "360" {
		t.Fatalf("360 perspective preview = %+v, %v", m, err)
	}
	thumbnail, err := l.root.ReadFile(".thumbnails/" + m.ThumbnailKey + ".jpg")
	if err != nil {
		t.Fatal(err)
	}
	preview, err := jpeg.Decode(bytes.NewReader(thumbnail))
	if err != nil {
		t.Fatal(err)
	}
	if preview.Bounds().Dx() != 640 || preview.Bounds().Dy() != 360 {
		t.Fatalf("perspective preview dimensions = %v", preview.Bounds())
	}
	for _, x := range []int{20, 320, 620} {
		r, g, b, _ := preview.At(x, 180).RGBA()
		if g <= r || g <= b {
			t.Fatalf("preview includes outside the forward perspective at x=%d: %d/%d/%d", x, r, g, b)
		}
	}
	original, err := l.root.ReadFile("42/Reef panorama.jpg")
	if err != nil || !bytes.Equal(original, data.Bytes()) || m.SHA256 != originalSHA {
		t.Fatalf("360 processing modified the original: %v", err)
	}
}
