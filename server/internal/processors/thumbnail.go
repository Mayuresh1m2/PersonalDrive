package processors

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/disintegration/imaging"
	_ "golang.org/x/image/webp"
)

// maxPixels caps decoded image size so one huge panorama can't exhaust RAM.
const maxPixels = 150_000_000

// Thumbnails renders JPEG previews at fixed sizes into the cache directory.
// Images are decoded natively; HEIC/AVIF/RAW and video need ffmpeg.
type Thumbnails struct {
	CacheDir string
	Sizes    []int
	FFmpeg   string // path to ffmpeg; empty disables video/HEIC
}

func (t *Thumbnails) Name() string { return "thumbnail" }
func (t *Thumbnails) Version() int { return 1 }

var nativeImage = map[string]bool{
	"image/jpeg": true, "image/png": true, "image/gif": true, "image/webp": true, "image/bmp": true, "image/tiff": true,
}

func (t *Thumbnails) Accepts(f FileInfo) bool {
	if nativeImage[f.Mime] {
		return true
	}
	return t.FFmpeg != "" && (strings.HasPrefix(f.Mime, "image/") || strings.HasPrefix(f.Mime, "video/"))
}

// Path returns where the thumbnail of a given size is stored.
func (t *Thumbnails) Path(sha string, size int) string {
	return filepath.Join(t.CacheDir, sha[:2], fmt.Sprintf("%s-%d.jpg", sha, size))
}

// NearestSize picks the smallest configured size >= want (or the largest).
func (t *Thumbnails) NearestSize(want int) int {
	best := 0
	for _, s := range t.Sizes {
		if s >= want && (best == 0 || s < best) {
			best = s
		}
	}
	if best == 0 {
		for _, s := range t.Sizes {
			best = max(best, s)
		}
	}
	return best
}

func (t *Thumbnails) Process(ctx context.Context, f FileInfo, src *os.File) error {
	var img image.Image
	var err error
	if nativeImage[f.Mime] {
		cfg, _, cerr := image.DecodeConfig(src)
		if cerr != nil {
			return fmt.Errorf("%w: %v", ErrUnsupported, cerr)
		}
		if cfg.Width*cfg.Height > maxPixels {
			return fmt.Errorf("%w: image too large (%dx%d)", ErrUnsupported, cfg.Width, cfg.Height)
		}
		if _, err := src.Seek(0, 0); err != nil {
			return err
		}
		img, err = imaging.Decode(src, imaging.AutoOrientation(true))
		if err != nil {
			return fmt.Errorf("%w: %v", ErrUnsupported, err)
		}
	} else {
		img, err = t.ffmpegFrame(ctx, f)
		if err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(t.Path(f.SHA256, 0)), 0o750); err != nil {
		return err
	}
	for _, size := range t.Sizes {
		thumb := img
		if b := img.Bounds(); b.Dx() > size || b.Dy() > size {
			thumb = imaging.Fit(img, size, size, imaging.CatmullRom)
		}
		if err := writeJPEG(t.Path(f.SHA256, size), thumb); err != nil {
			return err
		}
	}
	return nil
}

// ffmpegFrame extracts a single frame (1s in for videos) scaled to the
// largest thumbnail size.
func (t *Thumbnails) ffmpegFrame(ctx context.Context, f FileInfo) (image.Image, error) {
	largest := t.NearestSize(1 << 30)
	scale := fmt.Sprintf("scale='min(%d,iw)':'min(%d,ih)':force_original_aspect_ratio=decrease", largest, largest)
	run := func(seek bool) ([]byte, error) {
		args := []string{"-v", "error"}
		if seek {
			args = append(args, "-ss", "1")
		}
		args = append(args, "-i", f.AbsPath, "-frames:v", "1", "-vf", scale, "-f", "image2", "-c:v", "mjpeg", "pipe:1")
		return exec.CommandContext(ctx, t.FFmpeg, args...).Output()
	}
	out, err := run(strings.HasPrefix(f.Mime, "video/"))
	if err != nil || len(out) == 0 {
		out, err = run(false) // very short clip, or a still image
	}
	if err != nil || len(out) == 0 {
		return nil, fmt.Errorf("%w: ffmpeg could not decode: %v", ErrUnsupported, err)
	}
	img, err := jpeg.Decode(bytes.NewReader(out))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnsupported, err)
	}
	return img, nil
}

func writeJPEG(path string, img image.Image) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := jpeg.Encode(f, img, &jpeg.Options{Quality: 82}); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
