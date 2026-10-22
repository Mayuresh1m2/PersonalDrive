// Package dupes finds exact duplicate files in existing folders so the owner
// can see potential savings before importing anything. It only reads.
package dupes

import (
	"context"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/Mayuresh1m2/PersonalDrive/server/internal/storage"
)

type Group struct {
	SHA256 string   `json:"sha256"`
	Size   int64    `json:"size"`
	Paths  []string `json:"paths"`
}

// Wasted is the bytes that would be saved by keeping one copy.
func (g Group) Wasted() int64 { return g.Size * int64(len(g.Paths)-1) }

type Report struct {
	Files       int      `json:"files"`
	TotalBytes  int64    `json:"total_bytes"`
	HashedBytes int64    `json:"hashed_bytes"`
	Groups      []Group  `json:"groups"`
	WastedBytes int64    `json:"wasted_bytes"`
	Errors      []string `json:"errors,omitempty"`
}

// Scan walks roots and groups identical files. Files are bucketed by size
// first, so only files that share a size with another file are hashed.
func Scan(ctx context.Context, roots []string, minSize int64, progress func(string)) (*Report, error) {
	if progress == nil {
		progress = func(string) {}
	}
	rep := &Report{}
	bySize := map[int64][]string{}
	for _, root := range roots {
		progress("listing " + root)
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				rep.Errors = append(rep.Errors, err.Error())
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !d.Type().IsRegular() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				rep.Errors = append(rep.Errors, err.Error())
				return nil
			}
			rep.Files++
			rep.TotalBytes += info.Size()
			if info.Size() >= minSize {
				bySize[info.Size()] = append(bySize[info.Size()], p)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	candidates := 0
	for _, ps := range bySize {
		if len(ps) > 1 {
			candidates += len(ps)
		}
	}
	progress("hashing candidate files")
	byHash := map[string]*Group{}
	done := 0
	for size, ps := range bySize {
		if len(ps) < 2 {
			continue
		}
		for _, p := range ps {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			sum, n, err := storage.HashFile(p)
			if err != nil {
				rep.Errors = append(rep.Errors, err.Error())
				continue
			}
			rep.HashedBytes += n
			g := byHash[sum]
			if g == nil {
				g = &Group{SHA256: sum, Size: size}
				byHash[sum] = g
			}
			g.Paths = append(g.Paths, p)
			done++
			if done%500 == 0 {
				progress("hashed " + strconv.Itoa(done) + " of " + strconv.Itoa(candidates))
			}
		}
	}
	for _, g := range byHash {
		if len(g.Paths) > 1 {
			sort.Strings(g.Paths)
			rep.Groups = append(rep.Groups, *g)
			rep.WastedBytes += g.Wasted()
		}
	}
	sort.Slice(rep.Groups, func(i, j int) bool { return rep.Groups[i].Wasted() > rep.Groups[j].Wasted() })
	return rep, nil
}
