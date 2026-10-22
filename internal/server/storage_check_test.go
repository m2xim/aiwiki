package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestS3StoreListPaginatesAndStripsPrefix(t *testing.T) {
	s3Env(t)
	fake, srv := newFakeS3(t)
	st, err := newS3Store(context.Background(), S3Config{Bucket: "wiki", Region: "us-east-1", Endpoint: srv.URL, Prefix: "team/a", PathStyle: true, AllowInsecureLoopback: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, k := range []string{"a1", "a2", "a3", "b1", "art-x"} {
		if err = st.Put(ctx, k, strings.NewReader("data-"+k), int64(len("data-"+k)), ""); err != nil {
			t.Fatal(err)
		}
	}
	fake.objects["wiki/other/zzz"] = []byte("not ours") // outside the store prefix
	all, err := st.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 5 || fake.lists < 3 {
		t.Fatalf("expected 5 objects over >=3 pages, got %d objects, %d pages", len(all), fake.lists)
	}
	seen := map[string]int64{}
	for _, o := range all {
		seen[o.Key] = o.Size
		if o.Modified.IsZero() {
			t.Fatal("no modification time")
		}
	}
	if seen["a1"] != int64(len("data-a1")) || seen["zzz"] != 0 || seen["other/zzz"] != 0 {
		t.Fatalf("unexpected listing %v", seen)
	}
	onlyA, err := st.List(ctx, "a")
	if err != nil || len(onlyA) != 4 {
		t.Fatalf("prefix listing: %v %v", onlyA, err)
	}
	if _, err = st.List(ctx, "bad\\prefix"); err == nil {
		t.Fatal("backslash prefix accepted")
	}
}

func TestLocalStoreList(t *testing.T) {
	dir := t.TempDir()
	st := localStore{dir: dir}
	ctx := context.Background()
	if err := st.Put(ctx, "f1", strings.NewReader("12345"), 5, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.Put(ctx, "g1", strings.NewReader("1"), 1, ""); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	all, err := st.List(ctx, "")
	if err != nil || len(all) != 2 {
		t.Fatalf("list: %v %v", all, err)
	}
	f, err := st.List(ctx, "f")
	if err != nil || len(f) != 1 || f[0].Key != "f1" || f[0].Size != 5 || f[0].Modified.IsZero() {
		t.Fatalf("prefix list: %+v %v", f, err)
	}
	missing, err := localStore{dir: filepath.Join(dir, "none")}.List(ctx, "")
	if err != nil || len(missing) != 0 {
		t.Fatalf("missing dir: %v %v", missing, err)
	}
}

func shaHex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// memStoreAt is a memStore with controllable modification times.
type memStoreAt struct {
	*memStore
	age map[string]time.Duration
}

func (m memStoreAt) List(ctx context.Context, prefix string) ([]objectInfo, error) {
	objs, err := m.memStore.List(ctx, prefix)
	for i := range objs {
		objs[i].Modified = time.Now().Add(-m.age[objs[i].Key])
	}
	return objs, err
}

func TestCheckStorageReport(t *testing.T) {
	ctx := context.Background()
	ms := newMemStore()
	good, bad := []byte("good bytes"), []byte("corrupted!")
	put := func(k string, b []byte) {
		if err := ms.Put(ctx, k, bytes.NewReader(b), int64(len(b)), ""); err != nil {
			t.Fatal(err)
		}
	}
	put("att-ok", []byte("12345"))
	put("att-short", []byte("123"))
	put("art-"+shaHex(good), good)
	put("art-"+shaHex([]byte("original")), bad) // same length class, wrong content
	put("old-orphan", []byte("x"))
	put("new-orphan", []byte("y"))
	store := memStoreAt{ms, map[string]time.Duration{"old-orphan": 48 * time.Hour, "new-orphan": time.Hour}}
	refs := []storageRef{
		{Key: "att-ok", Size: 5, Kinds: []string{"attachment"}},
		{Key: "att-gone", Size: 9, Kinds: []string{"attachment"}},
		{Key: "att-short", Size: 10, Kinds: []string{"attachment"}},
		{Key: "art-" + shaHex(good), Size: int64(len(good)), SHA256: shaHex(good), Kinds: []string{"artifact blob"}},
		{Key: "art-" + shaHex([]byte("original")), Size: int64(len(bad)), SHA256: shaHex([]byte("original")), Kinds: []string{"artifact blob"}},
	}
	load := func(context.Context) ([]storageRef, error) { return refs, nil }

	rep, err := checkStorage(ctx, load, store, StorageCheckConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.References != 5 || rep.Objects != 6 || len(rep.Missing) != 1 || rep.Missing[0].Key != "att-gone" || len(rep.SizeMismatch) != 1 {
		t.Fatalf("report %+v", rep)
	}
	if len(rep.Orphans) != 2 || rep.YoungOrphans != 1 || len(rep.HashMismatch) != 0 || !rep.Failed() {
		t.Fatalf("report %+v", rep)
	}
	rep, err = checkStorage(ctx, load, store, StorageCheckConfig{VerifyHashes: true})
	if err != nil || len(rep.HashMismatch) != 1 {
		t.Fatalf("hash check: %+v %v", rep, err)
	}
	var out bytes.Buffer
	rep.write(&out)
	text := out.String()
	for _, want := range []string{"MISSING (in database, not in bucket): 1", "att-gone", "RESULT: FAILED", "old-orphan"} {
		if !strings.Contains(text, want) {
			t.Fatalf("report lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "good bytes") || strings.Contains(text, "corrupted") {
		t.Fatal("report printed object content")
	}

	// Deleting orphans: needs -yes, spares young ones and anything a fresh read now references.
	if rep, _ = checkStorage(ctx, load, store, StorageCheckConfig{DeleteOrphans: true}); rep.OrphansDeleted != 0 {
		t.Fatal("deleted without confirmation")
	}
	calls := 0
	racing := func(c context.Context) ([]storageRef, error) {
		calls++
		if calls > 1 {
			return append(append([]storageRef{}, refs...), storageRef{Key: "old-orphan", Size: -1}), nil
		}
		return refs, nil
	}
	if rep, _ = checkStorage(ctx, racing, store, StorageCheckConfig{DeleteOrphans: true, Yes: true}); rep.OrphansDeleted != 0 {
		t.Fatal("deleted an object that gained a reference")
	}
	rep, err = checkStorage(ctx, load, store, StorageCheckConfig{DeleteOrphans: true, Yes: true})
	if err != nil || rep.OrphansDeleted != 1 {
		t.Fatalf("delete: %+v %v", rep, err)
	}
	if _, err = ms.Get(ctx, "old-orphan"); err != errObjectNotFound {
		t.Fatal("old orphan not deleted")
	}
	if _, err = ms.Get(ctx, "new-orphan"); err != nil {
		t.Fatal("young orphan deleted")
	}
	if _, err = ms.Get(ctx, "att-ok"); err != nil {
		t.Fatal("referenced object deleted")
	}
}

func TestCheckStorageCleanAndLocalStore(t *testing.T) {
	ctx := context.Background()
	st := localStore{dir: t.TempDir()}
	if err := st.Put(ctx, "k1", strings.NewReader("abc"), 3, ""); err != nil {
		t.Fatal(err)
	}
	rep, err := checkStorage(ctx, func(context.Context) ([]storageRef, error) {
		return []storageRef{{Key: "k1", Size: 3}}, nil
	}, st, StorageCheckConfig{})
	if err != nil || rep.Failed() || len(rep.Orphans) != 0 {
		t.Fatalf("clean store: %+v %v", rep, err)
	}
	var out bytes.Buffer
	rep.write(&out)
	if !strings.Contains(out.String(), "RESULT: OK") {
		t.Fatal(out.String())
	}
}
