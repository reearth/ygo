package persistence

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/reearth/ygo/crdt"
)

// FilePersistence is a directory-backed VersionedPersistence. Layout:
//
//	<root>/<roomHex>/updates/<version>.bin     one V1 update per file
//	<root>/<roomHex>/snapshots/<nameHex>.bin   named V1 snapshot
//	<root>/<roomHex>/checkpoint                target version + rolled-back head
//
// Room and snapshot names are hex-encoded so arbitrary Unicode/space names map
// to safe filenames. The checkpoint file is the crash-safety pivot: PruneAfter
// writes it (atomically via temp+rename) before deleting future update files,
// and every read clamps the visible version range to the checkpoint, so a crash
// between the checkpoint write and the deletes can never resurrect a future
// version. Safe for concurrent use via a process-local mutex; it is NOT a
// multi-process lock.
type FilePersistence struct {
	root string
	mu   sync.Mutex

	// crashAfterCheckpoint: see MemoryPersistence. Test-only.
	crashAfterCheckpoint func() bool
}

// NewFilePersistence opens (creating if necessary) a file-backed store rooted at
// dir.
func NewFilePersistence(dir string) (*FilePersistence, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("persistence: mkdir %q: %w", dir, err)
	}
	return &FilePersistence{root: dir}, nil
}

// Reopen returns a fresh handle over the same directory, modelling a process
// restart. Satisfies Reopener for the conformance crash-safety subtest.
func (f *FilePersistence) Reopen() (VersionedPersistence, error) {
	return NewFilePersistence(f.root)
}

// SetCrashAfterCheckpoint satisfies CrashInjector.
func (f *FilePersistence) SetCrashAfterCheckpoint(fn func() bool) {
	f.mu.Lock()
	f.crashAfterCheckpoint = fn
	f.mu.Unlock()
}

func (f *FilePersistence) roomDir(room string) string {
	return filepath.Join(f.root, hex.EncodeToString([]byte(room)))
}

func (f *FilePersistence) updatesDir(room string) string {
	return filepath.Join(f.roomDir(room), "updates")
}

func (f *FilePersistence) snapshotsDir(room string) string {
	return filepath.Join(f.roomDir(room), "snapshots")
}

func (f *FilePersistence) checkpointPath(room string) string {
	return filepath.Join(f.roomDir(room), "checkpoint")
}

// checkpoint holds the persisted prune ceiling and rolled-back head.
type checkpoint struct {
	target     Version
	rolledBack []byte
}

// readCheckpoint reads the room's checkpoint file, or (nil, nil) if absent.
func (f *FilePersistence) readCheckpoint(room string) (*checkpoint, error) {
	data, err := os.ReadFile(f.checkpointPath(room))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// Format: [8 bytes big-endian target][rolledBack...]
	if len(data) < 8 {
		return nil, fmt.Errorf("persistence: corrupt checkpoint for room %q", room)
	}
	cp := &checkpoint{
		target:     Version(binary.BigEndian.Uint64(data[:8])),
		rolledBack: append([]byte(nil), data[8:]...),
	}
	return cp, nil
}

// writeCheckpoint atomically writes the room's checkpoint file (temp + rename).
func (f *FilePersistence) writeCheckpoint(room string, cp *checkpoint) error {
	if err := os.MkdirAll(f.roomDir(room), 0o755); err != nil {
		return err
	}
	buf := make([]byte, 8+len(cp.rolledBack))
	binary.BigEndian.PutUint64(buf[:8], uint64(cp.target))
	copy(buf[8:], cp.rolledBack)
	return atomicWrite(f.checkpointPath(room), buf)
}

// atomicWrite writes data to path via a temp file + rename (atomic on POSIX).
func atomicWrite(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op if rename succeeded
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// listUpdateVersions returns the on-disk update versions, ascending, clamped to
// the checkpoint ceiling if one exists.
func (f *FilePersistence) listUpdateVersions(room string) ([]Version, *checkpoint, error) {
	cp, err := f.readCheckpoint(room)
	if err != nil {
		return nil, nil, err
	}
	entries, err := os.ReadDir(f.updatesDir(room))
	if errors.Is(err, os.ErrNotExist) {
		return nil, cp, nil
	}
	if err != nil {
		return nil, cp, err
	}
	versions := make([]Version, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".bin") {
			continue
		}
		base := strings.TrimSuffix(e.Name(), ".bin")
		n, err := strconv.ParseUint(base, 10, 64)
		if err != nil {
			continue
		}
		v := Version(n)
		if cp != nil && v > cp.target {
			continue // checkpoint ceiling: never expose future versions
		}
		versions = append(versions, v)
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i] < versions[j] })
	return versions, cp, nil
}

func (f *FilePersistence) updatePath(room string, v Version) string {
	return filepath.Join(f.updatesDir(room), strconv.FormatUint(uint64(v), 10)+".bin")
}

// Load returns the materialized head.
func (f *FilePersistence) Load(ctx context.Context, room string) (LoadResult, error) {
	if err := ctx.Err(); err != nil {
		return LoadResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	versions, cp, err := f.listUpdateVersions(room)
	if err != nil {
		return LoadResult{}, err
	}
	if len(versions) == 0 && cp == nil {
		return LoadResult{}, nil
	}
	return f.materializeLocked(room, headVersion(versions, cp), versions, cp)
}

// headVersion returns the highest visible version given the version list and
// optional checkpoint.
func headVersion(versions []Version, cp *checkpoint) Version {
	var head Version
	if len(versions) > 0 {
		head = versions[len(versions)-1]
	}
	if cp != nil && cp.target > head {
		head = cp.target
	}
	return head
}

// materializeLocked folds rolledBack + updates <= v into a head LoadResult.
func (f *FilePersistence) materializeLocked(room string, v Version, versions []Version, cp *checkpoint) (LoadResult, error) {
	blobs := make([][]byte, 0, len(versions)+1)
	if cp != nil && len(cp.rolledBack) > 0 {
		blobs = append(blobs, cp.rolledBack)
	}
	var head Version
	for _, ver := range versions {
		if ver > v {
			break
		}
		data, err := os.ReadFile(f.updatePath(room, ver))
		if err != nil {
			return LoadResult{}, err
		}
		blobs = append(blobs, data)
		head = ver
	}
	if cp != nil && cp.target <= v && cp.target > head {
		head = cp.target
	}
	if len(blobs) == 0 {
		return LoadResult{Version: head}, nil
	}
	merged, err := crdt.MergeUpdatesV1(blobs...)
	if err != nil {
		return LoadResult{}, err
	}
	return LoadResult{Update: merged, Version: head}, nil
}

// nextVersion computes the next version to assign: one past the max of the
// on-disk max version and the checkpoint target.
func (f *FilePersistence) nextVersion(room string) (Version, error) {
	cp, err := f.readCheckpoint(room)
	if err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(f.updatesDir(room))
	var maxV Version
	if err == nil {
		for _, e := range entries {
			base := strings.TrimSuffix(e.Name(), ".bin")
			if n, perr := strconv.ParseUint(base, 10, 64); perr == nil {
				if Version(n) > maxV {
					maxV = Version(n)
				}
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	if cp != nil && cp.target > maxV {
		maxV = cp.target
	}
	return maxV + 1, nil
}

// AppendUpdate appends one V1 update.
func (f *FilePersistence) AppendUpdate(ctx context.Context, room string, update []byte) (Version, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := crdt.ApplyUpdateV1(crdt.New(), update, nil); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := os.MkdirAll(f.updatesDir(room), 0o755); err != nil {
		return 0, err
	}
	v, err := f.nextVersion(room)
	if err != nil {
		return 0, err
	}
	if err := atomicWrite(f.updatePath(room, v), update); err != nil {
		return 0, err
	}
	return v, nil
}

// ListVersions returns metadata newest-first.
//
// Cost: O(n) in the number of stored versions — it does one os.Stat per record
// to read the modification time. Keep histories bounded with Compact (or
// PruneAfter) so this stays cheap; an unbounded log makes every ListVersions
// (and Load/MaterializeAt) linearly slower.
func (f *FilePersistence) ListVersions(ctx context.Context, room string) ([]VersionMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	versions, _, err := f.listUpdateVersions(room)
	if err != nil {
		return nil, err
	}
	out := make([]VersionMeta, 0, len(versions))
	for i := len(versions) - 1; i >= 0; i-- {
		v := versions[i]
		info, err := os.Stat(f.updatePath(room, v))
		ts := time.Time{}
		if err == nil {
			ts = info.ModTime()
		}
		out = append(out, VersionMeta{Version: v, UpdatedAt: ts})
	}
	return out, nil
}

// GetUpdate returns the single update at v.
func (f *FilePersistence) GetUpdate(ctx context.Context, room string, v Version) ([]byte, VersionMeta, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, VersionMeta{}, false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	cp, err := f.readCheckpoint(room)
	if err != nil {
		return nil, VersionMeta{}, false, err
	}
	if cp != nil && v > cp.target {
		return nil, VersionMeta{}, false, nil // checkpoint ceiling
	}
	data, err := os.ReadFile(f.updatePath(room, v))
	if errors.Is(err, os.ErrNotExist) {
		return nil, VersionMeta{}, false, nil
	}
	if err != nil {
		return nil, VersionMeta{}, false, err
	}
	info, err := os.Stat(f.updatePath(room, v))
	ts := time.Time{}
	if err == nil {
		ts = info.ModTime()
	}
	return data, VersionMeta{Version: v, UpdatedAt: ts}, true, nil
}

// MaterializeAt rebuilds the V1 head at v.
func (f *FilePersistence) MaterializeAt(ctx context.Context, room string, v Version) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if v == 0 {
		return nil, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	versions, cp, err := f.listUpdateVersions(room)
	if err != nil {
		return nil, err
	}
	if len(versions) == 0 && cp == nil {
		return nil, ErrRoomNotFound
	}
	res, err := f.materializeLocked(room, v, versions, cp)
	if err != nil {
		return nil, err
	}
	return res.Update, nil
}

// CaptureSnapshot stores a named V1 snapshot.
func (f *FilePersistence) CaptureSnapshot(ctx context.Context, room, name string, state []byte) (Version, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	versions, cp, err := f.listUpdateVersions(room)
	if err != nil {
		return 0, err
	}
	v := headVersion(versions, cp)
	if err := os.MkdirAll(f.snapshotsDir(room), 0o755); err != nil {
		return 0, err
	}
	// Format: [8 bytes version][state...]
	buf := make([]byte, 8+len(state))
	binary.BigEndian.PutUint64(buf[:8], uint64(v))
	copy(buf[8:], state)
	if err := atomicWrite(f.snapshotPath(room, name), buf); err != nil {
		return 0, err
	}
	return v, nil
}

func (f *FilePersistence) snapshotPath(room, name string) string {
	return filepath.Join(f.snapshotsDir(room), hex.EncodeToString([]byte(name))+".bin")
}

// RestoreSnapshot returns a named snapshot.
func (f *FilePersistence) RestoreSnapshot(ctx context.Context, room, name string) ([]byte, Version, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	data, err := os.ReadFile(f.snapshotPath(room, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	if len(data) < 8 {
		return nil, 0, false, fmt.Errorf("persistence: corrupt snapshot %q/%q", room, name)
	}
	v := Version(binary.BigEndian.Uint64(data[:8]))
	return append([]byte(nil), data[8:]...), v, true, nil
}

// PruneAfter implements snapshot-before-delete.
func (f *FilePersistence) PruneAfter(ctx context.Context, room string, target Version, rolledBack []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	if _, err := os.Stat(f.roomDir(room)); errors.Is(err, os.ErrNotExist) {
		f.mu.Unlock()
		return ErrRoomNotFound
	}

	// Step 1: write checkpoint (atomic). From this point readers clamp to target.
	if err := f.writeCheckpoint(room, &checkpoint{target: target, rolledBack: rolledBack}); err != nil {
		f.mu.Unlock()
		return err
	}
	crash := f.crashAfterCheckpoint
	f.mu.Unlock()

	if crash != nil && crash() {
		return nil // simulated crash before deletes
	}

	// Step 2: delete update files newer than target.
	f.mu.Lock()
	defer f.mu.Unlock()
	entries, err := os.ReadDir(f.updatesDir(room))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		base := strings.TrimSuffix(e.Name(), ".bin")
		n, perr := strconv.ParseUint(base, 10, 64)
		if perr != nil {
			continue
		}
		if Version(n) > target {
			if rmErr := os.Remove(filepath.Join(f.updatesDir(room), e.Name())); rmErr != nil {
				return rmErr
			}
		}
	}
	return nil
}

// Compact folds the oldest updates into the oldest retained one.
func (f *FilePersistence) Compact(ctx context.Context, room string, keep int) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if keep <= 0 {
		return 0, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	versions, cp, err := f.listUpdateVersions(room)
	if err != nil {
		return 0, err
	}
	if len(versions) <= keep {
		return 0, nil
	}
	trimEnd := len(versions) - keep // fold [0:trimEnd] into versions[trimEnd]
	blobs := make([][]byte, 0, trimEnd+2)
	if cp != nil && len(cp.rolledBack) > 0 {
		blobs = append(blobs, cp.rolledBack)
	}
	for i := 0; i <= trimEnd; i++ {
		data, rerr := os.ReadFile(f.updatePath(room, versions[i]))
		if rerr != nil {
			return 0, rerr
		}
		blobs = append(blobs, data)
	}
	merged, err := crdt.MergeUpdatesV1(blobs...)
	if err != nil {
		return 0, err
	}
	// Overwrite the oldest retained version's file with the folded state, then
	// delete the trimmed-away files. The folded blob now also subsumes the
	// rolled-back head, so we can clear the checkpoint's rolledBack to avoid
	// double-applying it on future Loads.
	foldVersion := versions[trimEnd]
	if err := atomicWrite(f.updatePath(room, foldVersion), merged); err != nil {
		return 0, err
	}
	deleted := 0
	for i := 0; i < trimEnd; i++ {
		if err := os.Remove(f.updatePath(room, versions[i])); err != nil {
			return deleted, err
		}
		deleted++
	}
	if cp != nil && len(cp.rolledBack) > 0 {
		if err := f.writeCheckpoint(room, &checkpoint{target: cp.target}); err != nil {
			return deleted, err
		}
	}
	return deleted, nil
}

// Delete removes all data for room.
func (f *FilePersistence) Delete(ctx context.Context, room string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return os.RemoveAll(f.roomDir(room))
}
