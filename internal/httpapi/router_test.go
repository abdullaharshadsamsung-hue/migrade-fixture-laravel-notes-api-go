package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

func newTestRouter(t *testing.T) (http.Handler, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "notes.json")
	t.Setenv("NOTES_FILE", path)
	r := chi.NewRouter()
	RegisterRoutes(r)
	return r, path
}

func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeMap(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return m
}

func countNotes(t *testing.T, h http.Handler) int {
	t.Helper()
	rec := do(t, h, http.MethodGet, "/notes", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("index status %d", rec.Code)
	}
	var list []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	return len(list)
}

func TestHealth(t *testing.T) {
	h, _ := newTestRouter(t)
	for _, p := range []string{"/health", "/api/health"} {
		t.Run(p, func(t *testing.T) {
			rec := do(t, h, http.MethodGet, p, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d", rec.Code)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
				t.Fatalf("content-type %q", ct)
			}
			if m := decodeMap(t, rec); m["status"] != "ok" {
				t.Fatalf("body %v", m)
			}
		})
	}
}

func TestStore(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantCode  int
		wantCount int
		wantTitle string
		wantBody  string
	}{
		{"missing title", `{"body":"no title"}`, http.StatusBadRequest, 0, "", ""},
		{"empty title", `{"title":"   "}`, http.StatusBadRequest, 0, "", ""},
		{"no body", ``, http.StatusBadRequest, 0, "", ""},
		{"valid", `{"title":"Hello","body":"World"}`, http.StatusCreated, 1, "Hello", "World"},
		{"valid no body", `{"title":"Only"}`, http.StatusCreated, 1, "Only", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newTestRouter(t)
			rec := do(t, h, http.MethodPost, "/notes", tc.body)
			if rec.Code != tc.wantCode {
				t.Fatalf("status %d want %d body %s", rec.Code, tc.wantCode, rec.Body.String())
			}
			m := decodeMap(t, rec)
			if tc.wantCode == http.StatusBadRequest {
				if _, ok := m["error"]; !ok {
					t.Fatalf("expected error key: %v", m)
				}
			} else {
				if m["title"] != tc.wantTitle || m["body"] != tc.wantBody {
					t.Fatalf("got %v", m)
				}
				if m["id"].(float64) != 1 {
					t.Fatalf("id %v", m["id"])
				}
			}
			if n := countNotes(t, h); n != tc.wantCount {
				t.Fatalf("count %d want %d", n, tc.wantCount)
			}
		})
	}
}

func TestIndexAndShow(t *testing.T) {
	h, path := newTestRouter(t)
	if n := countNotes(t, h); n != 0 {
		t.Fatalf("expected empty, got %d", n)
	}
	do(t, h, http.MethodPost, "/notes", `{"title":"a","body":"1"}`)
	do(t, h, http.MethodPost, "/notes", `{"title":"b","body":"2"}`)
	before, _ := os.ReadFile(path)
	if n := countNotes(t, h); n != 2 {
		t.Fatalf("count %d", n)
	}
	tests := []struct {
		name, path string
		code       int
		title      string
	}{
		{"exists", "/notes/1", 200, "a"},
		{"exists api", "/api/notes/2", 200, "b"},
		{"missing", "/notes/99", 404, ""},
		{"non-numeric", "/notes/abc", 404, ""},
		{"overflow", "/notes/99999999999999999999999", 404, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, h, http.MethodGet, tc.path, "")
			if rec.Code != tc.code {
				t.Fatalf("status %d want %d", rec.Code, tc.code)
			}
			if tc.title != "" {
				if m := decodeMap(t, rec); m["title"] != tc.title {
					t.Fatalf("got %v", m)
				}
			}
		})
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatalf("read-only endpoints modified store")
	}
}

func TestUpdate(t *testing.T) {
	tests := []struct {
		name, path, body string
		code             int
		title, nbody     string
	}{
		{"valid", "/notes/1", `{"title":"new","body":"nb"}`, 200, "new", "nb"},
		{"partial", "/notes/1", `{"body":"only"}`, 200, "orig", "only"},
		{"empty title", "/notes/1", `{"title":""}`, 200, "orig", "ob"},
		{"non-string title", "/notes/1", `{"title":5}`, 400, "orig", "ob"},
		{"missing", "/notes/42", `{"title":"x"}`, 404, "orig", "ob"},
		{"non-numeric", "/notes/xyz", `{"title":"x"}`, 404, "orig", "ob"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newTestRouter(t)
			do(t, h, http.MethodPost, "/notes", `{"title":"orig","body":"ob"}`)
			rec := do(t, h, http.MethodPut, tc.path, tc.body)
			if rec.Code != tc.code {
				t.Fatalf("status %d want %d body %s", rec.Code, tc.code, rec.Body.String())
			}
			m := decodeMap(t, do(t, h, http.MethodGet, "/notes/1", ""))
			if m["title"] != tc.title || m["body"] != tc.nbody {
				t.Fatalf("stored %v", m)
			}
		})
	}
}

func TestDestroy(t *testing.T) {
	tests := []struct {
		name, path string
		code, left int
	}{
		{"existing", "/notes/1", 204, 0},
		{"missing", "/notes/7", 404, 1},
		{"non-numeric", "/notes/abc", 404, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newTestRouter(t)
			do(t, h, http.MethodPost, "/notes", `{"title":"t"}`)
			rec := do(t, h, http.MethodDelete, tc.path, "")
			if rec.Code != tc.code {
				t.Fatalf("status %d want %d", rec.Code, tc.code)
			}
			if tc.code == 204 && rec.Body.Len() != 0 {
				t.Fatalf("204 with body %q", rec.Body.String())
			}
			if n := countNotes(t, h); n != tc.left {
				t.Fatalf("left %d want %d", n, tc.left)
			}
		})
	}
}

func TestMethodNotAllowed(t *testing.T) {
	h, _ := newTestRouter(t)
	tests := []struct {
		method, path, allow string
	}{
		{http.MethodPut, "/notes", "GET, HEAD, POST"},
		{http.MethodDelete, "/notes", "GET, HEAD, POST"},
		{http.MethodPatch, "/notes", "GET, HEAD, POST"},
		{http.MethodPost, "/notes/1", "GET, HEAD, PUT, DELETE"},
		{http.MethodPatch, "/notes/1", "GET, HEAD, PUT, DELETE"},
		{http.MethodPost, "/health", "GET, HEAD"},
	}
	for _, tc := range tests {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := do(t, h, tc.method, tc.path, "")
			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status %d", rec.Code)
			}
			if got := rec.Header().Get("Allow"); got != tc.allow {
				t.Fatalf("allow %q want %q", got, tc.allow)
			}
			if _, ok := decodeMap(t, rec)["message"]; !ok {
				t.Fatalf("missing message")
			}
		})
	}
}

func TestNotFoundRoute(t *testing.T) {
	h, _ := newTestRouter(t)
	rec := do(t, h, http.MethodGet, "/nope", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d", rec.Code)
	}
	if m := decodeMap(t, rec); m["message"] != "The route nope could not be found." {
		t.Fatalf("got %v", m)
	}
}

func TestMethodSpoofing(t *testing.T) {
	h, _ := newTestRouter(t)
	do(t, h, http.MethodPost, "/notes", `{"title":"a"}`)
	rec := do(t, h, http.MethodPost, "/notes/1", `{"_method":"put","title":"b"}`)
	if rec.Code != 200 || decodeMap(t, rec)["title"] != "b" {
		t.Fatalf("spoof put: %d %s", rec.Code, rec.Body.String())
	}
	rec = do(t, h, http.MethodPost, "/notes/1", `{"_method":"DELETE"}`)
	if rec.Code != 204 {
		t.Fatalf("spoof delete: %d", rec.Code)
	}
}

func TestHelpers(t *testing.T) {
	if laravelPath("/") != "/" || laravelPath("/a/b/") != "a/b" {
		t.Fatal("laravelPath")
	}
	cases := []struct {
		in   any
		want string
	}{
		{nil, ""}, {true, "1"}, {false, ""}, {"x", "x"},
		{json.Number("12"), "12"}, {[]any{}, "Array"},
	}
	for _, c := range cases {
		if got := phpString(c.in); got != c.want {
			t.Errorf("phpString(%v)=%q want %q", c.in, got, c.want)
		}
	}
	if !isDigits("123") || isDigits("") || isDigits("1a") {
		t.Fatal("isDigits")
	}
}