package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func artKey(data []byte) string {
	h := sha256.Sum256(data)
	return "art-" + hex.EncodeToString(h[:])
}

func putLocal(t *testing.T, s localStore, key string, data []byte) {
	t.Helper()
	if err := s.Put(context.Background(), key, bytes.NewReader(data), int64(len(data)), ""); err != nil {
		t.Fatal(err)
	}
}

// fakeStore serves a fixed listing and lets a test shape Get and Put.
type fakeStore struct {
	objectStore
	list []objectInfo
	get  func(key string) (io.ReadCloser, error)
	put  map[string][]byte
}

func (f *fakeStore) List(context.Context, string) ([]objectInfo, error) { return f.list, nil }
func (f *fakeStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
	return f.get(key)
}
func (f *fakeStore) Stat(_ context.Context, key string) (objectInfo, error) {
	if b, ok := f.put[key]; ok {
		return objectInfo{Key: key, Size: int64(len(b))}, nil
	}
	return objectInfo{}, errObjectNotFound
}
func (f *fakeStore) Put(_ context.Context, key string, body io.ReadSeeker, _ int64, _ string) error {
	b, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if f.put == nil {
		f.put = map[string][]byte{}
	}
	f.put[key] = b
	return nil
}

func TestCopyObjectsNewSkipIdempotent(t *testing.T) {
	src, dst := localStore{dir: t.TempDir()}, localStore{dir: filepath.Join(t.TempDir(), "b")}
	a, b := []byte("alpha"), []byte("beta bytes")
	putLocal(t, src, artKey(a), a)
	putLocal(t, src, "att-1", b)
	var out bytes.Buffer
	rep, err := copyObjects(context.Background(), src, dst, &out)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Listed != 2 || rep.Copied != 2 || rep.Bytes != int64(len(a)+len(b)) || rep.Failed() {
		t.Fatalf("first run: %+v", rep)
	}
	got, err := os.ReadFile(filepath.Join(dst.dir, "att-1"))
	if err != nil || !bytes.Equal(got, b) {
		t.Fatalf("copied bytes differ: %v", err)
	}
	rep, err = copyObjects(context.Background(), src, dst, &out)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Copied != 0 || rep.Present != 2 || rep.Failed() {
		t.Fatalf("second run: %+v", rep)
	}
	// The staging directory must not be listed as an object and must be empty.
	list, _ := dst.List(context.Background(), "")
	if len(list) != 2 {
		t.Fatalf("destination lists %d objects", len(list))
	}
	if left, _ := os.ReadDir(filepath.Join(dst.dir, incomingDir)); len(left) != 0 {
		t.Fatalf("staging dir not empty: %d", len(left))
	}
}

func TestCopyObjectsConflictNotOverwritten(t *testing.T) {
	src, dst := localStore{dir: t.TempDir()}, localStore{dir: t.TempDir()}
	putLocal(t, src, "att-1", []byte("new longer content"))
	putLocal(t, dst, "att-1", []byte("old"))
	var out bytes.Buffer
	rep, err := copyObjects(context.Background(), src, dst, &out)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Conflicts != 1 || rep.Copied != 0 || !rep.Failed() {
		t.Fatalf("%+v", rep)
	}
	got, _ := os.ReadFile(filepath.Join(dst.dir, "att-1"))
	if string(got) != "old" {
		t.Fatalf("destination was overwritten: %q", got)
	}
	if strings.Contains(out.String(), "content") {
		t.Fatal("report leaks object content")
	}
}

func TestCopyObjectsHashMismatchRefused(t *testing.T) {
	src, dst := localStore{dir: t.TempDir()}, localStore{dir: t.TempDir()}
	bad := artKey([]byte("expected"))
	putLocal(t, src, bad, []byte("tampered"))
	rep, err := copyObjects(context.Background(), src, dst, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if rep.HashErrors != 1 || rep.Copied != 0 || !rep.Failed() {
		t.Fatalf("%+v", rep)
	}
	if _, err := os.Stat(filepath.Join(dst.dir, bad)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mismatching object was stored: %v", err)
	}
}

func TestCopyObjectsInvalidKeySkipped(t *testing.T) {
	dst := localStore{dir: t.TempDir()}
	src := &fakeStore{list: []objectInfo{{Key: "../evil", Size: 1}, {Key: "a/b", Size: 1}}, get: func(string) (io.ReadCloser, error) {
		t.Fatal("Get must not be called for an invalid key")
		return nil, nil
	}}
	rep, err := copyObjects(context.Background(), src, dst, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Skipped != 2 || rep.Copied != 0 || rep.Failed() {
		t.Fatalf("%+v", rep)
	}
}

type failingReader struct{ n int }

func (f *failingReader) Read(p []byte) (int, error) {
	if f.n == 0 {
		return 0, errors.New("connection reset")
	}
	f.n--
	p[0] = 'x'
	return 1, nil
}
func (f *failingReader) Close() error { return nil }

func TestCopyObjectsCrashLeavesNoPartialFile(t *testing.T) {
	dst := localStore{dir: t.TempDir()}
	src := &fakeStore{list: []objectInfo{{Key: "att-1", Size: 100}}, get: func(string) (io.ReadCloser, error) { return &failingReader{n: 10}, nil }}
	rep, err := copyObjects(context.Background(), src, dst, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Errors != 1 || rep.Copied != 0 {
		t.Fatalf("%+v", rep)
	}
	if _, err := os.Stat(filepath.Join(dst.dir, "att-1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial file under the final name: %v", err)
	}
	if left, _ := os.ReadDir(filepath.Join(dst.dir, incomingDir)); len(left) != 0 {
		t.Fatalf("staging file left behind: %d", len(left))
	}
	// A stale staging file from a killed process is cleaned and never listed.
	stale := filepath.Join(dst.dir, incomingDir, ".obj-stale")
	if err := os.WriteFile(stale, []byte("half"), 0600); err != nil {
		t.Fatal(err)
	}
	if list, _ := dst.List(context.Background(), ""); len(list) != 0 {
		t.Fatalf("staging content listed: %v", list)
	}
	if _, err := copyObjects(context.Background(), &fakeStore{}, dst, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale staging file not removed")
	}
}

func TestCopyObjectsToNonLocalStore(t *testing.T) {
	src := localStore{dir: t.TempDir()}
	data := []byte("restore me")
	putLocal(t, src, artKey(data), data)
	dst := &fakeStore{}
	rep, err := copyObjects(context.Background(), src, dst, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Copied != 1 || !bytes.Equal(dst.put[artKey(data)], data) {
		t.Fatalf("%+v", rep)
	}
}

func TestCheckStorageOverBackupDirReportsMissing(t *testing.T) {
	dir := localStore{dir: t.TempDir()}
	have := []byte("present")
	putLocal(t, dir, artKey(have), have)
	refs := []storageRef{
		{Key: artKey(have), Size: int64(len(have)), SHA256: strings.TrimPrefix(artKey(have), "art-"), Kinds: []string{"artifact blob"}},
		{Key: "att-gone", Size: 5, Kinds: []string{"attachment"}},
	}
	rep, err := checkStorage(context.Background(), func(context.Context) ([]storageRef, error) { return refs, nil }, dir, StorageCheckConfig{VerifyHashes: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Missing) != 1 || rep.Missing[0].Key != "att-gone" || !rep.Failed() {
		t.Fatalf("%+v", rep)
	}
}

func TestBackupSnapshotIgnoresReferencesAfterSnapshot(t *testing.T) {
	src, dst := localStore{dir: t.TempDir()}, localStore{dir: t.TempDir()}
	a, late := []byte("before"), []byte("uploaded during the copy")
	putLocal(t, src, artKey(a), a)
	calls := 0
	load := func(context.Context) ([]storageRef, error) {
		calls++
		refs := []storageRef{{Key: artKey(a), Size: int64(len(a)), SHA256: strings.TrimPrefix(artKey(a), "art-"), Kinds: []string{"artifact blob"}}}
		if calls > 1 { // a later read would see the new row, whose object the copy may not have
			refs = append(refs, storageRef{Key: artKey(late), Size: int64(len(late)), Kinds: []string{"artifact blob"}})
		}
		return refs, nil
	}
	rep, err := backupAndCheck(context.Background(), load, src, dst, BackupObjectsConfig{VerifyHashes: true, Out: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || rep.Failed() || len(rep.Check.Missing) != 0 {
		t.Fatalf("calls=%d %+v", calls, rep.Check)
	}
}

func TestBackupSnapshotReportsReferencedButAbsent(t *testing.T) {
	src, dst := localStore{dir: t.TempDir()}, localStore{dir: t.TempDir()}
	load := func(context.Context) ([]storageRef, error) {
		return []storageRef{{Key: "att-lost", Size: 3, Kinds: []string{"attachment"}}}, nil
	}
	rep, err := backupAndCheck(context.Background(), load, src, dst, BackupObjectsConfig{Out: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Check.Missing) != 1 || !rep.Failed() {
		t.Fatalf("%+v", rep.Check)
	}
}

func TestCopyObjectsStagingNameSkipped(t *testing.T) {
	dst := localStore{dir: t.TempDir()}
	src := &fakeStore{list: []objectInfo{{Key: incomingDir, Size: 1}}, get: func(string) (io.ReadCloser, error) {
		t.Fatal("Get must not be called for the staging name")
		return nil, nil
	}}
	rep, err := copyObjects(context.Background(), src, dst, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Skipped != 1 || rep.Copied != 0 || rep.Failed() {
		t.Fatalf("%+v", rep)
	}
	if fi, err := os.Stat(filepath.Join(dst.dir, incomingDir)); err != nil || !fi.IsDir() {
		t.Fatal("staging dir damaged")
	}
}
