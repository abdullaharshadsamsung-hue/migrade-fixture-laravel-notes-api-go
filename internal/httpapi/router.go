// Package httpapi exposes the notes JSON API, migrated from the Laravel
// routes/api.php file, NoteController and the JSON-file NoteRepository.
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/go-chi/chi/v5"
)

// DefaultNotesFile is where Laravel's local disk ("storage/app/private")
// kept notes.json. It can be overridden with the NOTES_FILE env var.
const DefaultNotesFile = "storage/app/private/notes.json"

// maxBodyBytes caps how much of a request body is read for input parsing.
const maxBodyBytes = 32 << 20

// idPattern mirrors Laravel's ->whereNumber('id') constraint ([0-9]+).
const idPattern = "{id:[0-9]+}"

// RegisterRoutes registers every route from routes/api.php on r.
//
// MIGRATION_NOTE: bootstrap/app.php sets apiPrefix: '', so Laravel served
// these routes WITHOUT the /api prefix (/health, /notes, ...). The migration
// contract lists them under /api. Both prefixes are registered with the same
// handlers so clients of either shape keep working. Review and drop one.
func RegisterRoutes(r chi.Router) {
	path := os.Getenv("NOTES_FILE")
	if path == "" {
		path = DefaultNotesFile
	}
	h := newNotesHandler(NewFileStore(path))

	// Laravel's fallback behaviour: JSON 404 / 405 bodies.
	r.NotFound(notFoundHandler)
	r.MethodNotAllowed(methodNotAllowedHandler)

	// bootstrap/app.php sets apiPrefix: '', so routes live at the root only;
	// /api/* falls through to Laravel's 404.
	for _, prefix := range []string{""} {
		// Laravel answers HEAD for every GET route automatically.
		r.Get(prefix+"/health", healthHandler)
		r.Head(prefix+"/health", healthHandler)

		r.Get(prefix+"/notes", h.index)
		r.Head(prefix+"/notes", h.index)
		r.Post(prefix+"/notes", h.postCollection)

		r.Get(prefix+"/notes/"+idPattern, h.show)
		r.Head(prefix+"/notes/"+idPattern, h.show)
		r.Put(prefix+"/notes/"+idPattern, h.update)
		r.Delete(prefix+"/notes/"+idPattern, h.destroy)
		// POST is registered only to support Laravel's _method spoofing
		// (POST + _method=PUT/DELETE); plain POST answers 405.
		r.Post(prefix+"/notes/"+idPattern, h.postMember)
	}
}

// healthHandler mirrors: Route::get('/health', fn () => response()->json(['status' => 'ok'])).
func healthHandler(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ---------------------------------------------------------------------------
// Handlers (NoteController)
// ---------------------------------------------------------------------------

type notesHandler struct {
	store *FileStore
}

func newNotesHandler(store *FileStore) *notesHandler {
	return &notesHandler{store: store}
}

func (h *notesHandler) index(w http.ResponseWriter, r *http.Request) {
	notes, err := h.store.All(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, notes)
}

func (h *notesHandler) show(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r)
	if !ok {
		noteNotFound(w)
		return
	}
	note, found, err := h.store.Find(r.Context(), id)
	if err != nil {
		serverError(w, err)
		return
	}
	if !found {
		noteNotFound(w)
		return
	}
	writeJSON(w, http.StatusOK, note)
}

func (h *notesHandler) store_(w http.ResponseWriter, r *http.Request, in map[string]any) {
	title, ok := in["title"].(string)
	if !ok || title == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "title is required"})
		return
	}
	// (string) $request->input('body', '')
	body := phpString(in["body"])
	note, err := h.store.Create(r.Context(), title, body)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, note)
}

func (h *notesHandler) update(w http.ResponseWriter, r *http.Request) {
	h.updateWith(w, r, readInput(r))
}

func (h *notesHandler) updateWith(w http.ResponseWriter, r *http.Request, in map[string]any) {
	id, ok := parseID(r)
	if !ok {
		noteNotFound(w)
		return
	}
	// array_filter($request->only(['title','body']), fn ($v) => $v !== null)
	fields := make(map[string]any, 2)
	for _, k := range []string{"title", "body"} {
		if v, present := in[k]; present && v != nil {
			fields[k] = v
		}
	}
	if v, present := fields["title"]; present {
		if s, isStr := v.(string); !isStr || s == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "title must not be empty"})
			return
		}
	}
	note, found, err := h.store.Update(r.Context(), id, fields)
	if err != nil {
		serverError(w, err)
		return
	}
	if !found {
		noteNotFound(w)
		return
	}
	writeJSON(w, http.StatusOK, note)
}

func (h *notesHandler) destroy(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r)
	if !ok {
		noteNotFound(w)
		return
	}
	deleted, err := h.store.Delete(r.Context(), id)
	if err != nil {
		serverError(w, err)
		return
	}
	if !deleted {
		noteNotFound(w)
		return
	}
	// response()->json(null, 204): Symfony strips the body of a 204.
	w.WriteHeader(http.StatusNoContent)
}

// postCollection handles POST /notes, honouring Laravel method spoofing.
func (h *notesHandler) postCollection(w http.ResponseWriter, r *http.Request) {
	in := readInput(r)
	switch method := overrideMethod(r, in); method {
	case http.MethodPost:
		h.store_(w, r, in)
	case http.MethodGet, http.MethodHead:
		h.index(w, r)
	default:
		writeMethodNotAllowed(w, r, method)
	}
}

// postMember handles POST /notes/{id}, which only exists via method spoofing.
func (h *notesHandler) postMember(w http.ResponseWriter, r *http.Request) {
	in := readInput(r)
	switch method := overrideMethod(r, in); method {
	case http.MethodPut:
		h.updateWith(w, r, in)
	case http.MethodDelete:
		h.destroy(w, r)
	case http.MethodGet, http.MethodHead:
		h.show(w, r)
	default:
		writeMethodNotAllowed(w, r, method)
	}
}

func parseID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		// Digits only (route constraint), so the only failure is overflow.
		return 0, false
	}
	return id, true
}

func noteNotFound(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
}

// ---------------------------------------------------------------------------
// Framework behaviour (404 / 405 / errors / input)
// ---------------------------------------------------------------------------

// MIGRATION_NOTE: Laravel renders HTML error pages for non-JSON clients;
// this port always answers JSON.
func notFoundHandler(w http.ResponseWriter, r *http.Request) {
	if !expectsJSON(r) {
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		w.WriteHeader(http.StatusNotFound)
		if r.Method != http.MethodHead {
			_, _ = w.Write([]byte(notFoundHTML))
		}
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]string{
		"message": fmt.Sprintf("The route %s could not be found.", laravelPath(r.URL.Path)),
	})
}

// expectsJSON mirrors Laravel's Request::expectsJson().
func expectsJSON(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	ajax := r.Header.Get("X-Requested-With") == "XMLHttpRequest"
	pjax := r.Header.Get("X-PJAX") == "true"
	if ajax && !pjax && (accept == "" || strings.Contains(accept, "*")) {
		return true
	}
	first := strings.TrimSpace(strings.Split(accept, ",")[0])
	return strings.Contains(first, "/json") || strings.Contains(first, "+json")
}

const notFoundHTML = `<!DOCTYPE html>
<html lang="en">
    <head>
        <meta charset="utf-8">
        <meta name="viewport" content="width=device-width, initial-scale=1">

        <title>Not Found</title>
    </head>
    <body class="antialiased">
        <div>404 | Not Found</div>
    </body>
</html>
`

func methodNotAllowedHandler(w http.ResponseWriter, r *http.Request) {
	writeMethodNotAllowed(w, r, r.Method)
}

func writeMethodNotAllowed(w http.ResponseWriter, r *http.Request, method string) {
	allowed := allowedMethods(r.URL.Path)
	if len(allowed) == 0 {
		notFoundHandler(w, r)
		return
	}
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
		"message": fmt.Sprintf("The %s method is not supported for route %s. Supported methods: %s.",
			method, laravelPath(r.URL.Path), strings.Join(allowed, ", ")),
	})
}

// allowedMethods lists the methods Laravel would report for a path.
func allowedMethods(path string) []string {
	p := strings.TrimSuffix(path, "/")
	switch {
	case p == "/health":
		return []string{"GET", "HEAD"}
	case p == "/notes":
		return []string{"GET", "HEAD", "POST"}
	case strings.HasPrefix(p, "/notes/") && isDigits(strings.TrimPrefix(p, "/notes/")):
		return []string{"GET", "HEAD", "PUT", "DELETE"}
	default:
		return nil
	}
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// laravelPath mirrors Request::path(): no leading/trailing slash, "/" for root.
func laravelPath(p string) string {
	t := strings.Trim(p, "/")
	if t == "" {
		return "/"
	}
	return t
}

// overrideMethod reproduces Symfony's HTTP method override for POST requests:
// X-HTTP-Method-Override header first, then the _method input field.
func overrideMethod(r *http.Request, in map[string]any) string {
	if r.Method != http.MethodPost {
		return r.Method
	}
	m := r.Header.Get("X-HTTP-Method-Override")
	if m == "" {
		s, ok := in["_method"].(string)
		if !ok {
			return http.MethodPost
		}
		m = s
	}
	return strings.ToUpper(m)
}

// readInput reproduces $request->input(): query string merged with the
// request body (body wins), followed by Laravel's global TrimStrings and
// ConvertEmptyStringsToNull middleware.
func readInput(r *http.Request) map[string]any {
	in := make(map[string]any)
	for k, vs := range r.URL.Query() {
		if len(vs) > 0 {
			in[k] = vs[len(vs)-1] // PHP keeps the last duplicate key
		}
	}

	ct := r.Header.Get("Content-Type")
	mediaType, _, _ := mime.ParseMediaType(ct)
	switch {
	case strings.Contains(ct, "/json") || strings.Contains(ct, "+json"):
		raw, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
		if err == nil && len(bytes.TrimSpace(raw)) > 0 {
			var obj map[string]any
			if err := decodeJSON(raw, &obj); err == nil {
				for k, v := range obj {
					in[k] = v
				}
			}
		}
	case mediaType == "application/x-www-form-urlencoded":
		if err := r.ParseForm(); err == nil {
			mergeValues(in, r.PostForm)
		}
	case mediaType == "multipart/form-data":
		if err := r.ParseMultipartForm(maxBodyBytes); err == nil {
			mergeValues(in, r.PostForm)
		}
	}

	out, _ := normalizeInput(in).(map[string]any)
	if out == nil {
		out = map[string]any{}
	}
	return out
}

func mergeValues(dst map[string]any, src map[string][]string) {
	for k, vs := range src {
		if len(vs) > 0 {
			dst[k] = vs[len(vs)-1]
		}
	}
}

// normalizeInput applies TrimStrings + ConvertEmptyStringsToNull recursively.
func normalizeInput(v any) any {
	switch t := v.(type) {
	case string:
		s := strings.Trim(t, " \t\n\r\x00\x0B")
		if s == "" {
			return nil
		}
		return s
	case map[string]any:
		for k, val := range t {
			t[k] = normalizeInput(val)
		}
		return t
	case []any:
		for i, val := range t {
			t[i] = normalizeInput(val)
		}
		return t
	default:
		return v
	}
}

// phpString reproduces PHP's (string) cast for decoded input values.
func phpString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "1"
		}
		return ""
	case json.Number:
		s := t.String()
		if strings.ContainsAny(s, ".eE") {
			if f, err := t.Float64(); err == nil {
				return strconv.FormatFloat(f, 'G', -1, 64)
			}
		}
		return s
	case map[string]any, []any:
		return "Array"
	default:
		return fmt.Sprint(t)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	payload, err := json.Marshal(v)
	if err != nil {
		slog.Error("encode JSON response", "err", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"Server Error"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}

func serverError(w http.ResponseWriter, err error) {
	slog.Error("notes request failed", "err", err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"message": "Server Error"})
}

func decodeJSON(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	return dec.Decode(v)
}

// ---------------------------------------------------------------------------
// Storage (NoteRepository)
// ---------------------------------------------------------------------------

// Note is one stored note. Title and Body are untyped because the source
// stored whatever (non-null) value the client sent on update.
type Note struct {
	ID    int64 `json:"id"`
	Title any   `json:"title"`
	Body  any   `json:"body"`
}

// FileStore keeps notes in a single JSON file, compatible with the file the
// Laravel NoteRepository wrote: {"next_id":N,"notes":{"<id>":{...}}}.
type FileStore struct {
	mu   sync.Mutex
	path string
}

// NewFileStore returns a FileStore backed by the file at path.
func NewFileStore(path string) *FileStore {
	return &FileStore{path: path}
}

type notesDoc struct {
	nextID int64
	notes  map[int64]Note
}

func (s *FileStore) load() (notesDoc, error) {
	doc := notesDoc{nextID: 1, notes: map[int64]Note{}}
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return doc, nil
	}
	if err != nil {
		return doc, fmt.Errorf("read notes file: %w", err)
	}

	var file struct {
		NextID json.Number     `json:"next_id"`
		Notes  json.RawMessage `json:"notes"`
	}
	if err := decodeJSON(raw, &file); err != nil {
		return doc, fmt.Errorf("decode notes file: %w", err)
	}
	if file.NextID != "" {
		n, err := file.NextID.Int64()
		if err != nil {
			return doc, fmt.Errorf("decode next_id: %w", err)
		}
		doc.nextID = n
	}

	notesRaw := bytes.TrimSpace(file.Notes)
	switch {
	case len(notesRaw) == 0 || bytes.Equal(notesRaw, []byte("null")):
	case notesRaw[0] == '[':
		// PHP json_encode writes an empty (or 0-indexed) array as a list.
		var list []Note
		if err := decodeJSON(notesRaw, &list); err != nil {
			return doc, fmt.Errorf("decode notes list: %w", err)
		}
		for i, n := range list {
			doc.notes[int64(i)] = n
		}
	default:
		var obj map[string]Note
		if err := decodeJSON(notesRaw, &obj); err != nil {
			return doc, fmt.Errorf("decode notes map: %w", err)
		}
		for k, n := range obj {
			id, err := strconv.ParseInt(k, 10, 64)
			if err != nil {
				slog.Warn("skipping note with non-numeric key", "key", k)
				continue
			}
			doc.notes[id] = n
		}
	}
	return doc, nil
}

func (s *FileStore) save(doc notesDoc) error {
	var buf bytes.Buffer
	buf.WriteString(`{"next_id":`)
	buf.WriteString(strconv.FormatInt(doc.nextID, 10))
	buf.WriteString(`,"notes":`)
	if len(doc.notes) == 0 {
		buf.WriteString("[]")
	} else {
		buf.WriteByte('{')
		for i, id := range sortedIDs(doc.notes) {
			if i > 0 {
				buf.WriteByte(',')
			}
			enc, err := json.Marshal(doc.notes[id])
			if err != nil {
				return fmt.Errorf("encode note %d: %w", id, err)
			}
			buf.WriteString(strconv.Quote(strconv.FormatInt(id, 10)))
			buf.WriteByte(':')
			buf.Write(enc)
		}
		buf.WriteByte('}')
	}
	buf.WriteByte('}')

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create notes dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".notes-*.json")
	if err != nil {
		return fmt.Errorf("create temp notes file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("write notes file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close notes file: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("replace notes file: %w", err)
	}
	return nil
}

func sortedIDs(m map[int64]Note) []int64 {
	ids := make([]int64, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// All returns every note ordered by id.
func (s *FileStore) All(ctx context.Context) ([]Note, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.load()
	if err != nil {
		return nil, err
	}
	out := make([]Note, 0, len(doc.notes))
	for _, id := range sortedIDs(doc.notes) {
		out = append(out, doc.notes[id])
	}
	return out, nil
}

// Find returns the note with the given id, if any.
func (s *FileStore) Find(ctx context.Context, id int64) (Note, bool, error) {
	if err := ctx.Err(); err != nil {
		return Note{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.load()
	if err != nil {
		return Note{}, false, err
	}
	n, ok := doc.notes[id]
	return n, ok, nil
}

// Create stores a new note and returns it.
func (s *FileStore) Create(ctx context.Context, title, body string) (Note, error) {
	if err := ctx.Err(); err != nil {
		return Note{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.load()
	if err != nil {
		return Note{}, err
	}
	n := Note{ID: doc.nextID, Title: title, Body: body}
	doc.notes[n.ID] = n
	doc.nextID++
	if err := s.save(doc); err != nil {
		return Note{}, err
	}
	return n, nil
}

// Update merges fields ("title"/"body") into the note with the given id.
func (s *FileStore) Update(ctx context.Context, id int64, fields map[string]any) (Note, bool, error) {
	if err := ctx.Err(); err != nil {
		return Note{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.load()
	if err != nil {
		return Note{}, false, err
	}
	n, ok := doc.notes[id]
	if !ok {
		return Note{}, false, nil
	}
	if v, present := fields["title"]; present {
		n.Title = v
	}
	if v, present := fields["body"]; present {
		n.Body = v
	}
	doc.notes[id] = n
	if err := s.save(doc); err != nil {
		return Note{}, false, err
	}
	return n, true, nil
}

// Delete removes the note with the given id, reporting whether it existed.
func (s *FileStore) Delete(ctx context.Context, id int64) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.load()
	if err != nil {
		return false, err
	}
	if _, ok := doc.notes[id]; !ok {
		return false, nil
	}
	delete(doc.notes, id)
	if err := s.save(doc); err != nil {
		return false, err
	}
	return true, nil
}
