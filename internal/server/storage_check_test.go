package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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
	// Missing objects forbid deletion altogether.
	rep, err = checkStorage(ctx, load, store, StorageCheckConfig{DeleteOrphans: true, Yes: true})
	var refused errDeleteRefused
	if !errors.As(err, &refused) || rep == nil || rep.OrphansDeleted != 0 {
		t.Fatalf("delete with missing objects: %+v %v", rep, err)
	}
	if _, err = ms.Get(ctx, "old-orphan"); err != nil {
		t.Fatal("orphan deleted although objects are missing")
	}
}

// storeAt is a memStore with controllable modification times.
type storeAt struct {
	*memStore
	mod    map[string]time.Time
	onStat func(key string)
}

func newStoreAt(ages map[string]time.Duration) *storeAt {
	s := &storeAt{memStore: newMemStore(), mod: map[string]time.Time{}}
	for k, a := range ages {
		s.mod[k] = time.Now().Add(-a).Truncate(time.Second)
	}
	return s
}

func (s *storeAt) put(t *testing.T, k, data string) {
	t.Helper()
	if err := s.memStore.Put(context.Background(), k, strings.NewReader(data), int64(len(data)), ""); err != nil {
		t.Fatal(err)
	}
}

func (s *storeAt) when(k string) time.Time {
	if m, ok := s.mod[k]; ok {
		return m
	}
	return time.Now()
}

func (s *storeAt) List(ctx context.Context, prefix string) ([]objectInfo, error) {
	objs, err := s.memStore.List(ctx, prefix)
	for i := range objs {
		objs[i].Modified = s.when(objs[i].Key)
	}
	return objs, err
}

func (s *storeAt) Stat(ctx context.Context, key string) (objectInfo, error) {
	if s.onStat != nil {
		s.onStat(key)
	}
	o, err := s.memStore.Stat(ctx, key)
	o.Modified = s.when(key)
	return o, err
}

func TestCheckStorageDeleteGuards(t *testing.T) {
	ctx := context.Background()
	const day = 48 * time.Hour
	mk := func(refKeys []string, orphans ...string) (*storeAt, func(context.Context) ([]storageRef, error)) {
		ages := map[string]time.Duration{}
		st := newStoreAt(ages)
		var refs []storageRef
		for _, k := range refKeys {
			st.mod[k] = time.Now().Add(-day)
			st.put(t, k, "data")
			refs = append(refs, storageRef{Key: k, Size: 4})
		}
		for _, k := range orphans {
			st.mod[k] = time.Now().Add(-day)
			st.put(t, k, "junk")
		}
		return st, func(context.Context) ([]storageRef, error) { return refs, nil }
	}
	del := StorageCheckConfig{DeleteOrphans: true, Yes: true}
	has := func(st *storeAt, k string) bool { _, err := st.memStore.Get(ctx, k); return err == nil }

	// Healthy: the orphan goes, referenced objects stay.
	st, load := mk([]string{"a", "b"}, "o1")
	rep, err := checkStorage(ctx, load, st, del)
	if err != nil || rep.OrphansDeleted != 1 || has(st, "o1") || !has(st, "a") {
		t.Fatalf("healthy delete: %+v %v", rep, err)
	}

	// Without -yes nothing is deleted.
	st, load = mk([]string{"a"}, "o1")
	if rep, err = checkStorage(ctx, load, st, StorageCheckConfig{DeleteOrphans: true}); err != nil || rep.OrphansDeleted != 0 || !has(st, "o1") {
		t.Fatalf("no -yes: %+v %v", rep, err)
	}

	// Empty database (migrated schema) + bucket with old objects: refused.
	st, load = mk(nil, "o1", "o2", "o3")
	rep, err = checkStorage(ctx, load, st, del)
	var refused errDeleteRefused
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "no objects at all") || rep == nil || rep.OrphansDeleted != 0 || !has(st, "o1") || !has(st, "o3") {
		t.Fatalf("empty database: %+v %v", rep, err)
	}
	// ...unless explicitly allowed.
	allow := del
	allow.AllowEmptyDatabase = true
	if rep, err = checkStorage(ctx, load, st, allow); err != nil || rep.OrphansDeleted != 3 {
		t.Fatalf("allowed empty database: %+v %v", rep, err)
	}

	// A database that shares nothing with the bucket (wrong database): its
	// references are missing, so deletion is refused. (The related "every object
	// is an orphan" rule is a second line of defence and cannot fire without
	// missing objects, so it has no separate case.)
	st, _ = mk(nil, "o1", "o2")
	foreign := func(context.Context) ([]storageRef, error) { return []storageRef{{Key: "other", Size: -1}}, nil }
	rep, err = checkStorage(ctx, foreign, st, del)
	if !errors.As(err, &refused) || rep.OrphansDeleted != 0 || !has(st, "o1") || !has(st, "o2") {
		t.Fatalf("foreign database: %+v %v", rep, err)
	}

	// Race: the object is overwritten between listing and deletion.
	st, load = mk([]string{"a"}, "o1", "o2")
	st.onStat = func(k string) {
		if k == "o1" {
			st.mod["o1"] = time.Now() // a new upload of the same key
		}
	}
	rep, err = checkStorage(ctx, load, st, del)
	if err != nil || rep.OrphansDeleted != 1 || rep.OrphansSkipped != 1 || !has(st, "o1") || has(st, "o2") {
		t.Fatalf("overwrite race: %+v %v", rep, err)
	}

	// Race: a row for the orphan appears before its batch is deleted.
	st, _ = mk([]string{"a"}, "o1")
	calls := 0
	racing := func(context.Context) ([]storageRef, error) {
		calls++
		if calls > 1 {
			return []storageRef{{Key: "a", Size: 4}, {Key: "o1", Size: -1}}, nil
		}
		return []storageRef{{Key: "a", Size: 4}}, nil
	}
	if rep, err = checkStorage(ctx, racing, st, del); err != nil || rep.OrphansDeleted != 0 || !has(st, "o1") {
		t.Fatalf("new reference race: %+v %v", rep, err)
	}

	// Batches: references are re-read before every batch of orphanDeleteBatch.
	many := make([]string, 0, 250)
	for i := 0; i < 250; i++ {
		many = append(many, fmt.Sprintf("m%03d", i))
	}
	st, _ = mk([]string{"a"}, many...)
	calls = 0
	counting := func(context.Context) ([]storageRef, error) { calls++; return []storageRef{{Key: "a", Size: 4}}, nil }
	if rep, err = checkStorage(ctx, counting, st, del); err != nil || rep.OrphansDeleted != 250 || calls != 1+3 {
		t.Fatalf("batches: deleted %d, loads %d, %v", rep.OrphansDeleted, calls, err)
	}

	// Keys that the server could not have written are reported, not checked or deleted.
	st, load = mk([]string{"a"}, "o1")
	st.put(t, "nested/obj", "x")
	st.mod["nested/obj"] = time.Now().Add(-day)
	rep, err = checkStorage(ctx, load, st, del)
	if err != nil || len(rep.Foreign) != 1 || rep.Objects != 2 || rep.OrphansDeleted != 1 || !has(st, "nested/obj") {
		t.Fatalf("foreign key: %+v %v", rep, err)
	}
	var out bytes.Buffer
	rep.write(&out)
	if !strings.Contains(out.String(), "nested/obj") {
		t.Fatal(out.String())
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

func TestStoreStatAndListPrefixValidation(t *testing.T) {
	s3Env(t)
	_, srv := newFakeS3(t)
	st, err := newS3Store(context.Background(), S3Config{Bucket: "wiki", Region: "us-east-1", Endpoint: srv.URL, Prefix: "p", PathStyle: true, AllowInsecureLoopback: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err = st.Put(ctx, "x0key", strings.NewReader("hello"), 5, ""); err != nil {
		t.Fatal(err)
	}
	info, err := st.Stat(ctx, "x0key")
	if err != nil || info.Size != 5 || info.Modified.Year() != 2020 {
		t.Fatalf("stat: %+v %v", info, err)
	}
	if _, err = st.Stat(ctx, "nokey"); err != errObjectNotFound {
		t.Fatalf("stat missing: %v", err)
	}
	// 'x' and '0' are ordinary characters; NUL and backslash are rejected.
	for _, ok := range []string{"x", "0", "x0", "x0k"} {
		if _, err = st.List(ctx, ok); err != nil {
			t.Fatalf("prefix %q rejected: %v", ok, err)
		}
	}
	if got, _ := st.List(ctx, "x0"); len(got) != 1 {
		t.Fatalf("prefix x0: %v", got)
	}
	for _, bad := range []string{"a\x00b", "\x00", "a" + string(rune(92)) + "b"} {
		if _, err = st.List(ctx, bad); err == nil {
			t.Fatalf("prefix %q accepted", bad)
		}
	}
	ls := localStore{dir: t.TempDir()}
	if _, err = ls.Stat(ctx, "nokey"); err != errObjectNotFound {
		t.Fatal(err)
	}
	_ = ls.Put(ctx, "k", strings.NewReader("abc"), 3, "")
	if fi, err := ls.Stat(ctx, "k"); err != nil || fi.Size != 3 || fi.Modified.IsZero() {
		t.Fatalf("local stat: %+v %v", fi, err)
	}
}
