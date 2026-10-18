// Package note implements persistence for notes.
package note

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
)

// FileName is the name of the JSON document holding every note, relative to
// the storage directory (Laravel's default "local" disk root,
// storage/app/private).
const FileName = "notes.json"

// Note is a single stored note. Body is kept as raw JSON because the PHP
// implementation passed update payloads straight through, so a body may be any
// JSON type (create always stores a string).
type Note struct {
	ID    int64           `json:"id"`
	Title string          `json:"title"`
	Body  json.RawMessage `json:"body"`
}

// Patch holds the fields to shallow-merge into an existing note. A nil field
// is left untouched (mirrors PHP array_merge with only the provided keys).
type Patch struct {
	Title *string
	Body  json.RawMessage
}

// Store is the persistence contract for notes. Absence is reported via the
// bool return value rather than an error.
type Store interface {
	All(ctx context.Context) ([]Note, error)
	Find(ctx context.Context, id int64) (Note, bool, error)
	Create(ctx context.Context, title, body string) (Note, error)
	Update(ctx context.Context, id int64, p Patch) (Note, bool, error)
	Delete(ctx context.Context, id int64) (bool, error)
}

// FileStore keeps all notes in a single JSON file so no database is needed.
// Every operation loads the whole document, mutates it and rewrites it; the
// mutex serialises these read-modify-write cycles within this process.
//
// MIGRATION_NOTE: the lock is in-process only. Running several instances
// against the same file can lose writes, exactly as the PHP original could.
type FileStore struct {
	mu   sync.Mutex
	path string
}

var _ Store = (*FileStore)(nil)

// NewFileStore returns a FileStore that persists to dir/notes.json. The
// directory is created on first write if it does not exist.
func NewFileStore(dir string) *FileStore {
	return &FileStore{path: filepath.Join(dir, FileName)}
}

// document is the in-memory form of notes.json.
type document struct {
	nextID int64
	notes  map[int64]Note
}

// All returns every stored note ordered by id. It never returns nil.
func (s *FileStore) All(ctx context.Context) ([]Note, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	doc, err := s.load(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Note, 0, len(doc.notes))
	for _, id := range sortedIDs(doc.notes) {
		out = append(out, doc.notes[id])
	}
	return out, nil
}

// Find returns the note with the given id; ok is false if it does not exist.
func (s *FileStore) Find(ctx context.Context, id int64) (Note, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	doc, err := s.load(ctx)
	if err != nil {
		return Note{}, false, err
	}
	n, ok := doc.notes[id]
	return n, ok, nil
}

// Create stores a new note under the next auto-increment id. Ids are never
// reused, even after deletion.
func (s *FileStore) Create(ctx context.Context, title, body string) (Note, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	doc, err := s.load(ctx)
	if err != nil {
		return Note{}, err
	}
	rawBody, err := json.Marshal(body)
	if err != nil {
		return Note{}, fmt.Errorf("note: encode body: %w", err)
	}
	n := Note{ID: doc.nextID, Title: title, Body: rawBody}
	doc.notes[n.ID] = n
	doc.nextID++
	if err := s.save(ctx, doc); err != nil {
		return Note{}, err
	}
	return n, nil
}

// Update shallow-merges the provided fields into an existing note and returns
// the updated note; ok is false if the note does not exist.
func (s *FileStore) Update(ctx context.Context, id int64, p Patch) (Note, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	doc, err := s.load(ctx)
	if err != nil {
		return Note{}, false, err
	}
	n, ok := doc.notes[id]
	if !ok {
		return Note{}, false, nil
	}
	if p.Title != nil {
		n.Title = *p.Title
	}
	if p.Body != nil {
		if !json.Valid(p.Body) {
			return Note{}, false, fmt.Errorf("note: body is not valid JSON")
		}
		n.Body = append(json.RawMessage(nil), p.Body...)
	}
	doc.notes[id] = n
	if err := s.save(ctx, doc); err != nil {
		return Note{}, false, err
	}
	return n, true, nil
}

// Delete removes the note with the given id; it reports false if the note did
// not exist.
func (s *FileStore) Delete(ctx context.Context, id int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	doc, err := s.load(ctx)
	if err != nil {
		return false, err
	}
	if _, ok := doc.notes[id]; !ok {
		return false, nil
	}
	delete(doc.notes, id)
	if err := s.save(ctx, doc); err != nil {
		return false, err
	}
	return true, nil
}

// load reads and decodes notes.json. A missing file yields an empty document
// with next_id 1. Corrupt JSON is returned as an error.
func (s *FileStore) load(ctx context.Context) (*document, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return &document{nextID: 1, notes: map[int64]Note{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("note: read %s: %w", s.path, err)
	}
	doc, err := decodeDocument(data)
	if err != nil {
		return nil, fmt.Errorf("note: decode %s: %w", s.path, err)
	}
	return doc, nil
}

func decodeDocument(data []byte) (*document, error) {
	var raw struct {
		NextID *int64          `json:"next_id"`
		Notes  json.RawMessage `json:"notes"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	doc := &document{nextID: 1, notes: map[int64]Note{}}
	// PHP: null++ yields 1, so a missing next_id behaves like 1.
	if raw.NextID != nil {
		doc.nextID = *raw.NextID
	}

	trimmed := bytes.TrimSpace(raw.Notes)
	switch {
	case len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")):
		// No notes.
	case trimmed[0] == '[':
		// PHP encodes an empty (or 0-based sequential) array as a JSON list;
		// list indices are the PHP array keys.
		var list []*Note
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return nil, err
		}
		for i, n := range list {
			if n != nil {
				doc.notes[int64(i)] = *n
			}
		}
	default:
		var obj map[string]Note
		if err := json.Unmarshal(trimmed, &obj); err != nil {
			return nil, err
		}
		for k, n := range obj {
			id, err := strconv.ParseInt(k, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("invalid note key %q: %w", k, err)
			}
			doc.notes[id] = n
		}
	}
	return doc, nil
}

// encodeDocument writes the PHP-compatible format:
// {"next_id":N,"notes":{"1":{...},...}} — or "notes":[] when empty, which is
// what PHP's json_encode emits for an empty array.
func encodeDocument(doc *document) ([]byte, error) {
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
			buf.WriteString(strconv.Quote(strconv.FormatInt(id, 10)))
			buf.WriteByte(':')
			b, err := json.Marshal(doc.notes[id])
			if err != nil {
				return nil, fmt.Errorf("encode note %d: %w", id, err)
			}
			buf.Write(b)
		}
		buf.WriteByte('}')
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// save atomically replaces notes.json (temp file + rename).
func (s *FileStore) save(ctx context.Context, doc *document) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := encodeDocument(doc)
	if err != nil {
		return fmt.Errorf("note: %w", err)
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("note: create dir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, FileName+".tmp-*")
	if err != nil {
		return fmt.Errorf("note: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("note: write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("note: sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("note: close temp file: %w", err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		cleanup()
		return fmt.Errorf("note: chmod temp file: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		cleanup()
		return fmt.Errorf("note: replace %s: %w", s.path, err)
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
