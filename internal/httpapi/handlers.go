// Package httpapi exposes the notes JSON REST API (migrated from Laravel's
// App\Http\Controllers\NoteController) together with the Laravel request
// semantics it implicitly relied on.
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/log"
)

// DefaultNotesFile is the location Laravel's default "local" disk used for
// notes.json (storage/app/private). It can be overridden with NOTES_FILE.
const DefaultNotesFile = "storage/app/private/notes.json"

// Note is a single note as persisted in notes.json and returned by the API.
// Body is usually a string, but PHP's update() stored whatever JSON type the
// client sent, so it is kept as an arbitrary JSON value.
type Note struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
	Body  any    `json:"body"`
}

// NotePatch carries the fields of a partial update. A nil Title or Body means
// "not provided" and leaves the stored value untouched.
type NotePatch struct {
	Title *string
	Body  any
}

// NoteStore is the persistence contract the handlers depend on (the Go
// counterpart of App\Models\NoteRepository).
type NoteStore interface {
	All(ctx context.Context) ([]Note, error)
	Find(ctx context.Context, id int64) (Note, bool, error)
	Create(ctx context.Context, title, body string) (Note, error)
	Update(ctx context.Context, id int64, patch NotePatch) (Note, bool, error)
	Delete(ctx context.Context, id int64) (bool, error)
}

// RegisterRoutes registers every notes route on r, backed by the JSON file
// store (path from NOTES_FILE, defaulting to DefaultNotesFile).
func RegisterRoutes(r chi.Router) {
	path := os.Getenv("NOTES_FILE")
	if path == "" {
		path = DefaultNotesFile
	}
	NewHandler(NewFileStore(path)).Register(r)
}

// Handler serves the notes endpoints.
type Handler struct {
	notes NoteStore
}

// NewHandler returns a Handler backed by the given store.
func NewHandler(notes NoteStore) *Handler {
	return &Handler{notes: notes}
}

// Register mounts the handler's routes on r.
func (h *Handler) Register(r chi.Router) {
	r.Get("/notes", h.Index)
	r.Post("/notes", h.Store)
	r.Get("/notes/{id}", h.Show)
	r.Put("/notes/{id}", h.Update)
	r.Patch("/notes/{id}", h.Update)
	r.Delete("/notes/{id}", h.Destroy)
}

// Index lists all notes (an empty store serializes as []).
func (h *Handler) Index(w http.ResponseWriter, r *http.Request) {
	notes, err := h.notes.All(r.Context())
	if err != nil {
		serverError(w, r, err)
		return
	}
	if notes == nil {
		notes = []Note{}
	}
	writeJSON(w, http.StatusOK, notes)
}

// Show returns a single note or 404.
func (h *Handler) Show(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	note, found, err := h.notes.Find(r.Context(), id)
	if err != nil {
		serverError(w, r, err)
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, errorBody("not found"))
		return
	}
	writeJSON(w, http.StatusOK, note)
}

// Store creates a note from a required non-empty string title and an
// optional body (cast to string the way PHP's (string) cast does).
func (h *Handler) Store(w http.ResponseWriter, r *http.Request) {
	in, err := parseInput(r)
	if err != nil {
		serverError(w, r, err)
		return
	}
	title, ok := in["title"].(string)
	if !ok || title == "" {
		writeJSON(w, http.StatusBadRequest, errorBody("title is required"))
		return
	}
	body := ""
	if v, present := in["body"]; present {
		if body, err = phpStringCast(v); err != nil {
			serverError(w, r, err)
			return
		}
	}
	note, err := h.notes.Create(r.Context(), title, body)
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, note)
}

// Update partially updates a note. Null (and, after middleware emulation,
// empty/whitespace-only) fields are ignored. A provided title must be a
// string; this is checked before the note's existence.
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	in, err := parseInput(r)
	if err != nil {
		serverError(w, r, err)
		return
	}
	var patch NotePatch
	if v := in["title"]; v != nil {
		title, isString := v.(string)
		if !isString || title == "" {
			writeJSON(w, http.StatusBadRequest, errorBody("title must not be empty"))
			return
		}
		patch.Title = &title
	}
	if v := in["body"]; v != nil {
		patch.Body = v
	}
	note, found, err := h.notes.Update(r.Context(), id, patch)
	if err != nil {
		serverError(w, r, err)
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, errorBody("not found"))
		return
	}
	writeJSON(w, http.StatusOK, note)
}

// Destroy deletes a note: 204 with no body, or 404.
func (h *Handler) Destroy(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	deleted, err := h.notes.Delete(r.Context(), id)
	if err != nil {
		serverError(w, r, err)
		return
	}
	if !deleted {
		writeJSON(w, http.StatusNotFound, errorBody("not found"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// HTTP helpers
// ---------------------------------------------------------------------------

var numericID = regexp.MustCompile(`^[0-9]+$`)

// parseID mirrors Laravel's ->whereNumber('id') constraint plus the int
// parameter type: a non-matching or overflowing id is a router-level 404.
func parseID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := chi.URLParam(r, "id")
	if !numericID.MatchString(raw) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return 0, false
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		// MIGRATION_NOTE: PHP would coerce an overflowing numeric string to
		// float and fail the int type hint (500); we answer as the router 404.
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return 0, false
	}
	return id, true
}

func errorBody(msg string) map[string]string {
	return map[string]string{"error": msg}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	payload, err := json.Marshal(v)
	if err != nil {
		log.Error().Err(err).Msg("encode JSON response")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"Server Error"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}

// serverError logs err and answers like Laravel's production exception handler.
func serverError(w http.ResponseWriter, r *http.Request, err error) {
	log.Error().Err(err).Str("method", r.Method).Str("path", r.URL.Path).Msg("request failed")
	writeJSON(w, http.StatusInternalServerError, map[string]string{"message": "Server Error"})
}

// ---------------------------------------------------------------------------
// Laravel input emulation
// ---------------------------------------------------------------------------

// parseInput reproduces $request->input(): query string merged with the body
// (body wins), with the TrimStrings and ConvertEmptyStringsToNull global
// middleware applied.
func parseInput(r *http.Request) (map[string]any, error) {
	in := valuesToMap(r.URL.Query())

	body, err := bodyInput(r)
	if err != nil {
		return nil, err
	}
	for k, v := range body {
		in[k] = v
	}
	for k, v := range in {
		in[k] = cleanValue(v)
	}
	return in, nil
}

func bodyInput(r *http.Request) (map[string]any, error) {
	if r.Body == nil {
		return nil, nil
	}
	ct := r.Header.Get("Content-Type")
	if strings.Contains(ct, "/json") || strings.Contains(ct, "+json") {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, fmt.Errorf("read request body: %w", err)
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			return nil, nil // invalid JSON contributes nothing, like Laravel
		}
		obj, _ := v.(map[string]any)
		return obj, nil
	}

	mediaType, _, _ := mime.ParseMediaType(ct)
	switch {
	case mediaType == "multipart/form-data" && r.Method == http.MethodPost:
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			return nil, nil
		}
	case mediaType == "application/x-www-form-urlencoded":
		if err := r.ParseForm(); err != nil {
			return nil, nil
		}
	default:
		return nil, nil
	}
	return valuesToMap(r.PostForm), nil
}

// valuesToMap converts url.Values into PHP-like input: the last duplicate
// wins, and "key[]" / "key[sub]" become arrays.
// MIGRATION_NOTE: only one level of PHP bracket nesting is reproduced.
func valuesToMap(values url.Values) map[string]any {
	out := make(map[string]any, len(values))
	for key, vals := range values {
		if len(vals) == 0 {
			continue
		}
		open := strings.IndexByte(key, '[')
		if open <= 0 || !strings.HasSuffix(key, "]") {
			out[key] = vals[len(vals)-1]
			continue
		}
		base, sub := key[:open], key[open+1:len(key)-1]
		if sub == "" {
			list, _ := out[base].([]any)
			for _, v := range vals {
				list = append(list, v)
			}
			out[base] = list
			continue
		}
		m, _ := out[base].(map[string]any)
		if m == nil {
			m = map[string]any{}
		}
		m[sub] = vals[len(vals)-1]
		out[base] = m
	}
	return out
}

func isTrimRune(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\v', '\f', '\r', '\uFEFF', '\u200B', '\u200E':
		return true
	}
	return false
}

// cleanValue applies TrimStrings and ConvertEmptyStringsToNull recursively.
func cleanValue(v any) any {
	switch t := v.(type) {
	case string:
		s := strings.TrimFunc(t, isTrimRune)
		if s == "" {
			return nil
		}
		return s
	case []any:
		for i := range t {
			t[i] = cleanValue(t[i])
		}
		return t
	case map[string]any:
		for k := range t {
			t[k] = cleanValue(t[k])
		}
		return t
	default:
		return v
	}
}

// errArrayToString mirrors PHP's "Array to string conversion" error.
var errArrayToString = errors.New("array to string conversion")

// phpStringCast mirrors PHP's (string) cast for decoded input values.
func phpStringCast(v any) (string, error) {
	switch t := v.(type) {
	case nil:
		return "", nil
	case string:
		return t, nil
	case bool:
		if t {
			return "1", nil
		}
		return "", nil
	case json.Number:
		if i, err := strconv.ParseInt(t.String(), 10, 64); err == nil {
			return strconv.FormatInt(i, 10), nil
		}
		f, err := strconv.ParseFloat(t.String(), 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			return "", fmt.Errorf("cast number %q: %w", t, err)
		}
		return phpFloatString(f), nil
	case []any, map[string]any:
		return "", errArrayToString
	default:
		return fmt.Sprint(t), nil
	}
}

// phpFloatString formats f like PHP's (string) cast (precision=14).
func phpFloatString(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "INF"
	case math.IsInf(f, -1):
		return "-INF"
	case math.IsNaN(f):
		return "NAN"
	}
	s := strconv.FormatFloat(f, 'G', 14, 64)
	e := strings.IndexByte(s, 'E')
	if e < 0 {
		return s
	}
	mant, exp := s[:e], s[e+1:]
	if !strings.Contains(mant, ".") {
		mant += ".0"
	}
	sign := exp[:1]
	digits := strings.TrimLeft(exp[1:], "0")
	if digits == "" {
		digits = "0"
	}
	return mant + "E" + sign + digits
}

// ---------------------------------------------------------------------------
// JSON file store (compatible with Laravel's storage/app/private/notes.json)
// ---------------------------------------------------------------------------

// FileStore keeps all notes in a single JSON file, compatible in shape
// with what the PHP NoteRepository wrote: {"next_id":N,"notes":{"1":{...}}}.
type FileStore struct {
	mu   sync.Mutex
	path string
}

// NewFileStore returns a FileStore persisting to path.
func NewFileStore(path string) *FileStore {
	return &FileStore{path: path}
}

type noteEntry struct {
	key  string
	note Note
}

type notesData struct {
	nextID  int64
	entries []noteEntry // insertion order, as PHP arrays preserve it
}

func (d *notesData) index(id int64) int {
	key := strconv.FormatInt(id, 10)
	for i, e := range d.entries {
		if e.key == key {
			return i
		}
	}
	return -1
}

func (s *FileStore) load() (*notesData, error) {
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return &notesData{nextID: 1}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read notes file: %w", err)
	}

	var top struct {
		NextID json.Number     `json:"next_id"`
		Notes  json.RawMessage `json:"notes"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("decode notes file: %w", err)
	}
	nextID, err := strconv.ParseInt(top.NextID.String(), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("decode notes file next_id: %w", err)
	}
	data := &notesData{nextID: nextID}

	trimmed := bytes.TrimSpace(top.Notes)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return data, nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	switch trimmed[0] {
	case '[':
		var list []Note
		if err := dec.Decode(&list); err != nil {
			return nil, fmt.Errorf("decode notes list: %w", err)
		}
		for i, n := range list {
			data.entries = append(data.entries, noteEntry{key: strconv.Itoa(i), note: n})
		}
	case '{':
		if _, err := dec.Token(); err != nil {
			return nil, fmt.Errorf("decode notes object: %w", err)
		}
		for dec.More() {
			tok, err := dec.Token()
			if err != nil {
				return nil, fmt.Errorf("decode notes key: %w", err)
			}
			key, _ := tok.(string)
			var n Note
			if err := dec.Decode(&n); err != nil {
				return nil, fmt.Errorf("decode note %q: %w", key, err)
			}
			data.entries = append(data.entries, noteEntry{key: key, note: n})
		}
	default:
		return nil, errors.New("decode notes file: notes is neither object nor array")
	}
	return data, nil
}

func (s *FileStore) save(data *notesData) error {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, `{"next_id":%d,"notes":`, data.nextID)

	sequential := true
	for i, e := range data.entries {
		if e.key != strconv.Itoa(i) {
			sequential = false
			break
		}
	}
	open, closing := byte('{'), byte('}')
	if sequential {
		open, closing = '[', ']'
	}
	buf.WriteByte(open)
	for i, e := range data.entries {
		if i > 0 {
			buf.WriteByte(',')
		}
		if !sequential {
			k, _ := json.Marshal(e.key)
			buf.Write(k)
			buf.WriteByte(':')
		}
		n, err := json.Marshal(e.note)
		if err != nil {
			return fmt.Errorf("encode note: %w", err)
		}
		buf.Write(n)
	}
	buf.WriteByte(closing)
	buf.WriteByte('}')

	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("create notes dir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".notes-*.json")
	if err != nil {
		return fmt.Errorf("create temp notes file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		return fmt.Errorf("write notes file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close notes file: %w", err)
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		return fmt.Errorf("replace notes file: %w", err)
	}
	return nil
}

// All returns every note in insertion order.
func (s *FileStore) All(_ context.Context) ([]Note, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.load()
	if err != nil {
		return nil, err
	}
	notes := make([]Note, 0, len(data.entries))
	for _, e := range data.entries {
		notes = append(notes, e.note)
	}
	return notes, nil
}

// Find returns the note stored under id.
func (s *FileStore) Find(_ context.Context, id int64) (Note, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.load()
	if err != nil {
		return Note{}, false, err
	}
	i := data.index(id)
	if i < 0 {
		return Note{}, false, nil
	}
	return data.entries[i].note, true, nil
}

// Create stores a new note under the next id.
func (s *FileStore) Create(_ context.Context, title, body string) (Note, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.load()
	if err != nil {
		return Note{}, err
	}
	note := Note{ID: data.nextID, Title: title, Body: body}
	key := strconv.FormatInt(note.ID, 10)
	if i := data.index(note.ID); i >= 0 {
		data.entries[i].note = note
	} else {
		data.entries = append(data.entries, noteEntry{key: key, note: note})
	}
	data.nextID++
	if err := s.save(data); err != nil {
		return Note{}, err
	}
	return note, nil
}

// Update applies patch to the note under id. The file is rewritten even for
// an empty patch, as the PHP repository did.
func (s *FileStore) Update(_ context.Context, id int64, patch NotePatch) (Note, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.load()
	if err != nil {
		return Note{}, false, err
	}
	i := data.index(id)
	if i < 0 {
		return Note{}, false, nil
	}
	note := &data.entries[i].note
	if patch.Title != nil {
		note.Title = *patch.Title
	}
	if patch.Body != nil {
		note.Body = patch.Body
	}
	if err := s.save(data); err != nil {
		return Note{}, false, err
	}
	return *note, true, nil
}

// Delete removes the note under id, reporting whether it existed.
func (s *FileStore) Delete(_ context.Context, id int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.load()
	if err != nil {
		return false, err
	}
	i := data.index(id)
	if i < 0 {
		return false, nil
	}
	data.entries = append(data.entries[:i], data.entries[i+1:]...)
	if err := s.save(data); err != nil {
		return false, err
	}
	return true, nil
}
