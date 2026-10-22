package processors

import (
	"context"
	"image"
	"os"
	"strings"
	"time"

	"github.com/rwcarlsen/goexif/exif"

	"github.com/Mayuresh1m2/PersonalDrive/server/internal/metadata"
)

// MediaInfo extracts dimensions, capture time, camera and GPS from images.
// It feeds timeline views now and "memories" later.
type MediaInfo struct {
	DB *metadata.DB
}

func (m *MediaInfo) Name() string { return "mediainfo" }
func (m *MediaInfo) Version() int { return 1 }

func (m *MediaInfo) Accepts(f FileInfo) bool { return strings.HasPrefix(f.Mime, "image/") }

func (m *MediaInfo) Process(ctx context.Context, f FileInfo, src *os.File) error {
	info := &metadata.MediaInfo{SHA256: f.SHA256}
	if cfg, _, err := image.DecodeConfig(src); err == nil {
		info.Width, info.Height = cfg.Width, cfg.Height
	}
	if _, err := src.Seek(0, 0); err != nil {
		return err
	}
	if x, err := exif.Decode(src); err == nil {
		if t, err := x.DateTime(); err == nil && t.Year() > 1900 {
			// EXIF times are wall-clock without a zone; keep the wall clock as-is.
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, time.UTC)
			info.TakenAt = &t
		}
		if lat, lon, err := x.LatLong(); err == nil && (lat != 0 || lon != 0) {
			info.Latitude, info.Longitude = &lat, &lon
		}
		info.CameraMake = tagString(x, exif.Make)
		info.CameraModel = tagString(x, exif.Model)
		if tag, err := x.Get(exif.Orientation); err == nil {
			if v, err := tag.Int(0); err == nil {
				info.Orientation = v
			}
		}
		if info.Width == 0 {
			if tag, err := x.Get(exif.PixelXDimension); err == nil {
				info.Width, _ = tag.Int(0)
			}
			if tag, err := x.Get(exif.PixelYDimension); err == nil {
				info.Height, _ = tag.Int(0)
			}
		}
	}
	if info.Width == 0 && info.TakenAt == nil && info.CameraMake == "" {
		return ErrUnsupported
	}
	return m.DB.SaveMediaInfo(ctx, info)
}

func tagString(x *exif.Exif, name exif.FieldName) string {
	tag, err := x.Get(name)
	if err != nil {
		return ""
	}
	s, err := tag.StringVal()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.Trim(s, "\x00"))
}
