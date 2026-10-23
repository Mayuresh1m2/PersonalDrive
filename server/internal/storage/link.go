package storage

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"
)

// LinkMode is how identical files share storage on disk.
type LinkMode string

const (
	LinkReflink  LinkMode = "reflink"  // copy-on-write clone (btrfs, XFS); edits diverge automatically
	LinkHardlink LinkMode = "hardlink" // same inode; safe because files are never modified in place
	LinkOff      LinkMode = "off"      // plain copy
)

// ResolveLinkMode turns the configured value into a concrete mode by probing
// what the library filesystem supports. "auto" prefers reflink, then hardlink.
func ResolveLinkMode(root, configured string) (LinkMode, error) {
	switch configured {
	case "off":
		return LinkOff, nil
	case "reflink", "hardlink", "auto":
	default:
		return "", fmt.Errorf("unknown dedup mode %q", configured)
	}
	tmpDir := filepath.Join(root, ".tmp")
	if err := os.MkdirAll(tmpDir, 0o750); err != nil {
		return "", err
	}
	src := filepath.Join(tmpDir, "probe-"+uuid.NewString())
	if err := os.WriteFile(src, []byte("pdrive link probe"), 0o640); err != nil {
		return "", err
	}
	defer os.Remove(src)
	dst := src + ".clone"
	defer os.Remove(dst)

	if configured != "hardlink" {
		if err := reflink(src, dst); err == nil {
			return LinkReflink, nil
		}
		os.Remove(dst)
		if configured == "reflink" {
			return "", fmt.Errorf("dedup.on_disk is reflink but %s does not support reflinks (use btrfs or XFS, or set auto)", root)
		}
	}
	if err := os.Link(src, dst); err == nil {
		return LinkHardlink, nil
	}
	if configured == "hardlink" {
		return "", fmt.Errorf("dedup.on_disk is hardlink but %s does not support hardlinks (exFAT/FAT don't)", root)
	}
	return LinkOff, nil
}
