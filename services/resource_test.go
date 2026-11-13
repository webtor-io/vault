package services

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/go-pg/pg/v10"
	cs "github.com/webtor-io/common-services"
)

// testWeb is a Web on the test Postgres (see testDB).
func testWeb(t *testing.T) (*Web, *pg.DB) {
	t.Helper()
	db := testDB(t)
	gin.SetMode(gin.TestMode)
	w := &Web{pg: cs.NewPG(testPGContext(t))}
	t.Cleanup(w.pg.Close)
	return w, db
}

func put(t *testing.T, h http.Handler, path, body string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, path, strings.NewReader(body)))
	return rec.Code
}

func TestPutUIDsKeepsStatus(t *testing.T) {
	w, db := testWeb(t)
	for st := StatusQueuedForStoring; st <= StatusDeleteError; st++ {
		id := "r" + strconv.Itoa(int(st))
		if _, err := db.Exec(`INSERT INTO resource (resource_id, status) VALUES (?, ?)`, id, st); err != nil {
			t.Fatal(err)
		}
		if code := put(t, w.router(), "/resource/"+id+"/uids", `{"uids":["`+uid1+`","`+uid0+`"]}`); code != http.StatusOK {
			t.Fatalf("%s: status %d", st, code)
		}
		r, err := ResourceGetByID(context.Background(), db, id)
		if err != nil || r.Status != st || !slices.Equal(r.UIDs, []string{uid1, uid0}) {
			t.Fatalf("%s: got %+v (%v), want status kept and uids [uid1 uid0] in order", st, r, err)
		}
	}
}

func TestPutUIDsMissingIs404(t *testing.T) {
	w, db := testWeb(t)
	if code := put(t, w.router(), "/resource/missing/uids", `{"uids":["`+uid0+`"]}`); code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", code)
	}
	if n, _ := db.Model((*Resource)(nil)).Where("resource_id = 'missing'").Count(); n != 0 {
		t.Fatal("PUT /uids created a resource")
	}
}

func TestPutUIDsRejectsMalformed(t *testing.T) {
	w, db := testWeb(t)
	if _, err := db.Exec(`INSERT INTO resource (resource_id, status, uids) VALUES ('r', 2, ARRAY[?])`, uid0); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		``, `not json`,
		`{"uids":["` + strings.ToUpper(strings.Repeat("a", 40)) + `"]}`,
		`{"uids":["` + strings.Repeat("0", 39) + `"]}`,
		`{"uids":["123e4567-e89b-12d3-a456-426614174000"]}`,
	} {
		if code := put(t, w.router(), "/resource/r/uids", body); code != http.StatusBadRequest {
			t.Fatalf("body %q: status %d, want 400", body, code)
		}
	}
	if r, _ := ResourceGetByID(context.Background(), db, "r"); !slices.Equal(r.UIDs, []string{uid0}) {
		t.Fatalf("uids changed by a rejected body: %v", r.UIDs)
	}
}

func TestPutUIDsEmptyListClears(t *testing.T) {
	w, db := testWeb(t)
	for i, body := range []string{`{"uids":[]}`, `{"uids":null}`, `{}`} {
		id := "c" + strconv.Itoa(i)
		if _, err := db.Exec(`INSERT INTO resource (resource_id, status, uids) VALUES (?, 2, ARRAY[?])`, id, uid0); err != nil {
			t.Fatal(err)
		}
		if code := put(t, w.router(), "/resource/"+id+"/uids", body); code != http.StatusOK {
			t.Fatalf("body %s: status %d, want 200", body, code)
		}
		r, err := ResourceGetByID(context.Background(), db, id)
		if err != nil || r.Status != StatusStored || len(r.UIDs) != 0 {
			t.Fatalf("body %s: got %+v (%v), want stored with no uids", body, r, err)
		}
	}
}

// The body of PUT /resource writes uids in every status branch, and only a
// non-empty list does: an old web-ui sends no body and must not wipe them.
func TestPutResourceBody(t *testing.T) {
	w, db := testWeb(t)
	if _, err := db.Exec(`INSERT INTO resource (resource_id, status, uids) VALUES ('s', 2, ARRAY[?]), ('e', 3, NULL), ('o', 3, ARRAY[?]), ('p', 2, ARRAY[?])`, uid0, uid0, uid0); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, id, body string
		status         Status
		uids           []string
	}{
		{"new", "n", `{"uids":["` + uid0 + `"]}`, StatusQueuedForStoring, []string{uid0}},
		{"stored", "s", `{"uids":["` + uid1 + `"]}`, StatusStored, []string{uid1}},
		{"error", "e", `{"uids":["` + uid1 + `"]}`, StatusQueuedForStoring, []string{uid1}},
		{"no body", "o", ``, StatusQueuedForStoring, []string{uid0}},
		{"no body stored", "p", ``, StatusStored, []string{uid0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code := put(t, w.router(), "/resource/"+tc.id, tc.body); code != http.StatusAccepted {
				t.Fatalf("status %d, want 202", code)
			}
			r, err := ResourceGetByID(context.Background(), db, tc.id)
			if err != nil || r == nil || r.Status != tc.status || !slices.Equal(r.UIDs, tc.uids) {
				t.Fatalf("got %+v (%v), want %s with uids %v", r, err, tc.status, tc.uids)
			}
		})
	}
	t.Run("bad body", func(t *testing.T) {
		if code := put(t, w.router(), "/resource/b", `{"uids":["x"]}`); code != http.StatusBadRequest {
			t.Fatalf("status %d, want 400", code)
		}
		if r, err := ResourceGetByID(context.Background(), db, "b"); err != nil || r != nil {
			t.Fatalf("got %+v (%v), want no row", r, err)
		}
	})
}
