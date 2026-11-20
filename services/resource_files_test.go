package services

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func getBody(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code, rec.Body.String()
}

func TestGetResourceFiles(t *testing.T) {
	w, db := testWeb(t)
	for _, q := range []string{
		`INSERT INTO resource (resource_id, status, uids) VALUES ('s', 2, ARRAY['` + uid1 + `','` + uid0 + `']), ('n', 2, NULL)`,
		`INSERT INTO resource (resource_id, status) VALUES ('q0', 0), ('q1', 1), ('q3', 3), ('q4', 4), ('q5', 5), ('q6', 6)`,
		`INSERT INTO file (hash, status, total_size, stored_size) VALUES ('h1', 2, 10, 10), ('h2', 2, 20, 20), ('h3', 1, 30, 0), ('h4', 2, 40, 40)`,
		`INSERT INTO resource_file (resource_id, file_hash, path) VALUES ('s', 'h2', '/N/b'), ('s', 'h4', '/N/a'), ('s', 'h1', '/N/a'), ('s', 'h3', '/N/c'), ('q3', 'h1', '/N/a')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		id   string
		code int
		body string
	}{
		// uids keep their order; /N/a twice → first by file_hash (h1, 10); /N/c's file is not stored; no hashes
		{"s", 200, `{"uids":["` + uid1 + `","` + uid0 + `"],"files":[{"path":"/N/a","size":10},{"path":"/N/b","size":20}]}`},
		{"n", 200, `{"uids":[],"files":[]}`}, // Review Focus 5
		{"q0", 404, ""}, {"q1", 404, ""}, {"q3", 404, ""}, {"q4", 404, ""}, {"q5", 404, ""}, {"q6", 404, ""},
		{"missing", 404, ""},
	} {
		code, body := getBody(t, w.router(), "/resource/"+tc.id+"/files")
		if code != tc.code || body != tc.body {
			t.Errorf("%s: %d %s, want %d %s", tc.id, code, body, tc.code, tc.body)
		}
	}
}
