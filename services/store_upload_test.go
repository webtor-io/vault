package services

// An upload has no .torrent: the store and verify-existing must not look for
// one. Postgres: VAULT_TEST_PG_DSN (see testDB).

import (
	"context"
	"strings"
	"testing"
)

const uploadID = "9c5db5752b7c0123456789abcdef0123456789ab"

// storeAs renames the harness resource to id and stores it.
func storeAs(e *storeEnv, id string) error {
	e.t.Helper()
	if _, err := e.db.Exec(`UPDATE resource SET resource_id = ?, status = ? WHERE resource_id = 'ih'`, id, StatusStoring); err != nil {
		e.t.Fatal(err)
	}
	return e.w.handleStore(context.Background(), e.db, id)
}

func linksOf(e *storeEnv, id string) map[string]string {
	e.t.Helper()
	var rfs []ResourceFile
	if err := e.db.Model(&rfs).Where("resource_id = ?", id).Select(); err != nil {
		e.t.Fatal(err)
	}
	out := map[string]string{}
	for _, rf := range rfs {
		out[rf.Path] = rf.FileHash
	}
	return out
}

// An upload stores with verification on although the seeder has no .torrent
// for it; any other id still needs one.
func TestStore_UploadSkipsTorrentCheck(t *testing.T) {
	for _, tc := range []struct {
		id   string
		skip bool
	}{
		{uploadID, true},
		{"9c5db5752b7c1123456789abcdef0123456789ab", true},
		{"9c5db5752b7cf123456789abcdef0123456789ab", true},
		{strings.ToUpper(uploadID), false},
		{"0123456789abcdef0123456789abcdef01234567", false},
	} {
		t.Run(tc.id, func(t *testing.T) {
			files := twoFiles()
			info, _, _ := buildTestTorrent(t, mib, files)
			e := newStoreEnv(t, info, files)
			noTorrent(e)
			err := storeAs(e, tc.id)
			if !tc.skip {
				if err == nil || !strings.Contains(err.Error(), "failed to fetch torrent for verification") {
					t.Fatalf("store: %v, want the torrent fetch failure", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("store: %v", err)
			}
			e.assertStored(files)
			if r, _ := ResourceGetByID(context.Background(), e.db, tc.id); r == nil || r.Status != StatusStored {
				t.Fatalf("resource %+v, want stored", r)
			}
		})
	}
}

// With verification on, an upload next to another resource holding the same
// path and size with other bytes still gets its own bytes.
func TestStore_UploadForeignPathSize(t *testing.T) {
	files := []testFile{{"a", pattern(4, 6*mib+3)}}
	info, _, _ := buildTestTorrent(t, mib, files)
	e := newStoreEnv(t, info, files)
	noTorrent(e)
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
	if err := storeAs(e, uploadID); err != nil {
		t.Fatalf("store: %v", err)
	}
	if h := linksOf(e, uploadID)["/t"]; h != contentKey(files[0].bytes) {
		t.Errorf("/t linked to %q (other=%v), want its own %q; opens=%v", h, h == "other", contentKey(files[0].bytes), e.src.opens)
	}
}

// verify-existing has nothing to verify an upload against: it skips it
// without calling rest-api and without counting an error.
func TestVerifyExisting_SkipsUpload(t *testing.T) {
	files := twoFiles()
	info, _, _ := buildTestTorrent(t, mib, files)
	e := newStoreEnv(t, info, files)
	noTorrent(e)
	if err := storeAs(e, uploadID); err != nil {
		t.Fatalf("store: %v", err)
	}
	e.src.apiCalls = nil
	stats, err := RunVerifyExisting(context.Background(), e.w.pg, e.w.s3, e.w.api, "vault", VerifyExistingOptions{ResourceID: uploadID})
	if err != nil {
		t.Fatalf("verify-existing: %v", err)
	}
	if stats.Resources != 1 || stats.Errors != 0 {
		t.Errorf("stats %+v, want 1 resource and 0 errors", stats)
	}
	if len(e.src.apiCalls) != 0 {
		t.Errorf("rest-api called: %v", e.src.apiCalls)
	}
}
