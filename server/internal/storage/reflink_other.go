//go:build !linux

package storage

import "errors"

func reflink(src, dst string) error { return errors.ErrUnsupported }
