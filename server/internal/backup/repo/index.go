package repo

import (
	"bytes"
	"encoding/gob"
	"sync"
)

// Index maps blob ids to their pack locations. It is held in memory
// (~100 bytes per blob: ~100 MB for a 1 TB library at 1 MiB chunks) and
// cached on local disk so incremental backups don't read the remote index.
type Index struct {
	mu    sync.RWMutex
	blobs map[BlobID]Location
	packs map[string]PackMeta
	files map[string]bool // index file ids merged into this index
}

type PackMeta struct {
	Hot   bool
	Size  int64
	Blobs int
}

func NewIndex() *Index {
	return &Index{blobs: map[BlobID]Location{}, packs: map[string]PackMeta{}, files: map[string]bool{}}
}

func (ix *Index) Has(id BlobID) bool {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	_, ok := ix.blobs[id]
	return ok
}

func (ix *Index) Lookup(id BlobID) (Location, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	l, ok := ix.blobs[id]
	return l, ok
}

func (ix *Index) AddPack(p PackInfo) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.packs[p.ID] = PackMeta{Hot: p.Hot, Size: p.Size, Blobs: len(p.Blobs)}
	for _, b := range p.Blobs {
		ix.blobs[b.ID] = Location{Pack: p.ID, Offset: b.Offset, Length: b.Length, Raw: b.Raw, Type: b.Type}
	}
}

func (ix *Index) Pack(id string) (PackMeta, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	p, ok := ix.packs[id]
	return p, ok
}

// Packs returns a copy of the pack table.
func (ix *Index) Packs() map[string]PackMeta {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	out := make(map[string]PackMeta, len(ix.packs))
	for k, v := range ix.packs {
		out[k] = v
	}
	return out
}

// PackInfos reconstructs full pack descriptions (for rewriting the index).
func (ix *Index) PackInfos(keep func(packID string) bool) []PackInfo {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	byPack := map[string]*PackInfo{}
	for id, l := range ix.blobs {
		if keep != nil && !keep(l.Pack) {
			continue
		}
		p := byPack[l.Pack]
		if p == nil {
			m := ix.packs[l.Pack]
			p = &PackInfo{ID: l.Pack, Hot: m.Hot, Size: m.Size}
			byPack[l.Pack] = p
		}
		p.Blobs = append(p.Blobs, PackEntry{ID: id, Type: l.Type, Offset: l.Offset, Length: l.Length, Raw: l.Raw})
	}
	out := make([]PackInfo, 0, len(byPack))
	for _, p := range byPack {
		out = append(out, *p)
	}
	return out
}

func (ix *Index) Len() int {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return len(ix.blobs)
}

func (ix *Index) Files() []string {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	out := make([]string, 0, len(ix.files))
	for f := range ix.files {
		out = append(out, f)
	}
	return out
}

func (ix *Index) markFile(id string) {
	ix.mu.Lock()
	ix.files[id] = true
	ix.mu.Unlock()
}

func (ix *Index) hasFile(id string) bool {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.files[id]
}

// indexFile is the plaintext of index/<id> and of the local cache.
type indexFile struct {
	Version int
	Files   []string // only used by the local cache
	Packs   []PackInfo
}

// encodeIndex/decodeIndex handle the gob form; callers compress.
func encodeIndex(f indexFile) ([]byte, error) {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(f); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func decodeIndex(b []byte) (indexFile, error) {
	var f indexFile
	err := gob.NewDecoder(bytes.NewReader(b)).Decode(&f)
	return f, err
}
