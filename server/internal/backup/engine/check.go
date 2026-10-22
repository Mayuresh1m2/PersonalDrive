package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"

	"github.com/Mayuresh1m2/PersonalDrive/server/internal/backup/objectstore"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/backup/repo"
)

type CheckOptions struct {
	// ReadFraction (0–1) of packs to download and fully verify. Archived
	// packs that aren't thawed are skipped (or thawed, with Thaw).
	ReadFraction float64
	Thaw         bool
	Tier         string
	// LocalSource/LocalPath compare a live directory against the latest
	// snapshot's copy, to catch bit rot on the primary disk.
	LocalSource string
	LocalPath   string
}

type CheckReport struct {
	Snapshots      int      `json:"snapshots"`
	Packs          int      `json:"packs"`
	PacksRead      int      `json:"packs_read"`
	PacksThawing   int      `json:"packs_thawing"`
	BlobsVerified  int      `json:"blobs_verified"`
	FilesCompared  int      `json:"files_compared"`
	Errors         []string `json:"errors"`
	LocalMismatch  []string `json:"local_mismatch,omitempty"`
	OrphanPacks    int      `json:"orphan_packs"`
	UnindexedBytes int64    `json:"orphan_bytes"`
}

func (c *CheckReport) OK() bool { return len(c.Errors) == 0 && len(c.LocalMismatch) == 0 }

func (c *CheckReport) errorf(format string, args ...any) {
	if len(c.Errors) < 1000 {
		c.Errors = append(c.Errors, fmt.Sprintf(format, args...))
	}
}

// Check verifies repository structure (cheap: hot data and listings only),
// optionally reads a sample of packs, and optionally compares the live
// library with the latest snapshot.
func Check(ctx context.Context, r *repo.Repo, opts CheckOptions) (*CheckReport, error) {
	rep := &CheckReport{}
	if err := r.LoadIndex(ctx); err != nil {
		return nil, err
	}
	objects := map[string]objectstore.ObjectInfo{}
	err := r.Store.List(ctx, "data/", func(o objectstore.ObjectInfo) error {
		objects[o.Key[strings.LastIndexByte(o.Key, '/')+1:]] = o
		return nil
	})
	if err != nil {
		return nil, err
	}
	packs := r.Index.Packs()
	rep.Packs = len(packs)
	for id, meta := range packs {
		o, ok := objects[id]
		switch {
		case !ok:
			rep.errorf("pack %s is in the index but missing from storage", id[:12])
		case o.Size != meta.Size:
			rep.errorf("pack %s has size %d, index says %d", id[:12], o.Size, meta.Size)
		}
	}
	for id, o := range objects {
		if _, ok := packs[id]; !ok {
			rep.OrphanPacks++
			rep.UnindexedBytes += o.Size
		}
	}

	snaps, err := r.Snapshots(ctx)
	if err != nil {
		return nil, err
	}
	rep.Snapshots = len(snaps)
	seen := map[string]bool{}
	for _, s := range snaps {
		checkTree(ctx, r, s.Tree, "", seen, rep)
	}

	if opts.ReadFraction > 0 {
		ids := make([]string, 0, len(packs))
		for id := range packs {
			ids = append(ids, id)
		}
		rand.Shuffle(len(ids), func(i, j int) { ids[i], ids[j] = ids[j], ids[i] })
		n := int(float64(len(ids))*opts.ReadFraction + 0.999)
		for _, id := range ids[:min(n, len(ids))] {
			if err := ctx.Err(); err != nil {
				return rep, err
			}
			readPack(ctx, r, id, opts, rep)
		}
	}

	if opts.LocalPath != "" && len(snaps) > 0 {
		root, err := r.LoadTree(ctx, snaps[len(snaps)-1].Tree)
		if err == nil {
			if n := root.Find(opts.LocalSource); n != nil {
				compareLocal(ctx, r, n.Subtree, opts.LocalPath, "", rep)
			}
		}
	}
	return rep, nil
}

func checkTree(ctx context.Context, r *repo.Repo, treeHex, path string, seen map[string]bool, rep *CheckReport) {
	if seen[treeHex] {
		return
	}
	seen[treeHex] = true
	t, err := r.LoadTree(ctx, treeHex)
	if err != nil {
		rep.errorf("tree %s (%s): %v", treeHex[:12], path, err)
		return
	}
	for _, n := range t.Nodes {
		p := path + "/" + n.Name
		for _, c := range n.Content {
			id, err := repo.ParseID(c)
			if err != nil {
				rep.errorf("%s: bad blob id", p)
				continue
			}
			if !r.Index.Has(id) {
				rep.errorf("%s: blob %s missing from index", p, c[:12])
			}
		}
		if n.Subtree != "" {
			checkTree(ctx, r, n.Subtree, p, seen, rep)
		}
	}
}

func readPack(ctx context.Context, r *repo.Repo, id string, opts CheckOptions, rep *CheckReport) {
	info, err := r.Store.Head(ctx, repo.PackKey(id))
	if err != nil {
		rep.errorf("pack %s: %v", id[:12], err)
		return
	}
	if !info.Readable() {
		if opts.Thaw && info.Restore == objectstore.RestoreNone {
			r.Store.RequestRestore(ctx, repo.PackKey(id), tierOr(opts.Tier), 3)
		}
		rep.PacksThawing++
		return
	}
	data, err := objectstore.GetAll(ctx, r.Store, repo.PackKey(id))
	if err != nil {
		rep.errorf("pack %s: download: %v", id[:12], err)
		return
	}
	if err := repo.VerifyPackID(id, bytes.NewReader(data)); err != nil {
		rep.errorf("%v", err)
		return
	}
	entries, err := repo.ReadPackHeader(r.Key, data)
	if err != nil {
		rep.errorf("pack %s: %v", id[:12], err)
		return
	}
	for _, e := range entries {
		end := int(e.Offset) + int(e.Length)
		if end > len(data) {
			rep.errorf("pack %s: blob beyond end", id[:12])
			continue
		}
		if _, err := repo.DecodeBlob(r.Key, e.ID, data[e.Offset:end]); err != nil {
			rep.errorf("pack %s: %v", id[:12], err)
			continue
		}
		rep.BlobsVerified++
	}
	rep.PacksRead++
}

// compareLocal re-hashes local files whose size and mtime match the snapshot
// and reports content differences (silent corruption on the primary disk).
func compareLocal(ctx context.Context, r *repo.Repo, treeHex, dir, rel string, rep *CheckReport) {
	t, err := r.LoadTree(ctx, treeHex)
	if err != nil {
		rep.errorf("tree %s: %v", treeHex[:12], err)
		return
	}
	for _, n := range t.Nodes {
		if ctx.Err() != nil {
			return
		}
		p := filepath.Join(dir, n.Name)
		relp := strings.TrimPrefix(rel+"/"+n.Name, "/")
		if n.Type == "dir" {
			compareLocal(ctx, r, n.Subtree, p, relp, rep)
			continue
		}
		st, err := os.Stat(p)
		if err != nil || st.Size() != n.Size || !st.ModTime().Equal(n.MTime) {
			continue // changed or deleted since the backup: not corruption
		}
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		h := sha256.New()
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			rep.LocalMismatch = append(rep.LocalMismatch, relp+": read error: "+err.Error())
			continue
		}
		rep.FilesCompared++
		if hex.EncodeToString(h.Sum(nil)) != n.SHA256 {
			rep.LocalMismatch = append(rep.LocalMismatch, relp+": content differs from backup but size/mtime are unchanged (possible disk corruption)")
		}
	}
}
