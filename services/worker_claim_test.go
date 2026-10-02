package services

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/go-pg/pg/v10"
)

// These run the claim statement and the shutdown path against a real
// Postgres: VAULT_TEST_PG_DSN names an empty database the tests may wipe.
//
//	docker run --rm -d --name vault-pg -p 55432:5432 -e POSTGRES_PASSWORD=x postgres:16
//	VAULT_TEST_PG_DSN='postgres://postgres:x@localhost:55432/postgres?sslmode=disable' go test ./services/
func testDB(t *testing.T) *pg.DB {
	t.Helper()
	dsn := os.Getenv("VAULT_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("VAULT_TEST_PG_DSN not set")
	}
	opt, err := pg.ParseURL(dsn)
	if err != nil {
		t.Fatal(err)
	}
	db := pg.Connect(opt)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec("DROP SCHEMA public CASCADE; CREATE SCHEMA public"); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob("../migrations/*.up.sql")
	num := func(f string) int { n, _ := strconv.Atoi(strings.SplitN(filepath.Base(f), "_", 2)[0]); return n }
	sort.Slice(files, func(i, j int) bool { return num(files[i]) < num(files[j]) })
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(b)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
	return db
}

func seedClaimRows(t *testing.T, db *pg.DB) {
	t.Helper()
	for _, q := range []string{
		`INSERT INTO resource (resource_id, status) VALUES ('queued', 0), ('queued-delete', 4)`,
		// A retry past its backoff, and one still inside it.
		`INSERT INTO resource (resource_id, status, updated_at) VALUES ('retry', 3, now() - interval '1 hour'), ('retry-backoff', 3, now())`,
		// First attempts whose pod stopped (lease released, holder kept):
		// one a fresh loop ran, one a general loop ran.
		`INSERT INTO resource (resource_id, status, claimed_by) VALUES ('interrupted-fresh', 1, 'pod-a#f2'), ('interrupted-general', 1, 'pod-a#3')`,
		// Held by a live fresh loop: nobody else may take it.
		`INSERT INTO resource (resource_id, status, claimed_by, claim_expires_at) VALUES ('held', 1, 'pod-b#f0', now() + interval '1 minute')`,
		`INSERT INTO resource (resource_id, status) VALUES ('stored', 2)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
}

func claimAll(t *testing.T, db *pg.DB, workerID string, freshOnly bool) []string {
	t.Helper()
	var got []string
	for {
		res, err := (&Worker{}).tryClaim(context.Background(), db, workerID, freshOnly)
		if err != nil {
			t.Fatal(err)
		}
		if res == nil {
			sort.Strings(got)
			return got
		}
		got = append(got, res.ID)
	}
}

func TestTryClaimFreshOnly(t *testing.T) {
	db := testDB(t)

	t.Run("general loops take what they always took", func(t *testing.T) {
		seedClaimRows(t, db)
		t.Cleanup(func() { _, _ = db.Exec("TRUNCATE resource CASCADE") })
		got := strings.Join(claimAll(t, db, "pod-c#0", false), " ")
		if want := "interrupted-fresh interrupted-general queued queued-delete retry"; got != want {
			t.Errorf("general claimed %q, want %q", got, want)
		}
	})

	t.Run("fresh loops take only new work, retries are left over", func(t *testing.T) {
		seedClaimRows(t, db)
		t.Cleanup(func() { _, _ = db.Exec("TRUNCATE resource CASCADE") })
		got := strings.Join(claimAll(t, db, "pod-c#f0", true), " ")
		if want := "interrupted-fresh queued queued-delete"; got != want {
			t.Errorf("fresh claimed %q, want %q", got, want)
		}
		got = strings.Join(claimAll(t, db, "pod-c#0", false), " ")
		if want := "interrupted-general retry"; got != want {
			t.Errorf("general claimed after fresh %q, want %q", got, want)
		}
	})
}

// A job cut short by shutdown stays storing, its lease left for Close to
// hand back -- not store_error, which waited out storeErrorBackoff.
func TestProcessClaimedShutdownIsNotAFailure(t *testing.T) {
	db := testDB(t)
	if _, err := db.Exec(`INSERT INTO resource (resource_id, status, claimed_by, claim_expires_at) VALUES ('r', 1, 'pod-a#f0', now() + interval '2 minutes')`); err != nil {
		t.Fatal(err)
	}
	w := &Worker{api: &Api{
		url:            "http://127.0.0.1:1",
		cl:             http.DefaultClient,
		prepareRequest: func(r *http.Request, _ *Claims) (*http.Request, error) { return r, nil },
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w.processClaimed(ctx, db, &Resource{ID: "r", Status: StatusStoring}, "pod-a#f0")

	var res Resource
	if err := db.Model(&res).Where("resource_id = 'r'").Select(); err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusStoring || res.ClaimedBy == nil || *res.ClaimedBy != "pod-a#f0" {
		t.Errorf("interrupted job must stay storing and held, got status=%s claimed_by=%v error=%v", res.Status, res.ClaimedBy, res.Error)
	}
}

// Whatever URL a failure quotes, resource.error -- served by GET /resource
// -- keeps no credential.
func TestProcessClaimedStoresRedactedError(t *testing.T) {
	db := testDB(t)
	if _, err := db.Exec(`INSERT INTO resource (resource_id, status, claimed_by, claim_expires_at) VALUES ('r', 1, 'pod-a#0', now() + interval '2 minutes')`); err != nil {
		t.Fatal(err)
	}
	w := &Worker{api: &Api{
		url:            "http://127.0.0.1:1/?api-key=SECRETKEY123&x=",
		cl:             http.DefaultClient,
		prepareRequest: func(r *http.Request, _ *Claims) (*http.Request, error) { return r, nil },
	}}
	w.processClaimed(context.Background(), db, &Resource{ID: "r", Status: StatusStoring}, "pod-a#0")

	var res Resource
	if err := db.Model(&res).Where("resource_id = 'r'").Select(); err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusStoreError || res.Error == nil || strings.Contains(*res.Error, "SECRETKEY123") || !strings.Contains(*res.Error, "api-key=<redacted>") {
		t.Errorf("stored error must be redacted, got status=%s error=%v", res.Status, res.Error)
	}
	var wl WorkerLog
	if err := db.Model(&wl).Where("resource_id = 'r'").Select(); err != nil {
		t.Fatal(err)
	}
	if wl.ErrorText == nil || strings.Contains(*wl.ErrorText, "SECRETKEY123") || !strings.Contains(*wl.ErrorText, "api-key=<redacted>") {
		t.Errorf("worker log error must be redacted, got %v", wl.ErrorText)
	}
}

// Close hands the pod's leases back at once and keeps the last holder, so
// a fresh loop elsewhere resumes the first attempts its fresh loops ran.
func TestReleasePodLeases(t *testing.T) {
	db := testDB(t)
	if _, err := db.Exec(`INSERT INTO resource (resource_id, status, claimed_by, claim_expires_at) VALUES
		('fresh', 1, 'pod-a#f0', now() + interval '2 minutes'),
		('general', 1, 'pod-a#3', now() + interval '2 minutes'),
		('other-pod', 1, 'pod-b#f1', now() + interval '2 minutes')`); err != nil {
		t.Fatal(err)
	}
	n, err := releasePodLeases(context.Background(), db, "pod-a")
	if err != nil || n != 2 {
		t.Fatalf("released %d (%v), want 2", n, err)
	}
	if got := strings.Join(claimAll(t, db, "pod-c#f0", true), " "); got != "fresh" {
		t.Errorf("fresh claimed %q after pod-a stopped, want \"fresh\"", got)
	}
	if got := strings.Join(claimAll(t, db, "pod-c#0", false), " "); got != "general" {
		t.Errorf("general claimed %q after pod-a stopped, want \"general\" (pod-b's lease is live)", got)
	}
}

// A job a stopped pod cut short goes before retries: it was running, not
// failing.
func TestTryClaimInterruptedBeforeRetries(t *testing.T) {
	db := testDB(t)
	if _, err := db.Exec(`INSERT INTO resource (resource_id, status, updated_at) VALUES ('retry', 3, now() - interval '2 hours')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO resource (resource_id, status, claimed_by) VALUES ('interrupted', 1, 'pod-a#3')`); err != nil {
		t.Fatal(err)
	}
	res, err := (&Worker{}).tryClaim(context.Background(), db, "pod-c#0", false)
	if err != nil || res == nil || res.ID != "interrupted" {
		t.Errorf("first claim = %+v (%v), want the interrupted job", res, err)
	}
}
