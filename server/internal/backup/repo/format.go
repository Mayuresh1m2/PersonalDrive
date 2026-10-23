// Package repo implements the backup repository format (see docs/BACKUP_FORMAT.md):
//
//	config               encrypted repository config (chunker seed, pack size)
//	keys/<id>            master key wrapped by a passphrase or recovery key
//	data/<xx>/<pack-id>  packs of encrypted blobs
//	index/<id>           encrypted blob → pack location tables
//	snapshots/<id>       encrypted snapshot descriptions
//	locks/<id>           writer locks
//
// Everything except data packs holding file contents ("cold" packs) is stored
// in the hot storage class, so planning a restore, listing snapshots and
// incremental backups never need to thaw archived data.
package repo

import (
	"encoding/hex"
	"fmt"
	"time"

	"github.com/Mayuresh1m2/PersonalDrive/server/internal/backup/chunker"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/backup/crypto"
)

const FormatVersion = 1

type BlobID = crypto.BlobID

func IDString(id BlobID) string { return hex.EncodeToString(id[:]) }

func ParseID(s string) (BlobID, error) {
	var id BlobID
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != len(id) {
		return id, fmt.Errorf("invalid id %q", s)
	}
	copy(id[:], b)
	return id, nil
}

// BlobType distinguishes file content from directory listings.
type BlobType uint8

const (
	BlobData BlobType = 1
	BlobTree BlobType = 2
)

// Codec of a blob's plaintext inside the sealed payload.
const (
	CodecRaw  byte = 0
	CodecZstd byte = 1
)

// Config is the encrypted repository configuration.
type Config struct {
	Version     int            `json:"version"`
	ID          string         `json:"id"`
	ChunkerSeed []byte         `json:"chunker_seed"`
	Chunker     chunker.Params `json:"chunker"`
	PackSize    int64          `json:"pack_size"`
	Created     time.Time      `json:"created"`
}

// Snapshot is one point-in-time backup.
type Snapshot struct {
	ID       string        `json:"-"`
	Time     time.Time     `json:"time"`
	Host     string        `json:"host"`
	Tree     string        `json:"tree"` // root tree id (hex); top-level entries are sources
	Parent   string        `json:"parent,omitempty"`
	Tags     []string      `json:"tags,omitempty"`
	Stats    SnapshotStats `json:"stats"`
	Program  string        `json:"program"`
	Complete bool          `json:"complete"`
}

type SnapshotStats struct {
	Files          int64         `json:"files"`
	Dirs           int64         `json:"dirs"`
	BytesScanned   int64         `json:"bytes_scanned"`  // logical size of everything in the snapshot
	BytesRead      int64         `json:"bytes_read"`     // read from disk (changed files)
	BytesNew       int64         `json:"bytes_new"`      // new unique data before compression
	BytesDeduped   int64         `json:"bytes_deduped"`  // chunks already in the repository
	BytesStored    int64         `json:"bytes_stored"`   // after compression + encryption
	BytesUploaded  int64         `json:"bytes_uploaded"` // packs + index + trees
	PacksUploaded  int           `json:"packs_uploaded"`
	FilesUnchanged int64         `json:"files_unchanged"` // skipped via parent snapshot
	Errors         []string      `json:"errors,omitempty"`
	Duration       time.Duration `json:"duration"`
}

// Node is a file or directory in a tree.
type Node struct {
	Name    string    `json:"name"`
	Type    string    `json:"type"` // "file" | "dir"
	Size    int64     `json:"size,omitempty"`
	MTime   time.Time `json:"mtime"`
	Mode    uint32    `json:"mode"`
	Inode   uint64    `json:"inode,omitempty"`
	SHA256  string    `json:"sha256,omitempty"`
	Content []string  `json:"content,omitempty"` // data blob ids in order
	Subtree string    `json:"subtree,omitempty"` // tree blob id for directories
}

// Tree is a directory listing, sorted by name.
type Tree struct {
	Nodes []Node `json:"nodes"`
}

// Find returns the node with the given name, or nil.
func (t *Tree) Find(name string) *Node {
	for i := range t.Nodes {
		if t.Nodes[i].Name == name {
			return &t.Nodes[i]
		}
	}
	return nil
}

// Location is where a blob lives.
type Location struct {
	Pack   string // pack id (hex)
	Offset uint32
	Length uint32 // sealed length in the pack
	Raw    uint32 // plaintext length
	Type   BlobType
}

// PackInfo describes one uploaded pack, as recorded in index files.
type PackInfo struct {
	ID    string      `json:"id"`
	Hot   bool        `json:"hot"`
	Size  int64       `json:"size"`
	Blobs []PackEntry `json:"blobs"`
}

type PackEntry struct {
	ID     BlobID   `json:"id"`
	Type   BlobType `json:"type"`
	Offset uint32   `json:"off"`
	Length uint32   `json:"len"`
	Raw    uint32   `json:"raw"`
}

func PackKey(id string) string     { return "data/" + id[:2] + "/" + id }
func IndexKey(id string) string    { return "index/" + id }
func SnapshotKey(id string) string { return "snapshots/" + id }
func KeyFileKey(id string) string  { return "keys/" + id }
func LockKey(id string) string     { return "locks/" + id }

const ConfigKey = "config"
