package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

func hNewRouter(t *testing.T) http.Handler {
	t.Helper()
	t.Setenv("NOTES_FILE", filepath.Join(t.TempDir(), "sub", "notes.json"))
	r := chi.NewRouter()
	RegisterRoutes(r)
	return r
}

func hServe(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func hDecode(t *testing.T, rec *httptest.ResponseRecorder) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("invalid JSON %q: %v", rec.Body.String(), err)
	}
	return v
}

func hAssertError(t *testing.T, rec *httptest.ResponseRecorder, status int, msg string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d (%s)", rec.Code, status, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("content-type = %q", ct)
	}
	want := map[string]any{"error": msg}
	if got := hDecode(t, rec); !reflect.DeepEqual(got, want) {
		t.Errorf("body = %v, want %v", got, want)
	}
}

func TestHandlersIndexEmpty(t *testing.T) {
	h := hNewRouter(t)
	rec := hServe(t, h, http.MethodGet, "/notes", "")
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("index: %d %s", rec.Code, rec.Body.String())
	}
}

func TestHandlersShow(t *testing.T) {
	h := hNewRouter(t)
	hServe(t, h, http.MethodPost, "/notes", `{"title":"t","body":"b"}`)
	rec := hServe(t, h, http.MethodGet, "/notes/1", "")
	want := map[string]any{"id": float64(1), "title": "t", "body": "b"}
	if rec.Code != 200 || !reflect.DeepEqual(hDecode(t, rec), want) {
		t.Fatalf("show: %d %s", rec.Code, rec.Body.String())
	}
	hAssertError(t, hServe(t, h, http.MethodGet, "/notes/9", ""), 404, "not found")
	hAssertError(t, hServe(t, h, http.MethodGet, "/notes/99999999999999999999999", ""), 404, "not found")
	rec = hServe(t, h, http.MethodGet, "/notes/abc", "")
	if rec.Code != 404 {
		t.Fatalf("non-numeric: %d", rec.Code)
	}
}

func TestHandlersStoreCases(t *testing.T) {
	tests := []struct {
		name, body string
		status     int
		wantTitle  string
		wantBody   string
		wantErr    string
	}{
		{"title and body", `{"title":"hello","body":"world"}`, 201, "hello", "world", ""},
		{"body omitted", `{"title":"hello"}`, 201, "hello", "", ""},
		{"body number", `{"title":"hello","body":42}`, 201, "hello", "42", ""},
		{"body float", `{"title":"hello","body":1.5}`, 201, "hello", "1.5", ""},
		{"body true", `{"title":"hello","body":true}`, 201, "hello", "1", ""},
		{"body false", `{"title":"hello","body":false}`, 201, "hello", "", ""},
		{"body null", `{"title":"hello","body":null}`, 201, "hello", "", ""},
		{"title missing", `{"body":"x"}`, 400, "", "", "title is required"},
		{"title null", `{"title":null}`, 400, "", "", "title is required"},
		{"title empty", `{"title":""}`, 400, "", "", "title is required"},
		{"title whitespace", `{"title":"   "}`, 400, "", "", "title is required"},
		{"title number", `{"title":5}`, 400, "", "", "title is required"},
		{"title array", `{"title":["a"]}`, 400, "", "", "title is required"},
		{"invalid json", `{not json`, 400, "", "", "title is required"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := hNewRouter(t)
			rec := hServe(t, h, http.MethodPost, "/notes", tc.body)
			if tc.wantErr != "" {
				hAssertError(t, rec, tc.status, tc.wantErr)
				return
			}
			if rec.Code != tc.status {
				t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
			}
			want := map[string]any{"id": float64(1), "title": tc.wantTitle, "body": tc.wantBody}
			if got := hDecode(t, rec); !reflect.DeepEqual(got, want) {
				t.Errorf("body = %v, want %v", got, want)
			}
		})
	}
}

func TestHandlersStoreFormEncoded(t *testing.T) {
	h := hNewRouter(t)
	req := httptest.NewRequest(http.MethodPost, "/notes", strings.NewReader("title=form&body=b"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 201 || !strings.Contains(rec.Body.String(), `"title":"form"`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandlersUpdateCases(t *testing.T) {
	tests := []struct {
		name, path, body string
		status           int
		wantErr          string
		title, nbody     string
	}{
		{"valid", "/notes/1", `{"title":"new","body":"nb"}`, 200, "", "new", "nb"},
		{"body only", "/notes/1", `{"body":"nb"}`, 200, "", "old", "nb"},
		{"nulls ignored", "/notes/1", `{"title":null,"body":null,"x":1}`, 200, "", "old", "oldbody"},
		{"empty title ignored", "/notes/1", `{"title":""}`, 200, "", "old", "oldbody"},
		{"title number", "/notes/1", `{"title":7}`, 400, "title must not be empty", "old", "oldbody"},
		{"invalid title missing id", "/notes/999", `{"title":1}`, 400, "title must not be empty", "old", "oldbody"},
		{"not found", "/notes/999", `{"title":"x"}`, 404, "not found", "old", "oldbody"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := hNewRouter(t)
			hServe(t, h, http.MethodPost, "/notes", `{"title":"old","body":"oldbody"}`)
			rec := hServe(t, h, http.MethodPut, tc.path, tc.body)
			if tc.wantErr != "" {
				hAssertError(t, rec, tc.status, tc.wantErr)
			} else if rec.Code != tc.status {
				t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
			}
			got := hDecode(t, hServe(t, h, http.MethodGet, "/notes/1", ""))
			want := map[string]any{"id": float64(1), "title": tc.title, "body": tc.nbody}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("stored = %v, want %v", got, want)
			}
		})
	}
}

func TestHandlersUpdateBodyNotCast(t *testing.T) {
	h := hNewRouter(t)
	hServe(t, h, http.MethodPost, "/notes", `{"title":"t"}`)
	rec := hServe(t, h, http.MethodPut, "/notes/1", `{"body":42}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"body":42`) {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
}

func TestHandlersDestroyCases(t *testing.T) {
	h := hNewRouter(t)
	hServe(t, h, http.MethodPost, "/notes", `{"title":"t"}`)
	rec := hServe(t, h, http.MethodDelete, "/notes/1", "")
	if rec.Code != 204 || rec.Body.Len() != 0 {
		t.Fatalf("delete: %d %q", rec.Code, rec.Body.String())
	}
	hAssertError(t, hServe(t, h, http.MethodDelete, "/notes/1", ""), 404, "not found")
	if rec := hServe(t, h, http.MethodDelete, "/notes/x", ""); rec.Code != 404 {
		t.Fatalf("bad id: %d", rec.Code)
	}
}

func TestHandlersPhpString(t *testing.T) {
	tests := []struct {
		in   any
		want string
	}{
		{nil, ""},
		{"s", "s"},
		{true, "1"},
		{false, ""},
		{json.Number("42"), "42"},
		{json.Number("1.5"), "1.5"},
		{[]any{1}, "Array"},
		{map[string]any{}, "Array"},
	}
	for _, tc := range tests {
		if got := phpString(tc.in); got != tc.want {
			t.Errorf("phpString(%#v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}