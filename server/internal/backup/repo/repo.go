package repo

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Mayuresh1m2/PersonalDrive/server/internal/backup/chunker"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/backup/crypto"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/backup/objectstore"
)

var (
	ErrNoRepo     = errors.New("no backup repository here (run `pdrive backup init`)")
	ErrRepoExists = errors.New("a backup repository already exists here")
	ErrLocked     = errors.New("repository is locked by another backup or prune")
)

// Classes says which storage class to use for hot (metadata) and cold
// (file content) objects. Local stores ignore classes.
type Classes struct {
	Hot  string // e.g. STANDARD
	Cold string // e.g. DEEP_ARCHIVE
}

type Repo struct {
	Store    objectstore.Store
	Key      *crypto.MasterKey
	Config   Config
	Index    *Index
	Classes  Classes
	gear     *chunker.Gear
	cacheDir string

	treeMu    sync.Mutex
	treeCache map[string]*Tree
}

// InitOptions configures a new repository.
type InitOptions struct {
	Passphrase string
	PackSize   int64
	Classes    Classes
}

// Init creates a repository and returns the recovery key, which must be
// shown to the owner once and never stored on the server.
func Init(ctx context.Context, store objectstore.Store, opts InitOptions) (recoveryKey string, err error) {
	if _, err := store.Head(ctx, ConfigKey); err == nil {
		return "", ErrRepoExists
	} else if !errors.Is(err, objectstore.ErrNotFound) {
		return "", err
	}
	if len(opts.Passphrase) < 12 {
		return "", errors.New("backup passphrase must be at least 12 characters")
	}
	mk, err := crypto.NewMasterKey()
	if err != nil {
		return "", err
	}
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		return "", err
	}
	if opts.PackSize == 0 {
		opts.PackSize = 32 << 20
	}
	cfg := Config{
		Version: FormatVersion, ID: randomID(), ChunkerSeed: seed, Chunker: chunker.DefaultParams,
		PackSize: opts.PackSize, Created: time.Now().UTC(),
	}
	r := &Repo{Store: store, Key: mk, Config: cfg, Classes: opts.Classes}
	recoveryKey, err = crypto.NewRecoveryKey()
	if err != nil {
		return "", err
	}
	for _, k := range []struct{ secret, label string }{
		{opts.Passphrase, crypto.LabelPassphrase}, {recoveryKey, crypto.LabelRecovery},
	} {
		if err := r.AddKey(ctx, k.secret, k.label); err != nil {
			return "", err
		}
	}
	data, _ := json.Marshal(cfg)
	if err := r.putSealed(ctx, ConfigKey, data, "pdrive config"); err != nil {
		return "", err
	}
	return recoveryKey, nil
}

// Open unlocks a repository with a passphrase or recovery key. cacheDir holds
// the local index and tree caches (keyed by repository id).
func Open(ctx context.Context, store objectstore.Store, secret, cacheDir string, classes Classes) (*Repo, error) {
	var keyFiles []string
	err := store.List(ctx, "keys/", func(o objectstore.ObjectInfo) error {
		keyFiles = append(keyFiles, o.Key)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(keyFiles) == 0 {
		return nil, ErrNoRepo
	}
	var mk *crypto.MasterKey
	for _, k := range keyFiles {
		data, err := objectstore.GetAll(ctx, store, k)
		if err != nil {
			return nil, err
		}
		var kf crypto.KeyFile
		if err := json.Unmarshal(data, &kf); err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		if mk, err = kf.Unwrap(secret); err == nil {
			break
		}
	}
	if mk == nil {
		return nil, crypto.ErrWrongSecret
	}
	r := &Repo{Store: store, Key: mk, Classes: classes, treeCache: map[string]*Tree{}}
	data, err := r.getSealed(ctx, ConfigKey, "pdrive config")
	if err != nil {
		return nil, fmt.Errorf("reading repository config: %w", err)
	}
	if err := json.Unmarshal(data, &r.Config); err != nil {
		return nil, err
	}
	if r.Config.Version != FormatVersion {
		return nil, fmt.Errorf("repository format v%d is not supported by this version", r.Config.Version)
	}
	r.gear = chunker.NewGear(r.Config.ChunkerSeed)
	r.cacheDir = filepath.Join(cacheDir, r.Config.ID)
	r.Index = NewIndex()
	return r, nil
}

func (r *Repo) Gear() *chunker.Gear { return r.gear }

// AddKey adds another way to unlock the repository (e.g. a new passphrase).
func (r *Repo) AddKey(ctx context.Context, secret, label string) error {
	kf, err := crypto.Wrap(r.Key, secret, label)
	if err != nil {
		return err
	}
	data, _ := json.MarshalIndent(kf, "", "  ")
	return r.put(ctx, KeyFileKey(label+"-"+randomID()[:16]), data, r.Classes.Hot)
}

// ChangePassphrase replaces passphrase key files, keeping the recovery key.
func (r *Repo) ChangePassphrase(ctx context.Context, newPassphrase string) error {
	if len(newPassphrase) < 12 {
		return errors.New("backup passphrase must be at least 12 characters")
	}
	var old []string
	r.Store.List(ctx, "keys/"+crypto.LabelPassphrase+"-", func(o objectstore.ObjectInfo) error {
		old = append(old, o.Key)
		return nil
	})
	if err := r.AddKey(ctx, newPassphrase, crypto.LabelPassphrase); err != nil {
		return err
	}
	return r.Store.Delete(ctx, old...)
}

// ---- Object helpers -----------------------------------------------------------

func (r *Repo) put(ctx context.Context, key string, data []byte, class string) error {
	return Retry(ctx, func() error {
		return r.Store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), objectstore.PutOptions{StorageClass: class})
	})
}

func (r *Repo) putSealed(ctx context.Context, key string, plain []byte, aad string) error {
	return r.put(ctx, key, r.Key.Seal(zenc.EncodeAll(plain, nil), []byte(aad)), r.Classes.Hot)
}

func (r *Repo) getSealed(ctx context.Context, key, aad string) ([]byte, error) {
	var data []byte
	err := Retry(ctx, func() error {
		var err error
		data, err = objectstore.GetAll(ctx, r.Store, key)
		return err
	})
	if err != nil {
		return nil, err
	}
	z, err := r.Key.Open(data, []byte(aad))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", key, err)
	}
	return zdec.DecodeAll(z, nil)
}

// Retry runs fn with exponential backoff for transient errors.
func Retry(ctx context.Context, fn func() error) error {
	delay := time.Second
	var err error
	for attempt := 0; attempt < 6; attempt++ {
		if err = fn(); err == nil {
			return nil
		}
		if errors.Is(err, objectstore.ErrNotFound) || errors.Is(err, objectstore.ErrArchived) ||
			errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay = min(delay*2, time.Minute)
	}
	return err
}

// ---- Index ------------------------------------------------------------------------

// LoadIndex brings the in-memory index up to date: from the local cache, plus
// any remote index files the cache hasn't seen. If the remote set shrank
// (a prune rewrote it), the index is rebuilt from scratch.
func (r *Repo) LoadIndex(ctx context.Context) error {
	var remote []string
	err := r.Store.List(ctx, "index/", func(o objectstore.ObjectInfo) error {
		remote = append(remote, strings.TrimPrefix(o.Key, "index/"))
		return nil
	})
	if err != nil {
		return err
	}
	remoteSet := map[string]bool{}
	for _, id := range remote {
		remoteSet[id] = true
	}
	ix := NewIndex()
	if cached, err := r.readCache(); err == nil {
		stale := false
		for _, f := range cached.Files {
			if !remoteSet[f] {
				stale = true
				break
			}
		}
		if !stale {
			for _, p := range cached.Packs {
				ix.AddPack(p)
			}
			for _, f := range cached.Files {
				ix.markFile(f)
			}
		}
	}
	changed := false
	for _, id := range remote {
		if ix.hasFile(id) {
			continue
		}
		data, err := r.getSealed(ctx, IndexKey(id), "pdrive index")
		if err != nil {
			return err
		}
		f, err := decodeIndex(data)
		if err != nil {
			return fmt.Errorf("index %s: %w", id, err)
		}
		for _, p := range f.Packs {
			ix.AddPack(p)
		}
		ix.markFile(id)
		changed = true
	}
	r.Index = ix
	if changed {
		return r.writeCache()
	}
	return nil
}

// SaveIndex uploads a new index file describing packs, and records it locally.
func (r *Repo) SaveIndex(ctx context.Context, packs []PackInfo) (string, error) {
	if len(packs) == 0 {
		return "", nil
	}
	data, err := encodeIndex(indexFile{Version: 1, Packs: packs})
	if err != nil {
		return "", err
	}
	id := randomID()
	if err := r.putSealed(ctx, IndexKey(id), data, "pdrive index"); err != nil {
		return "", err
	}
	r.Index.markFile(id)
	return id, r.writeCache()
}

// ReplaceIndex writes one index file containing exactly packs and deletes all
// other index files (used by prune).
func (r *Repo) ReplaceIndex(ctx context.Context, packs []PackInfo) error {
	old := r.Index.Files()
	ix := NewIndex()
	for _, p := range packs {
		ix.AddPack(p)
	}
	r.Index = ix
	if _, err := r.SaveIndex(ctx, packs); err != nil {
		return err
	}
	keys := make([]string, len(old))
	for i, id := range old {
		keys[i] = IndexKey(id)
	}
	return r.Store.Delete(ctx, keys...)
}

func (r *Repo) cachePath() string { return filepath.Join(r.cacheDir, "index.cache") }

func (r *Repo) readCache() (indexFile, error) {
	data, err := os.ReadFile(r.cachePath())
	if err != nil {
		return indexFile{}, err
	}
	raw, err := zdec.DecodeAll(data, nil)
	if err != nil {
		return indexFile{}, err
	}
	return decodeIndex(raw)
}

func (r *Repo) writeCache() error {
	if err := os.MkdirAll(r.cacheDir, 0o700); err != nil {
		return err
	}
	data, err := encodeIndex(indexFile{Version: 1, Files: r.Index.Files(), Packs: r.Index.PackInfos(nil)})
	if err != nil {
		return err
	}
	tmp := r.cachePath() + ".tmp"
	if err := os.WriteFile(tmp, zenc.EncodeAll(data, nil), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, r.cachePath())
}

// ---- Packs ---------------------------------------------------------------------------

// UploadPack uploads a finalized pack file from local staging.
func (r *Repo) UploadPack(ctx context.Context, path string, info PackInfo) error {
	class := r.Classes.Cold
	if info.Hot {
		class = r.Classes.Hot
	}
	return Retry(ctx, func() error {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		return r.Store.Put(ctx, PackKey(info.ID), f, info.Size, objectstore.PutOptions{StorageClass: class})
	})
}

// ReadBlob fetches and decodes one blob with a range read.
func (r *Repo) ReadBlob(ctx context.Context, id BlobID) ([]byte, error) {
	loc, ok := r.Index.Lookup(id)
	if !ok {
		return nil, fmt.Errorf("blob %s not in index", IDString(id)[:12])
	}
	var sealed []byte
	err := Retry(ctx, func() error {
		rc, err := r.Store.Get(ctx, PackKey(loc.Pack), int64(loc.Offset), int64(loc.Length))
		if err != nil {
			return err
		}
		defer rc.Close()
		sealed, err = io.ReadAll(rc)
		return err
	})
	if err != nil {
		return nil, err
	}
	return DecodeBlob(r.Key, id, sealed)
}

// ---- Trees -------------------------------------------------------------------------------

// EncodeTree serialises a tree; identical directories produce identical blobs.
func EncodeTree(t *Tree) []byte {
	sort.Slice(t.Nodes, func(i, j int) bool { return t.Nodes[i].Name < t.Nodes[j].Name })
	data, _ := json.Marshal(t)
	return data
}

// LoadTree reads a tree blob, using an on-disk cache (trees are immutable).
func (r *Repo) LoadTree(ctx context.Context, idHex string) (*Tree, error) {
	r.treeMu.Lock()
	if t, ok := r.treeCache[idHex]; ok {
		r.treeMu.Unlock()
		return t, nil
	}
	r.treeMu.Unlock()
	id, err := ParseID(idHex)
	if err != nil {
		return nil, err
	}
	cache := filepath.Join(r.cacheDir, "trees", idHex[:2], idHex)
	var plain []byte
	if sealed, err := os.ReadFile(cache); err == nil {
		plain, err = DecodeBlob(r.Key, id, sealed)
		if err != nil {
			os.Remove(cache)
			plain = nil
		}
	}
	if plain == nil {
		plain, err = r.ReadBlob(ctx, id)
		if err != nil {
			return nil, err
		}
		sealed, _ := EncodeBlob(r.Key, id, plain)
		os.MkdirAll(filepath.Dir(cache), 0o700)
		os.WriteFile(cache, sealed, 0o600)
	}
	var t Tree
	if err := json.Unmarshal(plain, &t); err != nil {
		return nil, fmt.Errorf("tree %s: %w", idHex[:12], err)
	}
	r.treeMu.Lock()
	if len(r.treeCache) > 10000 {
		r.treeCache = map[string]*Tree{}
	}
	r.treeCache[idHex] = &t
	r.treeMu.Unlock()
	return &t, nil
}

// ---- Snapshots -----------------------------------------------------------------------------

func (r *Repo) SaveSnapshot(ctx context.Context, s *Snapshot) error {
	if s.ID == "" {
		s.ID = randomID()
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return r.putSealed(ctx, SnapshotKey(s.ID), data, "pdrive snapshot")
}

// Snapshots returns all snapshots, oldest first.
func (r *Repo) Snapshots(ctx context.Context) ([]*Snapshot, error) {
	var ids []string
	err := r.Store.List(ctx, "snapshots/", func(o objectstore.ObjectInfo) error {
		ids = append(ids, strings.TrimPrefix(o.Key, "snapshots/"))
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]*Snapshot, 0, len(ids))
	for _, id := range ids {
		data, err := r.getSealed(ctx, SnapshotKey(id), "pdrive snapshot")
		if err != nil {
			return nil, err
		}
		var s Snapshot
		if err := json.Unmarshal(data, &s); err != nil {
			return nil, fmt.Errorf("snapshot %s: %w", id, err)
		}
		s.ID = id
		out = append(out, &s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out, nil
}

// FindSnapshot resolves "latest", a full id, or a unique id prefix.
func (r *Repo) FindSnapshot(ctx context.Context, ref string) (*Snapshot, error) {
	snaps, err := r.Snapshots(ctx)
	if err != nil {
		return nil, err
	}
	if len(snaps) == 0 {
		return nil, errors.New("repository has no snapshots yet")
	}
	if ref == "" || ref == "latest" {
		return snaps[len(snaps)-1], nil
	}
	var found *Snapshot
	for _, s := range snaps {
		if strings.HasPrefix(s.ID, ref) {
			if found != nil {
				return nil, fmt.Errorf("snapshot id %q is ambiguous", ref)
			}
			found = s
		}
	}
	if found == nil {
		return nil, fmt.Errorf("snapshot %q not found", ref)
	}
	return found, nil
}

func (r *Repo) DeleteSnapshots(ctx context.Context, ids ...string) error {
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = SnapshotKey(id)
	}
	return r.Store.Delete(ctx, keys...)
}

// ---- Locks ------------------------------------------------------------------------------------

type lockInfo struct {
	Host    string    `json:"host"`
	PID     int       `json:"pid"`
	Purpose string    `json:"purpose"`
	Time    time.Time `json:"time"`
}

const lockStale = 2 * time.Hour

// Lock takes an exclusive repository lock. Long operations must call the
// returned refresh func at least hourly; unlock removes the lock.
func (r *Repo) Lock(ctx context.Context, purpose string) (refresh func() error, unlock func(), err error) {
	host, _ := os.Hostname()
	var holders []string
	err = r.Store.List(ctx, "locks/", func(o objectstore.ObjectInfo) error {
		if time.Since(o.LastModified) < lockStale {
			holders = append(holders, o.Key)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	if len(holders) > 0 {
		data, _ := objectstore.GetAll(ctx, r.Store, holders[0])
		var li lockInfo
		json.Unmarshal(data, &li)
		return nil, nil, fmt.Errorf("%w (%s on %s, pid %d, since %s)", ErrLocked, li.Purpose, li.Host, li.PID, li.Time.Local().Format(time.Stamp))
	}
	key := LockKey(randomID()[:16])
	write := func() error {
		data, _ := json.Marshal(lockInfo{Host: host, PID: os.Getpid(), Purpose: purpose, Time: time.Now().UTC()})
		return r.put(ctx, key, data, r.Classes.Hot)
	}
	if err := write(); err != nil {
		return nil, nil, err
	}
	return write, func() { r.Store.Delete(context.Background(), key) }, nil
}

// RemoveStaleLocks deletes every lock (use after a crash, when sure nothing runs).
func (r *Repo) RemoveLocks(ctx context.Context) (int, error) {
	var keys []string
	r.Store.List(ctx, "locks/", func(o objectstore.ObjectInfo) error {
		keys = append(keys, o.Key)
		return nil
	})
	return len(keys), r.Store.Delete(ctx, keys...)
}

func randomID() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
