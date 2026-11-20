package services

// Stores without .torrent verification (mi == nil) and empty listings: what an
// upload, which has no .torrent, goes through. Postgres: VAULT_TEST_PG_DSN
// (see testDB).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// noTorrent makes the seeder answer 404 on /{id}/source.torrent, as for an
// id that has no .torrent.
func noTorrent(e *storeEnv) {
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/source.torrent") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		e.src.ServeHTTP(w, r)
	}))
	e.t.Cleanup(h.Close)
	e.w.api.url = h.URL
}

func links(e *storeEnv) map[string]string {
	e.t.Helper()
	var rfs []ResourceFile
	if err := e.db.Model(&rfs).Where("resource_id = 'ih'").Select(); err != nil {
		e.t.Fatal(err)
	}
	out := map[string]string{}
	for _, rf := range rfs {
		out[rf.Path] = rf.FileHash
	}
	return out
}

func status(e *storeEnv) Status {
	e.t.Helper()
	r, err := ResourceGetByID(context.Background(), e.db, "ih")
	if err != nil || r == nil {
		e.t.Fatalf("resource: %v %v", r, err)
	}
	return r.Status
}

// Q1: verification on, no .torrent: the whole store fails.
func TestFacts_NoTorrentFailsStore(t *testing.T) {
	files := []testFile{{"a", pattern(1, 2*mib+1)}}
	info, _, _ := buildTestTorrent(t, mib, files)
	e := newStoreEnv(t, info, files)
	noTorrent(e)
	err := e.store()
	if err == nil || !strings.Contains(err.Error(), "failed to fetch torrent for verification") {
		t.Fatalf("store: %v, want the torrent fetch failure", err)
	}
	t.Logf("error: %v", err)
}

// Q1: verification off (mi == nil): the same id stores fine.
func TestFacts_NoMiStores(t *testing.T) {
	files := twoFiles()
	info, _, _ := buildTestTorrent(t, mib, files)
	e := newStoreEnv(t, info, files)
	e.w.verifyIntegrity = false
	noTorrent(e)
	if err := e.store(); err != nil {
		t.Fatalf("store: %v", err)
	}
	e.assertStored(files)
	if s := status(e); s != StatusStored {
		t.Fatalf("status %v", s)
	}
}

// Q2: mi == nil, another resource holds the same path and size with other
// bytes. Correct: the resource gets its own bytes. Fails on b8f14d6.
func TestFacts_NoMiCrossResourcePathSize(t *testing.T) {
	files := []testFile{{"a", pattern(4, 6*mib+3)}}
	info, _, _ := buildTestTorrent(t, mib, files)
	e := newStoreEnv(t, info, files)
	e.w.verifyIntegrity = false
	other := pattern(5, len(files[0].bytes))
	for _, q := range []string{
		`INSERT INTO resource (resource_id, status) VALUES ('other-resource', 2)`,
		`INSERT INTO file (hash, status, total_size, stored_size) VALUES ('other', 2, 6291459, 6291459)`,
		`INSERT INTO resource_file (resource_id, file_hash, path) VALUES ('other-resource', 'other', '/t')`,
	} {
		if _, err := e.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	e.s3.objects["other"] = other
	if err := e.store(); err != nil {
		t.Fatalf("store: %v", err)
	}
	if h := links(e)["/t"]; h != contentKey(files[0].bytes) {
		t.Errorf("/t linked to %q (other=%v), want its own %q; opens=%v", h, h == "other", contentKey(files[0].bytes), e.src.opens)
	}
}

// Q3: PUT on a stored resource leaves it stored and nothing claims it.
func TestFacts_PutStoredIsNoop(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if _, err := db.Exec(`INSERT INTO resource (resource_id, status) VALUES ('up', 2)`); err != nil {
		t.Fatal(err)
	}
	res, err := ResourceQueueForStoring(ctx, db, "up", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusStored {
		t.Fatalf("status after PUT %v", res.Status)
	}
	w := &Worker{}
	got, err := w.tryClaim(ctx, db, "pod#1", false)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("claimed %v", got.ID)
	}
}

// Q3: a re-store with a changed listing adds the new file, keeps the
// unchanged one without downloading it, prunes the removed one.
func TestFacts_RestoreAddsAndPrunes(t *testing.T) {
	a, b, c := testFile{"a", pattern(1, 3*mib+1)}, testFile{"b", pattern(2, 2*mib+7)}, testFile{"c", pattern(3, mib+5)}
	info, _, _ := buildTestTorrent(t, mib, []testFile{a, b})
	e := newStoreEnv(t, info, []testFile{a, b})
	e.w.verifyIntegrity = false
	if err := e.store(); err != nil {
		t.Fatalf("store 1: %v", err)
	}
	e.src.files = []testFile{a, c}
	e.src.opens = nil
	if err := e.store(); err != nil {
		t.Fatalf("store 2: %v", err)
	}
	l := links(e)
	if len(l) != 2 || l["/t/a"] != contentKey(a.bytes) || l["/t/c"] != contentKey(c.bytes) {
		t.Errorf("links %v", l)
	}
	if e.s3.object(contentKey(b.bytes)) != nil {
		t.Errorf("b's object still in S3")
	}
	for _, o := range e.src.opens {
		if strings.HasPrefix(o, "a@") {
			t.Errorf("a downloaded again: %v", e.src.opens)
		}
	}
	r, _ := ResourceGetByID(context.Background(), e.db, "ih")
	if r.TotalSize != int64(len(a.bytes)+len(c.bytes)) || r.Status != StatusStored {
		t.Errorf("resource %+v", r)
	}
	t.Logf("opens on re-store: %v", e.src.opens)
}

// Q3: rest-api 404 on list (id unknown, e.g. after eviction) must not read as an
// empty listing: the re-store fails and keeps the stored copy.
func TestFacts_List404KeepsStoredCopy(t *testing.T) {
	files := twoFiles()
	info, _, _ := buildTestTorrent(t, mib, files)
	e := newStoreEnv(t, info, files)
	if err := e.store(); err != nil {
		t.Fatalf("store 1: %v", err)
	}
	e.src.gate = func(string) (int, string) { return http.StatusNotFound, `{"error":"not found infoHash=ih"}` }
	err := e.store()
	l := links(e)
	s := status(e)
	t.Logf("store 2 err=%v links=%d status=%v objects=%d", err, len(l), s, len(e.s3.objects))
	if err == nil || len(l) != 2 {
		t.Errorf("404 listing: err=%v links=%d status=%v, want an error and both links kept", err, len(l), s)
	}
}

// rest-api 404 on a later page of the listing (the id went away between two
// calls) must not read as the end of the listing: the re-store fails and
// keeps every file.
func TestStore_List404OnSecondPageKeepsLinks(t *testing.T) {
	files := twoFiles()
	info, _, _ := buildTestTorrent(t, mib, files)
	e := newStoreEnv(t, info, files)
	if err := e.store(); err != nil {
		t.Fatalf("store 1: %v", err)
	}
	// Page 1 is full (file a and 99 directories), page 2 is a 404.
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/list") {
			e.src.ServeHTTP(w, r)
			return
		}
		if r.URL.Query().Get("offset") != "0" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		items := []map[string]any{{"id": "a", "path": "/t/a", "type": "file", "size": len(files[0].bytes)}}
		for len(items) < 100 {
			items = append(items, map[string]any{"type": "directory"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
	}))
	t.Cleanup(h.Close)
	e.w.api.url = h.URL
	err := e.store()
	if err == nil || !strings.Contains(err.Error(), "failed to list resource content") {
		t.Fatalf("store 2: %v, want the listing failure", err)
	}
	if l := links(e); len(l) != 2 {
		t.Errorf("links %v, want both kept", l)
	}
	e.assertStored(files)
}

// Review Focus: rest-api lists 200 with no files (an upload with every file
// still in parts/): the re-store fails and keeps the stored copy.
func TestStore_EmptyListingKeepsLinks(t *testing.T) {
	files := twoFiles()
	info, _, _ := buildTestTorrent(t, mib, files)
	e := newStoreEnv(t, info, files)
	if err := e.store(); err != nil {
		t.Fatalf("store 1: %v", err)
	}
	e.src.files = nil
	err := e.store()
	if err == nil || !strings.Contains(err.Error(), "empty listing: nothing to store") {
		t.Fatalf("store 2: %v, want the empty listing error", err)
	}
	if l := links(e); len(l) != 2 {
		t.Errorf("links %v, want both kept", l)
	}
	e.assertStored(files)
}

// Review Focus: with mi == nil the same bytes at the same path in another
// resource still dedup, by content hash, without a full download.
func TestStore_NoMiSameBytesDedupsByHash(t *testing.T) {
	files := twoFiles()
	info, _, _ := buildTestTorrent(t, mib, files)
	e := newStoreEnv(t, info, files)
	e.w.verifyIntegrity = false
	key := contentKey(files[0].bytes)
	for _, q := range []string{
		`INSERT INTO resource (resource_id, status) VALUES ('other-resource', 2)`,
		`INSERT INTO file (hash, status, total_size, stored_size) VALUES ('` + key + `', 2, 12583257, 12583257)`,
		`INSERT INTO resource_file (resource_id, file_hash, path) VALUES ('other-resource', '` + key + `', '/t/a')`,
	} {
		if _, err := e.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	e.s3.objects[key] = files[0].bytes
	if err := e.store(); err != nil {
		t.Fatalf("store: %v", err)
	}
	if h := links(e)["/t/a"]; h != key {
		t.Errorf("/t/a linked to %q, want the shared %q", h, key)
	}
	if slices.Contains(e.src.opens, "a@0") { // the tail read for the hash opens a@<size-512000>
		t.Errorf("a downloaded in full: %v", e.src.opens)
	}
}
