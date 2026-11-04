package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"mime"
	"os"
	"strconv"
	"strings"

	mediaproc "github.com/se0wtf/go-garmindives/internal/media"
)

func photoFallbackKey(source string) string {
	sum := sha256.Sum256([]byte("jpeg-q90-white-v1:" + source))
	return hex.EncodeToString(sum[:])
}

func photoFallbackSource(contentType, source string) string {
	if contentType == "image/heic" {
		return "heif-primary-v1:" + source
	}
	return source
}

func (l *mediaLibrary) removePhotoFallbacks(m mediaItem) {
	if m.SHA256 != "" {
		_ = l.root.Remove(".fallbacks/" + photoFallbackKey(m.SHA256) + ".jpg")
		_ = l.root.Remove(".fallbacks/" + photoFallbackKey(photoFallbackSource(m.ContentType, m.SHA256)) + ".jpg")
	}
	if m.ThumbnailKey != "" {
		_ = l.root.Remove(".fallbacks/" + photoFallbackKey(m.ThumbnailKey) + ".jpg")
	}
}

func (l *mediaLibrary) jpegFallback(ctx context.Context, input *os.File, sourceKey string) (*os.File, error) {
	select {
	case l.fallbackGate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-l.fallbackGate }()
	path := ".fallbacks/" + photoFallbackKey(sourceKey) + ".jpg"
	if cached, err := l.root.Open(path); err == nil {
		info, statErr := cached.Stat()
		if statErr == nil && info.Mode().IsRegular() && info.Size() > 0 {
			return cached, nil
		}
		cached.Close()
		if statErr != nil {
			return nil, statErr
		}
		if err := l.root.Remove(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	processor, ok := l.processor.(mediaproc.PhotoProcessor)
	if !ok {
		return nil, errors.New("JPEG fallback processor is unavailable")
	}
	before, err := input.Stat()
	if err != nil {
		return nil, err
	}
	output, stage, err := l.photoStage()
	if err != nil {
		return nil, err
	}
	defer output.Close()
	defer l.root.Remove(stage)
	if err := processor.JPEG(ctx, input, output); err != nil {
		return nil, err
	}
	after, err := input.Stat()
	if err != nil || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return nil, errors.New("photo changed while generating a fallback; rescan")
	}
	if stat, err := output.Stat(); err != nil || stat.Size() == 0 {
		return nil, errors.New("JPEG fallback is empty")
	}
	if err := l.root.Rename(stage, path); err != nil {
		return nil, err
	}
	if l.logger != nil {
		l.logger.Info("photo JPEG fallback cached", "cache_key", photoFallbackKey(sourceKey))
	}
	return l.root.Open(path)
}

// Wildcards do not demonstrate JPEG XL decoder support. Exact q=0 refuses it.
func acceptsPhotoFormat(accept, contentType string) bool {
	for _, part := range strings.Split(accept, ",") {
		kind, parameters, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil || !strings.EqualFold(kind, contentType) {
			continue
		}
		if raw, exists := parameters["q"]; exists {
			quality, err := strconv.ParseFloat(raw, 64)
			return err == nil && quality > 0 && quality <= 1
		}
		return true
	}
	return false
}

func needsPhotoFallback(contentType, accept string) bool {
	switch contentType {
	case "image/jxl":
		return !acceptsPhotoFormat(accept, contentType)
	case "image/heic", "image/tiff", "image/bmp":
		return true
	}
	return false
}
