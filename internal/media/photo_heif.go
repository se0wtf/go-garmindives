package media

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// HEIF images can be grids of independently coded HEVC tiles. Selecting the
// first FFmpeg video stream returns just one tile, not the displayed photograph.
func IsHEIF(input *os.File) bool {
	switch strings.ToLower(filepath.Ext(input.Name())) {
	case ".heic", ".heif":
		return true
	}
	var header [4096]byte
	n, _ := input.ReadAt(header[:], 0)
	if n < 16 || string(header[4:8]) != "ftyp" {
		return false
	}
	end := min(n, int(binary.BigEndian.Uint32(header[:4])))
	for offset := 8; offset+4 <= end; offset += 4 {
		if offset == 12 { // Minor version, not a compatible brand.
			continue
		}
		switch string(header[offset : offset+4]) {
		case "heic", "heix", "hevc", "hevx", "heim", "heis", "hevm", "hevs":
			return true
		}
	}
	return false
}

func findHEIFDecoder() string {
	for _, name := range []string{"heif-convert", "heif-dec"} {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	return ""
}

func (p *ffmpegProcessor) decodeHEIF(ctx context.Context, input *os.File, output string) error {
	tool := p.HEIF
	if tool == "" {
		tool = findHEIFDecoder()
	}
	if tool == "" {
		return errors.New("heif-convert/heif-dec is required for HEIC photos; install libheif/libheif-tools with an HEVC decoder")
	}
	if err := runPhotoTool(ctx, tool, input, "/dev/fd/3", output); err != nil {
		return err
	}
	// libheif assembles grids and applies container crop/rotation/mirror transforms.
	// Its CLI numbers outputs for multiple top-level images; never silently pick
	// one or fall back to FFmpeg in that case. Auxiliary images are not requested.
	if _, err := os.Stat(output); err != nil {
		if _, multiple := os.Stat(strings.TrimSuffix(output, ".png") + "-1.png"); multiple == nil {
			return errors.New("HEIF has multiple top-level images; source retained instead of selecting an arbitrary image")
		}
		return fmt.Errorf("HEIF decoder did not produce a complete photo: %w", err)
	}
	_, err := photoProbe(output, "heif")
	return err
}

func (p *ffmpegProcessor) inspectHEIF(ctx context.Context, input *os.File) (Probe, error) {
	dir, err := os.MkdirTemp("", "garmindives-heif-")
	if err != nil {
		return Probe{}, err
	}
	defer os.RemoveAll(dir)
	output := filepath.Join(dir, "decoded.png")
	if err := p.decodeHEIF(ctx, input, output); err != nil {
		return Probe{}, err
	}
	return photoProbe(output, "heif")
}
