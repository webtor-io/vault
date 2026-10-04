package services

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/json"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/go-pg/pg/v10"
	"github.com/urfave/cli"
	cs "github.com/webtor-io/common-services"
)

// These run handleStore end to end against a real Postgres (VAULT_TEST_PG_DSN,
// see testDB), an in-memory S3 and a fake rest-api/torrent source.

const mib = 1 << 20

// ---- in-memory S3 (path style, the subset vault uses) ----

type fakeS3 struct {
	mu      sync.Mutex
	failGet map[string]bool // GETs of these keys answer 500
	objects map[string][]byte
	uploads map[string]map[int64][]byte
	n       int
}

func newFakeS3() *fakeS3 {
	return &fakeS3{objects: map[string][]byte{}, uploads: map[string]map[int64][]byte{}}
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)[1]
	q := r.URL.Query()
	id := q.Get("uploadId")
	switch {
	case r.Method == http.MethodPost && q.Has("uploads"):
		f.n++
		id = "u" + strconv.Itoa(f.n)
		f.uploads[id] = map[int64][]byte{}
		fmt.Fprintf(w, `<InitiateMultipartUploadResult><Bucket>b</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>`, key, id)
	case id != "" && f.uploads[id] == nil:
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `<Error><Code>NoSuchUpload</Code><Message>no upload</Message></Error>`)
	case r.Method == http.MethodPut && id != "":
		n, _ := strconv.ParseInt(q.Get("partNumber"), 10, 64)
		body, _ := io.ReadAll(r.Body)
		f.uploads[id][n] = body
		w.Header().Set("ETag", fmt.Sprintf(`"%x"`, sha1.Sum(body)))
	case r.Method == http.MethodGet && id != "":
		var nums []int64
		for n := range f.uploads[id] {
			nums = append(nums, n)
		}
		sort.Slice(nums, func(i, j int) bool { return nums[i] < nums[j] })
		var b strings.Builder
		fmt.Fprintf(&b, `<ListPartsResult><Bucket>b</Bucket><Key>%s</Key><UploadId>%s</UploadId><IsTruncated>false</IsTruncated>`, key, id)
		for _, n := range nums {
			fmt.Fprintf(&b, `<Part><PartNumber>%d</PartNumber><ETag>"%x"</ETag><Size>%d</Size></Part>`, n, sha1.Sum(f.uploads[id][n]), len(f.uploads[id][n]))
		}
		b.WriteString(`</ListPartsResult>`)
		fmt.Fprint(w, b.String())
	case r.Method == http.MethodPost && id != "":
		var req struct {
			Parts []struct{ PartNumber int64 } `xml:"Part"`
		}
		_ = xml.NewDecoder(r.Body).Decode(&req)
		var obj []byte
		for _, p := range req.Parts {
			obj = append(obj, f.uploads[id][p.PartNumber]...)
		}
		f.objects[key] = obj
		delete(f.uploads, id)
		fmt.Fprintf(w, `<CompleteMultipartUploadResult><Bucket>b</Bucket><Key>%s</Key><ETag>"x"</ETag></CompleteMultipartUploadResult>`, key)
	case r.Method == http.MethodDelete && id != "":
		delete(f.uploads, id)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		f.objects[key] = body
		w.Header().Set("ETag", `"x"`)
	case r.Method == http.MethodDelete:
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && f.failGet[key]:
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `<Error><Code>InternalError</Code><Message>try again</Message></Error>`)
	case r.Method == http.MethodHead || r.Method == http.MethodGet:
		obj, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		start, end := int64(0), int64(len(obj))-1
		if rg := r.Header.Get("Range"); rg != "" {
			fmt.Sscanf(rg, "bytes=%d-%d", &start, &end)
			end = min(end, int64(len(obj))-1)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(obj)))
		}
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		if r.Header.Get("Range") != "" {
			w.WriteHeader(http.StatusPartialContent)
		}
		if r.Method == http.MethodGet {
			_, _ = w.Write(obj[start : end+1])
		}
	default:
		w.WriteHeader(http.StatusNotImplemented)
	}
}

func (f *fakeS3) object(key string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.objects[key]
}

// ---- fake rest-api + torrent-http-proxy ----

type fakeTorrent struct {
	infohash string
	info     *metainfo.Info
	files    []testFile // real files, in torrent order
	// mutate may change the bytes served for one GET of a file; streaming
	// (open-ended) requests have end == -1.
	mutate func(name string, start, end int64, data []byte)
	// fail answers one GET of a file with 500 when it returns true.
	fail  func(name string, start, end int64) bool
	mu    sync.Mutex
	opens []string // "name@start" of every open-ended (streaming) request
}

func (ft *fakeTorrent) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	switch {
	case p[0] == "resource" && len(p) == 3 && p[2] == "list":
		var items []map[string]any
		for _, f := range ft.files {
			path := "/t/" + f.name
			if len(ft.info.Files) == 0 {
				path = "/" + ft.info.Name // single-file torrent
			}
			items = append(items, map[string]any{"id": f.name, "path": path, "type": "file", "size": len(f.bytes)})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
	case p[0] == "resource" && len(p) == 4 && p[2] == "export":
		u := "http://" + r.Host + "/" + ft.infohash + "/" + p[3] + "?token=t"
		_ = json.NewEncoder(w).Encode(map[string]any{"exports": map[string]any{"download": map[string]any{"url": u}}})
	case len(p) == 2 && p[1] == "source.torrent":
		mi := metainfo.MetaInfo{InfoBytes: bencode.MustMarshal(ft.info)}
		_ = mi.Write(w)
	case len(p) == 2:
		name, _ := url.PathUnescape(p[1])
		var data []byte
		for _, f := range ft.files {
			if f.name == name {
				data = f.bytes
			}
		}
		start, end := int64(0), int64(-1)
		if rg := r.Header.Get("Range"); rg != "" {
			parts := strings.SplitN(strings.TrimPrefix(rg, "bytes="), "-", 2)
			start, _ = strconv.ParseInt(parts[0], 10, 64)
			if parts[1] != "" {
				end, _ = strconv.ParseInt(parts[1], 10, 64)
			}
		}
		hi := int64(len(data)) - 1
		if end >= 0 {
			hi = min(end, hi)
		}
		out := append([]byte(nil), data[start:hi+1]...)
		ft.mu.Lock()
		if ft.fail != nil && ft.fail(name, start, end) {
			ft.mu.Unlock()
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if end < 0 {
			ft.opens = append(ft.opens, fmt.Sprintf("%s@%d", name, start))
		}
		if ft.mutate != nil {
			ft.mutate(name, start, end, out)
		}
		ft.mu.Unlock()
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(out)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// ---- harness ----

type storeEnv struct {
	t   *testing.T
	db  *pg.DB
	s3  *fakeS3
	src *fakeTorrent
	w   *Worker
}

func newStoreEnv(t *testing.T, info *metainfo.Info, files []testFile) *storeEnv {
	t.Helper()
	db := testDB(t)
	s3srv := newFakeS3()
	s3hs := httptest.NewServer(s3srv)
	t.Cleanup(s3hs.Close)
	ft := &fakeTorrent{infohash: "ih", info: info, files: files}
	apihs := httptest.NewServer(ft)
	t.Cleanup(apihs.Close)

	dsn, _ := url.Parse(os.Getenv("VAULT_TEST_PG_DSN"))
	pass, _ := dsn.User.Password()
	app := cli.NewApp()
	app.Flags = cs.RegisterS3ClientFlags(cs.RegisterPGFlags(nil))
	set := flag.NewFlagSet("t", flag.ContinueOnError)
	for _, f := range app.Flags {
		f.Apply(set)
	}
	if err := set.Parse([]string{
		"--postgres-host=" + dsn.Hostname(), "--postgres-port=" + dsn.Port(),
		"--postgres-user=" + dsn.User.Username(), "--postgres-password=" + pass,
		"--postgres-database=" + strings.TrimPrefix(dsn.Path, "/"),
		"--aws-access-key-id=k", "--aws-secret-access-key=s", "--aws-region=de",
		"--aws-endpoint=" + s3hs.URL, "--aws-no-ssl",
	}); err != nil {
		t.Fatal(err)
	}
	c := cli.NewContext(app, set, nil)
	w := &Worker{
		pg:              cs.NewPG(c),
		s3:              cs.NewS3Client(c, http.DefaultClient),
		api:             &Api{url: apihs.URL, cl: http.DefaultClient, prepareRequest: func(r *http.Request, _ *Claims) (*http.Request, error) { return r, nil }},
		bucket:          "vault",
		part:            5 * mib,
		concur:          4,
		verifyIntegrity: true,
	}
	t.Cleanup(func() { w.pg.Close() })
	if _, err := db.Exec(`INSERT INTO resource (resource_id, status) VALUES ('ih', 1)`); err != nil {
		t.Fatal(err)
	}
	return &storeEnv{t: t, db: db, s3: s3srv, src: ft, w: w}
}

func (e *storeEnv) store() error { return e.w.handleStore(context.Background(), e.db, "ih") }

// contentKey is the S3 key vault derives for a file (generateFileHash).
func contentKey(data []byte) string {
	h := sha256.New()
	h.Write([]byte(strconv.Itoa(len(data))))
	if len(data) < 1000*1024 {
		h.Write(data)
	} else {
		h.Write(data[:500*1024+1]) // Range "bytes=0-512000" is inclusive
		h.Write(data[len(data)-500*1024:])
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// assertStored checks every file's object holds exactly the torrent's bytes.
func (e *storeEnv) assertStored(files []testFile) {
	e.t.Helper()
	for _, f := range files {
		got := e.s3.object(contentKey(f.bytes))
		if !bytes.Equal(got, f.bytes) {
			e.t.Errorf("%s: stored %d bytes, equal=%v; want the torrent's %d bytes", f.name, len(got), bytes.Equal(got, f.bytes), len(f.bytes))
		}
	}
}

// pattern returns n deterministic non-repeating bytes, so a misplaced part shows.
func pattern(seed byte, n int) []byte {
	out := make([]byte, n)
	x := uint32(seed) + 1
	for i := range out {
		x = x*1664525 + 1013904223
		out[i] = byte(x >> 24)
	}
	return out
}

func twoFiles() []testFile {
	return []testFile{{"a", pattern(1, 12*mib+345)}, {"b", pattern(2, 7*mib+11)}}
}

// ---- scenarios ----

// A hybrid torrent's padded file tails hash with their BEP 47 zeros; before,
// every padded file failed verification on every retry.
func TestStore_HybridPaddedFiles(t *testing.T) {
	files := twoFiles()
	info, _, _ := buildHybridTorrent(t, mib, files)
	e := newStoreEnv(t, info, files)
	if err := e.store(); err != nil {
		t.Fatalf("store: %v", err)
	}
	e.assertStored(files)
}

// A transient bad read in a file's tail (its right-boundary piece) is caught
// before the file is stored and re-read from the part holding it, keeping the
// parts before; before, the tail was not verified at all.
func TestStore_BadTailCaughtAndReReadInPlace(t *testing.T) {
	files := twoFiles()
	info, _, _ := buildTestTorrent(t, mib, files)
	e := newStoreEnv(t, info, files)
	bad := int64(12*mib + 100) // inside a's last, boundary-crossing piece
	served := false
	e.src.mutate = func(name string, start, end int64, data []byte) {
		// The main stream only: generateFileHash also opens a's last 500 KiB.
		if name == "a" && end < 0 && !served && start <= bad && start < int64(len(files[0].bytes))-500*1024 {
			served = true
			data[bad-start] ^= 0xFF
		}
	}
	if err := e.store(); err != nil {
		t.Fatalf("store: %v", err)
	}
	e.assertStored(files)
	// a is 12 MiB in 5 MiB parts: [0,5) and the merged last part [5,12.x).
	if got := strings.Join(e.src.opens, " "); !strings.Contains(got, "a@0 a@5242880 ") {
		t.Errorf("streams %q: want a re-read from the last part at 5 MiB, not from 0", got)
	}
}

// An interrupted upload left parts 1, 2 and 4: parts upload in parallel. The
// resume must refill part 3; before, it resumed at part 4's offset as part 5.
func TestStore_ResumeAfterGapInParts(t *testing.T) {
	files := []testFile{{"a", pattern(3, 22*mib+7)}}
	info, _, _ := buildTestTorrent(t, mib, files)
	e := newStoreEnv(t, info, files)
	a := files[0].bytes
	key := contentKey(a)
	e.s3.uploads["u-old"] = map[int64][]byte{1: a[:5*mib], 2: a[5*mib : 10*mib], 4: a[15*mib : 20*mib]}
	if _, err := e.db.Exec(`INSERT INTO file (hash, status, upload_id, part_size, total_size) VALUES (?, 1, 'u-old', ?, ?)`, key, 5*mib, len(a)); err != nil {
		t.Fatal(err)
	}
	if err := e.store(); err != nil {
		t.Fatalf("store: %v", err)
	}
	e.assertStored(files)
}

// A previous file stored with a corrupt tail (before right-boundary checks)
// is found by the next file's left-boundary piece and sent back to storing;
// the next store uploads it again. Before, the next file failed forever.
func TestStore_CorruptStoredTailIsReStored(t *testing.T) {
	files := twoFiles()
	info, _, _ := buildTestTorrent(t, mib, files)
	e := newStoreEnv(t, info, files)
	a := files[0].bytes
	corrupt := append([]byte(nil), a...)
	copy(corrupt[len(a)-50:], make([]byte, 50)) // zeros in a's stored tail
	key := contentKey(a)
	e.s3.objects[key] = corrupt
	for _, q := range []string{
		`INSERT INTO file (hash, status, total_size, stored_size) VALUES ('` + key + `', 2, ` + strconv.Itoa(len(a)) + `, ` + strconv.Itoa(len(a)) + `)`,
		`INSERT INTO resource_file (resource_id, file_hash, path) VALUES ('ih', '` + key + `', '/t/a')`,
	} {
		if _, err := e.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	err := e.store()
	if err == nil || !strings.Contains(err.Error(), "queued for re-store") {
		t.Fatalf("first store: want the corrupt file queued for re-store, got %v", err)
	}
	if err := e.store(); err != nil {
		t.Fatalf("second store: %v", err)
	}
	e.assertStored(files)
}

// Another resource's file with the same path and size but other bytes is not
// reused; before, path+size alone linked it.
func TestStore_PathSizeDedupChecksContent(t *testing.T) {
	files := []testFile{{"a", pattern(4, 6*mib+3)}}
	info, _, _ := buildTestTorrent(t, mib, files)
	e := newStoreEnv(t, info, files)
	other := pattern(5, len(files[0].bytes))
	for _, q := range []string{
		`INSERT INTO resource (resource_id, status) VALUES ('other-resource', 2)`,
		`INSERT INTO file (hash, status, total_size, stored_size) VALUES ('other', 2, ` + strconv.Itoa(len(other)) + `, ` + strconv.Itoa(len(other)) + `)`,
		`INSERT INTO resource_file (resource_id, file_hash, path) VALUES ('other-resource', 'other', '/t')`, // single-file path
	} {
		if _, err := e.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	e.s3.objects["other"] = other
	if err := e.store(); err != nil {
		t.Fatalf("store: %v", err)
	}
	e.assertStored(files)
	var hashes []string
	if _, err := e.db.Query(&hashes, `SELECT file_hash FROM resource_file WHERE resource_id = 'ih'`); err != nil {
		t.Fatal(err)
	}
	if len(hashes) != 1 || hashes[0] == "other" {
		t.Errorf("resource linked to %v, want its own file", hashes)
	}
}

// A stored prefix that cuts a piece holds that piece's start unverified (the
// stream stopped before the piece completed). Resuming rewinds to the part
// holding the piece start and uploads it again; resuming at the cut, with the
// prefix seeded from the source, would keep a bad byte there.
func TestStore_ResumeReUploadsTheCutPiecesPart(t *testing.T) {
	files := []testFile{{"a", pattern(6, 22*mib+7)}}
	info, _, _ := buildTestTorrent(t, 3*mib, files) // piece 3 = [9,12) MiB, cut at 10 MiB
	e := newStoreEnv(t, info, files)
	a := files[0].bytes
	part2 := append([]byte(nil), a[5*mib:10*mib]...)
	part2[9*mib+512*1024-5*mib] ^= 0xFF // in piece 3's prefix, inside part 2
	e.s3.uploads["u-old"] = map[int64][]byte{1: a[:5*mib], 2: part2}
	if _, err := e.db.Exec(`INSERT INTO file (hash, status, upload_id, part_size, total_size) VALUES (?, 1, 'u-old', ?, ?)`, contentKey(a), 5*mib, len(a)); err != nil {
		t.Fatal(err)
	}
	if err := e.store(); err != nil {
		t.Fatalf("store: %v", err)
	}
	e.assertStored(files)
}

// seedStored stores files as an already-stored resource: rows, links, objects.
func (e *storeEnv) seedStored(files []testFile, objects map[string][]byte) {
	e.t.Helper()
	if _, err := e.db.Exec(`UPDATE resource SET status = 2 WHERE resource_id = 'ih'`); err != nil {
		e.t.Fatal(err)
	}
	for _, f := range files {
		key := contentKey(f.bytes)
		obj, ok := objects[f.name]
		if !ok {
			obj = f.bytes
		}
		e.s3.objects[key] = obj
		if _, err := e.db.Exec(`INSERT INTO file (hash, status, total_size, stored_size) VALUES (?, 2, ?, ?)`, key, len(f.bytes), len(f.bytes)); err != nil {
			e.t.Fatal(err)
		}
		if _, err := e.db.Exec(`INSERT INTO resource_file (resource_id, file_hash, path) VALUES ('ih', ?, ?)`, key, "/t/"+f.name); err != nil {
			e.t.Fatal(err)
		}
	}
}

func (e *storeEnv) verifyExisting(boundariesOnly bool) VerifyExistingStats {
	e.t.Helper()
	stats, err := RunVerifyExisting(context.Background(), e.w.pg, e.w.s3, e.w.api, "vault",
		VerifyExistingOptions{ResourceID: "ih", BoundariesOnly: boundariesOnly})
	if err != nil {
		e.t.Fatal(err)
	}
	return stats
}

func (e *storeEnv) fileRows() map[string]bool {
	var hashes []string
	if _, err := e.db.Query(&hashes, `SELECT hash FROM file`); err != nil {
		e.t.Fatal(err)
	}
	out := map[string]bool{}
	for _, h := range hashes {
		out[h] = true
	}
	return out
}

// A stored file whose tail is corrupt (its boundary piece was never verified)
// is found by verify-existing, blamed alone, and its resource re-queued.
func TestVerifyExisting_CorruptBoundaryTail(t *testing.T) {
	for _, boundariesOnly := range []bool{true, false} {
		t.Run(fmt.Sprintf("boundariesOnly=%v", boundariesOnly), func(t *testing.T) {
			files := twoFiles()
			info, _, _ := buildTestTorrent(t, mib, files)
			e := newStoreEnv(t, info, files)
			bad := append([]byte(nil), files[0].bytes...)
			copy(bad[len(bad)-50:], make([]byte, 50))
			e.seedStored(files, map[string][]byte{"a": bad})
			stats := e.verifyExisting(boundariesOnly)
			rows := e.fileRows()
			if stats.BadFiles != 1 || rows[contentKey(files[0].bytes)] || !rows[contentKey(files[1].bytes)] {
				t.Fatalf("bad_files=%d rows=%v: want a invalidated, b kept", stats.BadFiles, rows)
			}
			var status Status
			if _, err := e.db.QueryOne(pg.Scan(&status), `SELECT status FROM resource WHERE resource_id = 'ih'`); err != nil || status != StatusQueuedForStoring {
				t.Errorf("resource status %v (%v), want queued for storing", status, err)
			}
		})
	}
}

func TestVerifyExisting_CleanBoundariesUntouched(t *testing.T) {
	files := twoFiles()
	info, _, _ := buildTestTorrent(t, mib, files)
	e := newStoreEnv(t, info, files)
	e.seedStored(files, nil)
	if stats := e.verifyExisting(true); stats.BadFiles != 0 || stats.Clean != 1 || stats.BoundaryPieces == 0 {
		t.Fatalf("stats %+v: want clean with boundary pieces checked", stats)
	}
	if len(e.fileRows()) != 2 {
		t.Fatal("clean files were removed")
	}
}

// A hybrid's padded tail is checked with its padding zeros.
func TestVerifyExisting_HybridPaddedTail(t *testing.T) {
	files := twoFiles()
	info, _, _ := buildHybridTorrent(t, mib, files)
	e := newStoreEnv(t, info, files)
	bad := append([]byte(nil), files[0].bytes...)
	bad[len(bad)-1] ^= 0xFF
	e.seedStored(files, map[string][]byte{"a": bad})
	if stats := e.verifyExisting(true); stats.BadFiles != 1 {
		t.Fatalf("stats %+v: want the padded tail caught", stats)
	}
}

// With verification off, an upload whose parts are all there (the last one
// merged) is completed as is; before, it resumed short of the end and stored
// the tail twice.
func TestStore_CompletePartsWithoutVerification(t *testing.T) {
	files := []testFile{{"a", pattern(7, 12*mib+345)}}
	info, _, _ := buildTestTorrent(t, mib, files)
	e := newStoreEnv(t, info, files)
	e.w.verifyIntegrity = false
	a := files[0].bytes
	e.s3.uploads["u-old"] = map[int64][]byte{1: a[:5*mib], 2: a[5*mib:]}
	if _, err := e.db.Exec(`INSERT INTO file (hash, status, upload_id, part_size, total_size) VALUES (?, 1, 'u-old', ?, ?)`, contentKey(a), 5*mib, len(a)); err != nil {
		t.Fatal(err)
	}
	if err := e.store(); err != nil {
		t.Fatalf("store: %v", err)
	}
	e.assertStored(files)
}

// A path+size candidate whose object is gone is not a match; before, the
// failed read failed every store of the resource.
func TestStore_DedupCandidateMissingInS3(t *testing.T) {
	files := []testFile{{"a", pattern(8, 6*mib+3)}}
	info, _, _ := buildTestTorrent(t, mib, files)
	e := newStoreEnv(t, info, files)
	for _, q := range []string{
		`INSERT INTO resource (resource_id, status) VALUES ('other-resource', 2)`,
		`INSERT INTO file (hash, status, total_size, stored_size) VALUES ('gone', 2, ` + strconv.Itoa(len(files[0].bytes)) + `, ` + strconv.Itoa(len(files[0].bytes)) + `)`,
		`INSERT INTO resource_file (resource_id, file_hash, path) VALUES ('other-resource', 'gone', '/t')`,
	} {
		if _, err := e.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.store(); err != nil {
		t.Fatalf("store: %v", err)
	}
	e.assertStored(files)
}

// A source hiccup while completing a boundary piece fails the attempt but keeps
// the uploaded parts; the retry resumes instead of starting over.
func TestStore_SourceErrorKeepsParts(t *testing.T) {
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
	e.s3.mu.Lock()
	kept := 0
	for _, parts := range e.s3.uploads {
		kept += len(parts)
	}
	e.s3.mu.Unlock()
	if kept == 0 {
		t.Fatal("the failed attempt aborted its multipart upload")
	}
	if err := e.store(); err != nil {
		t.Fatalf("retry: %v", err)
	}
	e.assertStored(files)
	if got := strings.Join(e.src.opens, " "); strings.Count(got, "a@0") != 1 {
		t.Errorf("streams %q: the retry restarted a from 0", got)
	}
}

// When the torrent's own bytes do not hash right either, nothing is blamed:
// deleting a stored copy the source cannot replace would only lose data.
func TestVerifyExisting_UnreliableSourceBlamesNothing(t *testing.T) {
	files := twoFiles()
	info, _, _ := buildTestTorrent(t, mib, files)
	e := newStoreEnv(t, info, files)
	bad := append([]byte(nil), files[0].bytes...)
	copy(bad[len(bad)-50:], make([]byte, 50))
	e.seedStored(files, map[string][]byte{"a": bad})
	e.src.mutate = func(name string, start, end int64, data []byte) {
		if name == "a" && end >= 0 {
			data[len(data)-1] ^= 0x55 // the source's tail is wrong too, differently
		}
	}
	stats := e.verifyExisting(true)
	if stats.BadFiles != 0 || stats.Unresolved != 1 || len(e.fileRows()) != 2 {
		t.Fatalf("stats %+v rows %d: want unresolved, nothing invalidated", stats, len(e.fileRows()))
	}
}

// Independent transient bad reads on different pieces each get their re-reads;
// the limit is per piece. Before, the third one in a big file ended the attempt.
func TestStore_RewindLimitIsPerPiece(t *testing.T) {
	files := []testFile{{"a", pattern(9, 22*mib+7)}}
	info, _, _ := buildTestTorrent(t, mib, files)
	e := newStoreEnv(t, info, files)
	bad := []int64{3*mib + 10, 8*mib + 10, 13*mib + 10}
	n := 0
	e.src.mutate = func(name string, start, end int64, data []byte) {
		if end >= 0 || start >= int64(len(files[0].bytes))-500*1024 || n >= len(bad) {
			return // only the main stream, once per bad offset
		}
		if b := bad[n]; start <= b && b-start < int64(len(data)) {
			data[b-start] ^= 0xFF
			n++
		}
	}
	if err := e.store(); err != nil {
		t.Fatalf("store: %v", err)
	}
	e.assertStored(files)
	if n != len(bad) {
		t.Fatalf("injected %d bad reads, want %d", n, len(bad))
	}
}

// An S3 read error is not corruption: the object stays. Before, any error from
// the interior check deleted the object and re-queued its resources.
func TestVerifyExisting_ReadErrorIsNotCorruption(t *testing.T) {
	files := twoFiles()
	info, _, _ := buildTestTorrent(t, mib, files)
	e := newStoreEnv(t, info, files)
	e.seedStored(files, nil)
	e.s3.failGet = map[string]bool{contentKey(files[0].bytes): true}
	stats := e.verifyExisting(false)
	if stats.BadFiles != 0 || stats.Errors == 0 || len(e.fileRows()) != 2 {
		t.Fatalf("stats %+v rows %d: want an error counted and nothing invalidated", stats, len(e.fileRows()))
	}
}

// A pure v2 torrent has no v1 piece hashes to verify against; it is stored
// unverified (as before verification existed) instead of failing every retry.
func TestStore_PureV2StoredUnverified(t *testing.T) {
	data := pattern(10, 6*mib+5)
	info := &metainfo.Info{Name: "t", PieceLength: mib, MetaVersion: 2,
		FileTree: metainfo.FileTree{File: metainfo.FileTreeFile{Length: int64(len(data)), PiecesRoot: string(make([]byte, 32))}}}
	files := []testFile{{"t", data}}
	e := newStoreEnv(t, info, files)
	if err := e.store(); err != nil {
		t.Fatalf("store: %v", err)
	}
	e.assertStored(files)
}
