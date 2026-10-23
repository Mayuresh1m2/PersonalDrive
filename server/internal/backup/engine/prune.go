package engine

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/Mayuresh1m2/PersonalDrive/server/internal/backup/objectstore"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/backup/repo"
)

// Retention says which snapshots to keep. A snapshot is kept if any rule
// selects it; the newest snapshot is always kept.
type Retention struct {
	Last    int `yaml:"last" json:"last"`
	Hourly  int `yaml:"hourly" json:"hourly"`
	Daily   int `yaml:"daily" json:"daily"`
	Weekly  int `yaml:"weekly" json:"weekly"`
	Monthly int `yaml:"monthly" json:"monthly"`
	Yearly  int `yaml:"yearly" json:"yearly"`
}

func (p Retention) IsZero() bool { return p == Retention{} }

// ApplyRetention splits snapshots (any order) into keep and remove.
func ApplyRetention(snaps []*repo.Snapshot, p Retention) (keep, remove []*repo.Snapshot) {
	sorted := append([]*repo.Snapshot(nil), snaps...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Time.After(sorted[j].Time) }) // newest first
	keepSet := map[string]bool{}
	if len(sorted) > 0 {
		keepSet[sorted[0].ID] = true
	}
	for i := 0; i < p.Last && i < len(sorted); i++ {
		keepSet[sorted[i].ID] = true
	}
	bucket := func(n int, key func(time.Time) string) {
		seen := map[string]bool{}
		for _, s := range sorted {
			if len(seen) >= n {
				return
			}
			k := key(s.Time.Local())
			if !seen[k] {
				seen[k] = true
				keepSet[s.ID] = true
			}
		}
	}
	bucket(p.Hourly, func(t time.Time) string { return t.Format("2006-01-02 15") })
	bucket(p.Daily, func(t time.Time) string { return t.Format("2006-01-02") })
	bucket(p.Weekly, func(t time.Time) string { y, w := t.ISOWeek(); return fmt.Sprintf("%d-%d", y, w) })
	bucket(p.Monthly, func(t time.Time) string { return t.Format("2006-01") })
	bucket(p.Yearly, func(t time.Time) string { return t.Format("2006") })
	for _, s := range sorted {
		if keepSet[s.ID] {
			keep = append(keep, s)
		} else {
			remove = append(remove, s)
		}
	}
	return keep, remove
}

type PruneOptions struct {
	Retention Retention
	DryRun    bool
	Pricing   Pricing
	// RepackThreshold: hot packs whose unused fraction exceeds this are
	// rewritten. Cold packs are never repacked (that would require thawing);
	// they are deleted once fully unused and past their minimum duration.
	RepackThreshold float64
	StagingDir      string
}

type PruneReport struct {
	SnapshotsKept       int      `json:"snapshots_kept"`
	SnapshotsRemoved    []string `json:"snapshots_removed"`
	PacksDeleted        int      `json:"packs_deleted"`
	BytesFreed          int64    `json:"bytes_freed"`
	PacksDeferred       int      `json:"packs_deferred"` // unused but inside minimum storage duration
	BytesDeferred       int64    `json:"bytes_deferred"`
	PacksRepacked       int      `json:"packs_repacked"`
	UnusedBytesKept     int64    `json:"unused_bytes_kept"` // dead blobs inside still-used cold packs
	MonthlySavingsUSD   float64  `json:"monthly_savings_usd"`
	EarlyDeleteAvoidUSD float64  `json:"early_delete_fees_avoided_usd"`
}

// Prune forgets snapshots outside the retention policy and deletes data no
// remaining snapshot needs, without paying early-deletion fees.
func Prune(ctx context.Context, r *repo.Repo, opts PruneOptions) (*PruneReport, error) {
	if opts.RepackThreshold == 0 {
		opts.RepackThreshold = 0.5
	}
	refresh, unlock, err := r.Lock(ctx, "prune")
	if err != nil {
		return nil, err
	}
	defer unlock()
	stop := keepFresh(ctx, refresh)
	defer stop()
	if err := r.LoadIndex(ctx); err != nil {
		return nil, err
	}
	snaps, err := r.Snapshots(ctx)
	if err != nil {
		return nil, err
	}
	rep := &PruneReport{}
	keep, remove := snaps, []*repo.Snapshot(nil)
	if !opts.Retention.IsZero() {
		keep, remove = ApplyRetention(snaps, opts.Retention)
	}
	rep.SnapshotsKept = len(keep)
	for _, s := range remove {
		rep.SnapshotsRemoved = append(rep.SnapshotsRemoved, s.ID)
	}

	// Mark every blob reachable from kept snapshots (trees are hot: cheap).
	used := map[repo.BlobID]bool{}
	for _, s := range keep {
		if err := markTree(ctx, r, s.Tree, used); err != nil {
			return nil, fmt.Errorf("snapshot %s: %w", s.ID[:12], err)
		}
	}

	// What's actually in the store.
	objects := map[string]objectstore.ObjectInfo{}
	err = r.Store.List(ctx, "data/", func(o objectstore.ObjectInfo) error {
		objects[o.Key[strings.LastIndexByte(o.Key, '/')+1:]] = o
		return nil
	})
	if err != nil {
		return nil, err
	}

	usedBytes := map[string]int64{}
	totalBytes := map[string]int64{}
	for _, pi := range r.Index.PackInfos(nil) {
		for _, b := range pi.Blobs {
			totalBytes[pi.ID] += int64(b.Length)
			if used[b.ID] {
				usedBytes[pi.ID] += int64(b.Length)
			}
		}
	}

	now := time.Now()
	var deleteKeys []string
	deleted := map[string]bool{}
	var repack []string
	consider := func(id string, o objectstore.ObjectInfo) {
		age := now.Sub(o.LastModified)
		minDays := opts.Pricing.MinDays(o.StorageClass)
		if age < time.Duration(minDays)*24*time.Hour {
			rep.PacksDeferred++
			rep.BytesDeferred += o.Size
			remaining := float64(time.Duration(minDays)*24*time.Hour-age) / float64(30*24*time.Hour)
			rep.EarlyDeleteAvoidUSD += float64(o.Size) / gb * opts.Pricing.class(o.StorageClass).StoragePerGBMonth * remaining
			return
		}
		deleteKeys = append(deleteKeys, repo.PackKey(id))
		deleted[id] = true
		rep.PacksDeleted++
		rep.BytesFreed += o.Size
		rep.MonthlySavingsUSD += float64(o.Size) / gb * opts.Pricing.class(o.StorageClass).StoragePerGBMonth
	}
	for id, o := range objects {
		meta, indexed := r.Index.Pack(id)
		switch {
		case !indexed:
			// Orphan from an interrupted backup (we hold the lock, so not in flight).
			consider(id, o)
		case usedBytes[id] == 0:
			consider(id, o)
		case meta.Hot && float64(totalBytes[id]-usedBytes[id]) > opts.RepackThreshold*float64(totalBytes[id]):
			repack = append(repack, id)
		default:
			rep.UnusedBytesKept += totalBytes[id] - usedBytes[id]
		}
	}
	if opts.DryRun {
		rep.PacksRepacked = len(repack)
		return rep, nil
	}

	// Remove forgotten snapshots first: if we crash after this, the data
	// is merely unreferenced and the next prune collects it.
	if err := r.DeleteSnapshots(ctx, rep.SnapshotsRemoved...); err != nil {
		return nil, err
	}

	// Repack hot packs: copy live blobs into new packs.
	newPacks, err := repackHot(ctx, r, repack, used, opts.StagingDir)
	if err != nil {
		return nil, err
	}
	for _, id := range repack {
		deleteKeys = append(deleteKeys, repo.PackKey(id))
		deleted[id] = true
	}
	rep.PacksRepacked = len(repack)

	// New index: every surviving pack, plus the repacked ones. Written
	// before deleting packs so no index ever points at missing data.
	keepPacks := r.Index.PackInfos(func(id string) bool { return !deleted[id] && objects[id].Key != "" })
	keepPacks = append(keepPacks, newPacks...)
	if err := r.ReplaceIndex(ctx, keepPacks); err != nil {
		return nil, err
	}
	if err := r.Store.Delete(ctx, deleteKeys...); err != nil {
		return nil, err
	}
	return rep, nil
}

func markTree(ctx context.Context, r *repo.Repo, treeHex string, used map[repo.BlobID]bool) error {
	id, err := repo.ParseID(treeHex)
	if err != nil {
		return err
	}
	if used[id] {
		return nil // identical subtree already walked
	}
	used[id] = true
	t, err := r.LoadTree(ctx, treeHex)
	if err != nil {
		return err
	}
	for _, n := range t.Nodes {
		for _, c := range n.Content {
			cid, err := repo.ParseID(c)
			if err != nil {
				return err
			}
			used[cid] = true
		}
		if n.Subtree != "" {
			if err := markTree(ctx, r, n.Subtree, used); err != nil {
				return err
			}
		}
	}
	return nil
}

func repackHot(ctx context.Context, r *repo.Repo, packs []string, used map[repo.BlobID]bool, staging string) ([]repo.PackInfo, error) {
	if len(packs) == 0 {
		return nil, nil
	}
	if staging == "" {
		staging = os.TempDir()
	}
	var out []repo.PackInfo
	w, err := repo.NewPackWriter(staging, true, r.Key)
	if err != nil {
		return nil, err
	}
	flush := func() error {
		if w.Count() == 0 {
			w.Abort()
			return nil
		}
		path, info, err := w.Finalize()
		if err != nil {
			return err
		}
		defer os.Remove(path)
		if err := r.UploadPack(ctx, path, info); err != nil {
			return err
		}
		out = append(out, info)
		return nil
	}
	seen := map[repo.BlobID]bool{}
	for _, pi := range r.Index.PackInfos(func(id string) bool {
		for _, p := range packs {
			if p == id {
				return true
			}
		}
		return false
	}) {
		for _, b := range pi.Blobs {
			if !used[b.ID] || seen[b.ID] {
				continue
			}
			seen[b.ID] = true
			plain, err := r.ReadBlob(ctx, b.ID)
			if err != nil {
				w.Abort()
				return nil, err
			}
			sealed, _ := repo.EncodeBlob(r.Key, b.ID, plain)
			if err := w.Add(b.ID, b.Type, sealed, len(plain)); err != nil {
				w.Abort()
				return nil, err
			}
			if w.Size() >= r.Config.PackSize {
				if err := flush(); err != nil {
					return nil, err
				}
				if w, err = repo.NewPackWriter(staging, true, r.Key); err != nil {
					return nil, err
				}
			}
		}
	}
	return out, flush()
}
