// Package volume manages the marker files that identify PersonalDrive disks.
//
// External disks are mounted at a directory that still exists (empty) when the
// disk is unplugged. Writing there would silently fill the laptop SSD, so every
// volume carries a .pdrive-volume marker and nothing is written unless the
// expected marker is present.
package volume

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

const MarkerName = ".pdrive-volume"

// Roles a volume can play.
const (
	RoleLibrary = "library"
	RoleBackup  = "backup"
)

type Marker struct {
	ID        string    `json:"id"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
	Version   int       `json:"version"`
}

var ErrNoMarker = errors.New("volume marker not found (disk unplugged or not initialised)")

// Init writes a marker into dir, which must already exist. It refuses to
// overwrite a marker with a different role; re-running with the same role
// returns the existing marker.
func Init(dir, role string) (Marker, error) {
	if role != RoleLibrary && role != RoleBackup {
		return Marker{}, fmt.Errorf("unknown volume role %q", role)
	}
	st, err := os.Stat(dir)
	if err != nil {
		return Marker{}, fmt.Errorf("volume path: %w (is the disk mounted?)", err)
	}
	if !st.IsDir() {
		return Marker{}, fmt.Errorf("%s is not a directory", dir)
	}
	if m, err := Read(dir); err == nil {
		if m.Role != role {
			return Marker{}, fmt.Errorf("%s is already initialised as a %s volume", dir, m.Role)
		}
		return m, nil
	} else if !errors.Is(err, ErrNoMarker) {
		return Marker{}, err
	}
	m := Marker{ID: uuid.NewString(), Role: role, CreatedAt: time.Now().UTC(), Version: 1}
	data, _ := json.MarshalIndent(m, "", "  ")
	tmp := filepath.Join(dir, MarkerName+".tmp")
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return Marker{}, err
	}
	if err := os.Rename(tmp, filepath.Join(dir, MarkerName)); err != nil {
		return Marker{}, err
	}
	return m, nil
}

func Read(dir string) (Marker, error) {
	data, err := os.ReadFile(filepath.Join(dir, MarkerName))
	if errors.Is(err, os.ErrNotExist) {
		return Marker{}, ErrNoMarker
	}
	if err != nil {
		return Marker{}, err
	}
	var m Marker
	if err := json.Unmarshal(data, &m); err != nil {
		return Marker{}, fmt.Errorf("corrupt volume marker in %s: %w", dir, err)
	}
	return m, nil
}

type Status struct {
	Path       string `json:"path"`
	Role       string `json:"role"`
	OK         bool   `json:"ok"`
	ID         string `json:"id,omitempty"`
	Problem    string `json:"problem,omitempty"`
	FreeBytes  uint64 `json:"free_bytes"`
	TotalBytes uint64 `json:"total_bytes"`
}

// Check verifies dir has a marker of the given role and, if expectedID is
// non-empty, that it is the same disk as before.
func Check(dir, role, expectedID string) Status {
	s := Status{Path: dir, Role: role}
	m, err := Read(dir)
	switch {
	case err != nil:
		s.Problem = err.Error()
	case m.Role != role:
		s.Problem = fmt.Sprintf("marker role is %q, expected %q", m.Role, role)
	case expectedID != "" && m.ID != expectedID:
		s.Problem = fmt.Sprintf("a different disk is mounted here (id %s, expected %s)", m.ID, expectedID)
	default:
		s.OK = true
		s.ID = m.ID
	}
	s.FreeBytes, s.TotalBytes = Usage(dir)
	return s
}

// Usage returns free and total bytes of the filesystem holding path.
func Usage(path string) (free, total uint64) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0
	}
	return st.Bavail * uint64(st.Bsize), st.Blocks * uint64(st.Bsize)
}

// Guard tracks whether the library volume is safe to write to. It is
// re-checked periodically so plugging the disk back in recovers automatically.
type Guard struct {
	dir, role, expectedID string

	mu      sync.RWMutex
	status  Status
	blocked string // e.g. a restore in progress
}

// SetBlocked forces the volume read-only with a reason ("" clears it).
func (g *Guard) SetBlocked(reason string) {
	g.mu.Lock()
	g.blocked = reason
	g.mu.Unlock()
}

func NewGuard(dir, role, expectedID string) *Guard {
	g := &Guard{dir: dir, role: role, expectedID: expectedID}
	g.Refresh()
	return g
}

// SetExpectedID pins the guard to a specific disk.
func (g *Guard) SetExpectedID(id string) {
	g.mu.Lock()
	g.expectedID = id
	g.mu.Unlock()
}

// Refresh re-reads the marker and returns the new status.
func (g *Guard) Refresh() Status {
	g.mu.RLock()
	expected := g.expectedID
	g.mu.RUnlock()
	s := Check(g.dir, g.role, expected)
	g.mu.Lock()
	g.status = s
	g.mu.Unlock()
	return s
}

func (g *Guard) Status() Status {
	g.mu.RLock()
	defer g.mu.RUnlock()
	s := g.status
	if g.blocked != "" && s.OK {
		s.OK, s.Problem = false, g.blocked
	}
	return s
}

// ErrUnavailable is returned by Writable when the volume is not safe to use.
var ErrUnavailable = errors.New("library volume unavailable")

// Writable returns nil if writes are allowed.
func (g *Guard) Writable() error {
	s := g.Status()
	if !s.OK {
		return fmt.Errorf("%w: %s", ErrUnavailable, s.Problem)
	}
	return nil
}
