package services

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The orphan sweeps (the one at the end of every deletion, the vault-gc
// CronJob) leave a failed store's multipart upload alone while its retry can
// still resume it. Before, they aborted it and the retry restarted from byte 0.

// orphanCleaners are the two sweeps; gc runs with prod's 4 h grace.
var orphanCleaners = []struct {
	name string
	run  func(*storeEnv)
}{{"sweep", (*storeEnv).sweep}, {"gc", (*storeEnv).gc}}

// sweep runs sweepOrphanFiles as prod does: at the end of another resource's
// deletion.
func (e *storeEnv) sweep() {
	e.t.Helper()
	if _, err := e.db.Exec(`INSERT INTO resource (resource_id, status) VALUES ('other', ?)`, StatusDeleting); err != nil {
		e.t.Fatal(err)
	}
	if err := e.w.handleDelete(context.Background(), e.db, "other"); err != nil {
		e.t.Fatal(err)
	}
}

func (e *storeEnv) gc() {
	e.t.Helper()
	if _, err := RunGC(context.Background(), e.w.pg, e.w.s3, e.w.bucket, GCOptions{Grace: 4 * time.Hour, Limit: 1000}); err != nil {
		e.t.Fatal(err)
	}
}

// ageFiles moves every file row's updated_at back by age, past the trigger
// that stamps now() on each update.
func (e *storeEnv) ageFiles(age string) {
	e.t.Helper()
	if _, err := e.db.Exec(`ALTER TABLE file DISABLE TRIGGER trg_file_set_updated_at;
		UPDATE file SET updated_at = now() - ?::interval;
		ALTER TABLE file ENABLE TRIGGER trg_file_set_updated_at`, age); err != nil {
		e.t.Fatal(err)
	}
}

// parts counts the parts held by open multipart uploads.
func (e *storeEnv) parts() int {
	e.s3.mu.Lock()
	defer e.s3.mu.Unlock()
	n := 0
	for _, p := range e.s3.uploads {
		n += len(p)
	}
	return n
}

func (e *storeEnv) uploadID(hash string) string {
	e.t.Helper()
	var ids []string
	if _, err := e.db.Query(&ids, `SELECT COALESCE(upload_id, '') FROM file WHERE hash = ?`, hash); err != nil {
		e.t.Fatal(err)
	}
	return strings.Join(ids, "")
}

// failedStore leaves a store failed mid-file: a's row storing, its upload
// open with parts in it, b not started.
func failedStore(t *testing.T) (*storeEnv, []testFile) {
	t.Helper()
	files := twoFiles()
	info, _, _ := buildTestTorrent(t, mib, files)
	e := newStoreEnv(t, info, files)
	failures := 3 // readRange's attempts
	e.src.fail = func(name string, start, end int64) bool {
		if name == "b" && end >= 0 && start == 0 && failures > 0 { // a's tail piece reads b's head
			failures--
			return true
		}
		return false
	}
	if err := e.store(); err == nil {
		t.Fatal("want the first attempt to fail on the source read")
	}
	if e.parts() == 0 {
		t.Fatal("the failed attempt aborted its multipart upload")
	}
	return e, files
}

// A store that failed 5 h ago (past gc's grace, inside the window) keeps its
// upload through both sweeps, and the retry resumes it.
func TestSweep_KeepsResumableUpload(t *testing.T) {
	e, files := failedStore(t)
	key := contentKey(files[0].bytes)
	e.ageFiles("5 hours")
	for _, c := range orphanCleaners {
		c.run(e)
		if id, n := e.uploadID(key), e.parts(); id == "" || n == 0 {
			t.Fatalf("after the %s: upload_id=%q, %d parts; want the upload kept", c.name, id, n)
		}
	}
	if err := e.store(); err != nil {
		t.Fatalf("retry: %v", err)
	}
	e.assertStored(files)
	if got := strings.Join(e.src.opens, " "); strings.Count(got, "a@0") != 1 {
		t.Errorf("streams %q: the retry restarted a from 0", got)
	}
}

// Past the window the upload is no longer kept: each sweep aborts it and
// drops the row.
func TestSweep_RemovesExpiredUpload(t *testing.T) {
	for _, c := range orphanCleaners {
		t.Run(c.name, func(t *testing.T) {
			e, files := failedStore(t)
			key := contentKey(files[0].bytes)
			e.ageFiles("8 days")
			c.run(e)
			if row, n := e.fileRows()[key], e.parts(); row || n != 0 {
				t.Errorf("row kept=%v, %d parts; want both gone", row, n)
			}
		})
	}
}

// Rows that hold no resumable upload are swept as before; a linked file
// never is.
func TestSweep_StillRemovesTrueOrphans(t *testing.T) {
	for _, c := range orphanCleaners {
		t.Run(c.name, func(t *testing.T) {
			e := newStoreEnv(t, nil, nil)
			if _, err := e.db.Exec(`INSERT INTO file (hash, status, upload_id, updated_at) VALUES
				('stored-unlinked', ?0, NULL, now() - interval '1 day'),
				('storing-empty-upload', ?1, '', now() - interval '1 day'),
				('storing-null-upload', ?1, NULL, now() - interval '1 day'),
				('deleting', ?2, 'u-del', now() - interval '1 day'),
				('linked', ?0, NULL, now() - interval '30 days');
				INSERT INTO resource_file (resource_id, file_hash, path) VALUES ('ih', 'linked', '/t/x')`,
				StatusStored, StatusStoring, StatusDeleting); err != nil {
				t.Fatal(err)
			}
			for _, k := range []string{"stored-unlinked", "storing-empty-upload", "storing-null-upload", "linked"} {
				e.s3.objects[k] = []byte(k)
			}
			e.s3.uploads["u-del"] = map[int64][]byte{1: []byte("part")}
			c.run(e)
			rows := e.fileRows()
			for _, k := range []string{"stored-unlinked", "storing-empty-upload", "storing-null-upload", "deleting"} {
				if rows[k] || e.s3.object(k) != nil {
					t.Errorf("%s: row kept=%v, object kept=%v; want both gone", k, rows[k], e.s3.object(k) != nil)
				}
			}
			if n := e.parts(); n != 0 {
				t.Errorf("deleting: %d parts kept; want the upload aborted", n)
			}
			if !rows["linked"] || e.s3.object("linked") == nil {
				t.Errorf("linked: row kept=%v, object kept=%v; want both kept", rows["linked"], e.s3.object("linked") != nil)
			}
		})
	}
}

// The orphan metric counts what the sweeps would remove, not resumable uploads.
func TestOrphanMetricExcludesResumable(t *testing.T) {
	db := testDB(t)
	if _, err := db.Exec(`INSERT INTO file (hash, status, upload_id, total_size) VALUES
		('resumable', ?, 'u1', 10), ('orphan', ?, NULL, 1)`, StatusStoring, StatusStored); err != nil {
		t.Fatal(err)
	}
	got, err := orphanFiles(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if got.Count != 1 || got.Bytes != 1 {
		t.Errorf("orphans %+v; want only the stored one: 1 file, 1 byte", got)
	}
}
