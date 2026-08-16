// Package convert renders camera RAW files to HEIF before upload.
//
// RAW is the worst possible thing to keep in a photo library: an ARW or CR2 is
// tens of megabytes of sensor data that Google Photos can only show you as a
// generated preview anyway. HEIF at high quality keeps the full resolution at a
// fraction of the size - measured on a Canon CR2 here, 25.2 MB became 10.9 MB
// at quality 90 with the 5184×3456 dimensions intact.
//
// The conversion is done by macOS's own `sips`, which reads RAW through Core
// Image and properly demosaics it rather than extracting the embedded preview
// thumbnail. That means no third-party dependency, and the same camera support
// the rest of the system has.
//
// This is a lossy, one-way step. It is only ever applied to the *copy* being
// uploaded - the OneDrive original is never touched, so the RAW survives.
package convert

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// RAWExts are the formats worth converting. Everything else is either already
// a rendered image or not a photograph.
var RAWExts = map[string]bool{
	"arw": true, "cr2": true, "cr3": true, "dng": true, "nef": true,
	"orf": true, "raf": true, "rw2": true, "srw": true, "pef": true,
}

// IsRAW reports whether a filename is a camera RAW file.
func IsRAW(name string) bool {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
	return RAWExts[ext]
}

// HEIFName is the name the converted file should carry in the library.
func HEIFName(name string) string {
	return strings.TrimSuffix(name, filepath.Ext(name)) + ".heic"
}

// Result describes one conversion.
type Result struct {
	Path         string // the temporary HEIF, caller's responsibility to remove
	Name         string // the name to publish it under
	Size         int64
	OriginalSize int64
}

// Saved is the proportion of the original's bytes the conversion avoided.
func (r Result) Saved() float64 {
	if r.OriginalSize == 0 {
		return 0
	}
	return 1 - float64(r.Size)/float64(r.OriginalSize)
}

// ToHEIF renders src into a HEIF file inside dir at the given quality (1–100).
//
// Quality 90 is the sensible default: visually transparent, roughly 57% smaller
// than the RAW. Note that quality 100 produces a file *larger* than the source
// on this camera's files, so it is not a "safest" choice - it is a worse one.
func ToHEIF(ctx context.Context, src, dir string, quality int) (Result, error) {
	if quality < 1 || quality > 100 {
		return Result{}, fmt.Errorf("quality %d out of range 1–100", quality)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Result{}, err
	}

	info, err := os.Stat(src)
	if err != nil {
		return Result{}, err
	}

	out := filepath.Join(dir, HEIFName(filepath.Base(src)))
	cmd := exec.CommandContext(ctx, "sips",
		"-s", "format", "heic",
		"-s", "formatOptions", fmt.Sprint(quality),
		src, "--out", out)
	if combined, err := cmd.CombinedOutput(); err != nil {
		os.Remove(out)
		return Result{}, fmt.Errorf("sips: %w: %s", err, strings.TrimSpace(string(combined)))
	}

	st, err := os.Stat(out)
	if err != nil {
		return Result{}, fmt.Errorf("sips reported success but wrote nothing: %w", err)
	}
	if st.Size() == 0 {
		os.Remove(out)
		return Result{}, fmt.Errorf("sips wrote an empty file")
	}
	return Result{
		Path:         out,
		Name:         HEIFName(filepath.Base(src)),
		Size:         st.Size(),
		OriginalSize: info.Size(),
	}, nil
}

// Available reports whether the converter can run at all.
func Available() bool {
	_, err := exec.LookPath("sips")
	return err == nil
}
