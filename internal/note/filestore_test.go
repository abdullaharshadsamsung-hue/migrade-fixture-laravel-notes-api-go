package note

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func strptr(s string) *string { return &s }

func rawStr(t *testing.T, s string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func newStore(t *testing.T) (*FileStore, string) {
	t.Helper()
	dir := t.TempDir()
	return NewFileStore(dir), filepath.Join(dir, FileName)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

type fileDoc struct {
	NextID int64                      `json:"next_id"`
	Notes  map[string]json.RawMessage `json:"notes"`
}

func parseFile(t *testing.T, path string) fileDoc {
	t.Helper()
	data := readFile(t, path)
	var raw struct {
		NextID int64           `json:"next_id"`
		Notes  json.RawMessage `json:"notes"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal file: %v (%s)", err, data)
	}
	d := fileDoc{NextID: raw.NextID, Notes: map[string]json.RawMessage{}}
	s := strings.TrimSpace(string(raw.Notes))
	if s == "[]" {
		return d
	}
	if err := json.Unmarshal(raw.Notes, &d.Notes); err != nil {
		t.Fatalf("unmarshal notes: %v (%s)", err, data)
	}
	return d
}

func notesEqual(a, b Note) bool {
	if a.ID != b.ID || a.Title != b.Title {
		return false
	}
	var av, bv interface{}
	if err := json.Unmarshal(a.Body, &av); err != nil {
		return false
	}
	if err := json.Unmarshal(b.Body, &bv); err != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

func mustCreate(t *testing.T, s *FileStore, title, body string) Note {
	t.Helper()
	n, err := s.Create(context.Background(), title, body)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return n
}

// ---------- All ----------

func TestAll(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name      string
		setup     func(t *testing.T, s *FileStore)
		wantIDs   []int64
		wantTitle []string
	}{
		{
			name:    "missing file returns empty list",
			setup:   func(t *testing.T, s *FileStore) {},
			wantIDs: []int64{},
		},
		{
			name: "notes in ascending id order",
			setup: func(t *testing.T, s *FileStore) {
				mustCreate(t, s, "a", "1")
				mustCreate(t, s, "b", "2")
				mustCreate(t, s, "c", "3")
			},
			wantIDs:   []int64{1, 2, 3},
			wantTitle: []string{"a", "b", "c"},
		},
		{
			name: "order stays by id after update",
			setup: func(t *testing.T, s *FileStore) {
				mustCreate(t, s, "a", "1")
				mustCreate(t, s, "b", "2")
				if _, _, err := s.Update(ctx, 1, Patch{Title: strptr("A")}); err != nil {
					t.Fatal(err)
				}
			},
			wantIDs:   []int64{1, 2},
			wantTitle: []string{"A", "b"},
		},
		{
			name: "all created then deleted returns empty",
			setup: func(t *testing.T, s *FileStore) {
				mustCreate(t, s, "a", "1")
				mustCreate(t, s, "b", "2")
				for _, id := range []int64{1, 2} {
					if ok, err := s.Delete(ctx, id); err != nil || !ok {
						t.Fatalf("Delete(%d) = %v, %v", id, ok, err)
					}
				}
			},
			wantIDs: []int64{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := newStore(t)
			tt.setup(t, s)
			got, err := s.All(ctx)
			if err != nil {
				t.Fatalf("All: %v", err)
			}
			if got == nil {
				t.Fatal("All returned nil slice")
			}
			if len(got) != len(tt.wantIDs) {
				t.Fatalf("len = %d, want %d", len(got), len(tt.wantIDs))
			}
			for i, n := range got {
				if n.ID != tt.wantIDs[i] {
					t.Errorf("got[%d].ID = %d, want %d", i, n.ID, tt.wantIDs[i])
				}
				if tt.wantTitle != nil && n.Title != tt.wantTitle[i] {
					t.Errorf("got[%d].Title = %q, want %q", i, n.Title, tt.wantTitle[i])
				}
			}
		})
	}
}

func TestAllEmptyEncodesAsJSONList(t *testing.T) {
	s, _ := newStore(t)
	got, err := s.All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got)
	if string(b) != "[]" {
		t.Errorf("json = %s, want []", b)
	}
}

func TestReadOperationsDoNotCreateFile(t *testing.T) {
	ctx := context.Background()
	ops := []struct {
		name string
		op   func(s *FileStore) error
	}{
		{"All", func(s *FileStore) error { _, err := s.All(ctx); return err }},
		{"Find", func(s *FileStore) error { _, _, err := s.Find(ctx, 1); return err }},
		{"Update missing", func(s *FileStore) error {
			_, ok, err := s.Update(ctx, 1, Patch{Title: strptr("x")})
			if ok {
				return errors.New("unexpected ok")
			}
			return err
		}},
		{"Delete missing", func(s *FileStore) error {
			ok, err := s.Delete(ctx, 1)
			if ok {
				return errors.New("unexpected ok")
			}
			return err
		}},
	}
	for _, tt := range ops {
		t.Run(tt.name, func(t *testing.T) {
			s, path := newStore(t)
			if err := tt.op(s); err != nil {
				t.Fatalf("op: %v", err)
			}
			if fileExists(path) {
				t.Error("file was created")
			}
			entries, _ := os.ReadDir(filepath.Dir(path))
			if len(entries) != 0 {
				t.Errorf("dir has %d entries, want 0", len(entries))
			}
		})
	}
}

func TestReadOperationsDoNotModifyFile(t *testing.T) {
	ctx := context.Background()
	s, path := newStore(t)
	mustCreate(t, s, "a", "b")
	before := readFile(t, path)
	st, _ := os.Stat(path)

	if _, err := s.All(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Find(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Find(ctx, 99); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.Update(ctx, 99, Patch{Title: strptr("x")}); err != nil || ok {
		t.Fatalf("Update missing = %v, %v", ok, err)
	}
	if ok, err := s.Delete(ctx, 99); err != nil || ok {
		t.Fatalf("Delete missing = %v, %v", ok, err)
	}
	after := readFile(t, path)
	if string(before) != string(after) {
		t.Errorf("file changed:\n%s\n%s", before, after)
	}
	st2, _ := os.Stat(path)
	if !st.ModTime().Equal(st2.ModTime()) {
		t.Error("modtime changed")
	}
}

// ---------- Corrupt storage ----------

func TestCorruptFileReturnsErrors(t *testing.T) {
	ctx := context.Background()
	contents := []struct {
		name string
		data string
	}{
		{"invalid json", "{not json"},
		{"empty file", ""},
		{"notes wrong type", `{"next_id":2,"notes":"oops"}`},
		{"non-numeric key", `{"next_id":2,"notes":{"abc":{"id":1,"title":"t","body":"b"}}}`},
	}
	ops := []struct {
		name string
		op   func(s *FileStore) error
	}{
		{"All", func(s *FileStore) error { _, err := s.All(ctx); return err }},
		{"Find", func(s *FileStore) error { _, _, err := s.Find(ctx, 1); return err }},
		{"Create", func(s *FileStore) error { _, err := s.Create(ctx, "t", "b"); return err }},
		{"Update", func(s *FileStore) error { _, _, err := s.Update(ctx, 1, Patch{}); return err }},
		{"Delete", func(s *FileStore) error { _, err := s.Delete(ctx, 1); return err }},
	}
	for _, c := range contents {
		for _, o := range ops {
			t.Run(c.name+"/"+o.name, func(t *testing.T) {
				s, path := newStore(t)
				if err := os.WriteFile(path, []byte(c.data), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := o.op(s); err == nil {
					t.Fatal("expected error, got nil")
				}
				if got := string(readFile(t, path)); got != c.data {
					t.Errorf("corrupt file was modified: %q", got)
				}
			})
		}
	}
}

func TestAllReturnsNilOnError(t *testing.T) {
	s, path := newStore(t)
	if err := os.WriteFile(path, []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := s.All(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

// ---------- Find ----------

func TestFind(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name      string
		setup     func(t *testing.T, s *FileStore)
		id        int64
		wantOK    bool
		wantTitle string
		wantBody  string
	}{
		{
			name:   "missing file",
			setup:  func(t *testing.T, s *FileStore) {},
			id:     1,
			wantOK: false,
		},
		{
			name: "existing note",
			setup: func(t *testing.T, s *FileStore) {
				mustCreate(t, s, "first", "body1")
				mustCreate(t, s, "second", "body2")
			},
			id:        2,
			wantOK:    true,
			wantTitle: "second",
			wantBody:  "body2",
		},
		{
			name: "never created id",
			setup: func(t *testing.T, s *FileStore) {
				mustCreate(t, s, "first", "body1")
			},
			id:     42,
			wantOK: false,
		},
		{
			name: "deleted note",
			setup: func(t *testing.T, s *FileStore) {
				mustCreate(t, s, "first", "body1")
				if ok, err := s.Delete(ctx, 1); err != nil || !ok {
					t.Fatalf("Delete = %v, %v", ok, err)
				}
			},
			id:     1,
			wantOK: false,
		},
		{
			name:   "zero id",
			setup:  func(t *testing.T, s *FileStore) { mustCreate(t, s, "x", "y") },
			id:     0,
			wantOK: false,
		},
		{
			name:   "negative id",
			setup:  func(t *testing.T, s *FileStore) { mustCreate(t, s, "x", "y") },
			id:     -1,
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := newStore(t)
			tt.setup(t, s)
			n, ok, err := s.Find(ctx, tt.id)
			if err != nil {
				t.Fatalf("Find: %v", err)
			}
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				if !reflect.DeepEqual(n, Note{}) {
					t.Errorf("expected zero Note, got %+v", n)
				}
				return
			}
			if n.ID != tt.id || n.Title != tt.wantTitle {
				t.Errorf("got %+v", n)
			}
			var body string
			if err := json.Unmarshal(n.Body, &body); err != nil || body != tt.wantBody {
				t.Errorf("body = %s, want %q", n.Body, tt.wantBody)
			}
		})
	}
}

func TestFindUsesStorageKeyNotIDField(t *testing.T) {
	s, path := newStore(t)
	data := `{"next_id":3,"notes":{"1":{"id":99,"title":"t","body":"b"}}}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	n, ok, err := s.Find(ctx, 1)
	if err != nil || !ok {
		t.Fatalf("Find(1) = %v, %v", ok, err)
	}
	if n.ID != 99 {
		t.Errorf("ID field = %d, want 99 (overwritten value)", n.ID)
	}
	if _, ok, _ := s.Find(ctx, 99); ok {
		t.Error("Find(99) should not find note stored under key 1")
	}
}

func TestFindReturnsExtraFieldsBody(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	mustCreate(t, s, "t", "b")
	newBody := json.RawMessage(`{"nested":[1,2,3]}`)
	if _, ok, err := s.Update(ctx, 1, Patch{Body: newBody}); err != nil || !ok {
		t.Fatalf("Update = %v, %v", ok, err)
	}
	n, ok, err := s.Find(ctx, 1)
	if err != nil || !ok {
		t.Fatalf("Find = %v, %v", ok, err)
	}
	if !notesEqual(n, Note{ID: 1, Title: "t", Body: newBody}) {
		t.Errorf("got %+v", n)
	}
}

// ---------- Create ----------

func TestCreateFirstWhenNoFile(t *testing.T) {
	s, path := newStore(t)
	n := mustCreate(t, s, "hello", "world")
	want := Note{ID: 1, Title: "hello", Body: rawStr(t, "world")}
	if !notesEqual(n, want) {
		t.Errorf("got %+v, want %+v", n, want)
	}
	d := parseFile(t, path)
	if d.NextID != 2 {
		t.Errorf("next_id = %d, want 2", d.NextID)
	}
	raw, ok := d.Notes["1"]
	if !ok {
		t.Fatalf("note not stored under key 1: %v", d.Notes)
	}
	var stored map[string]interface{}
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	wantStored := map[string]interface{}{"id": float64(1), "title": "hello", "body": "world"}
	if !reflect.DeepEqual(stored, wantStored) {
		t.Errorf("stored = %v, want %v", stored, wantStored)
	}
}

func TestCreateSequence(t *testing.T) {
	tests := []struct {
		name   string
		inputs [][2]string
	}{
		{"single", [][2]string{{"a", "b"}}},
		{"several", [][2]string{{"a", "1"}, {"b", "2"}, {"c", "3"}, {"d", "4"}}},
		{"empty strings", [][2]string{{"", ""}, {"", ""}}},
		{"duplicate titles", [][2]string{{"same", "x"}, {"same", "y"}}},
		{"special characters", [][2]string{{`quo"te`, "line\nbreak <&> ünï"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			s, path := newStore(t)
			for i, in := range tt.inputs {
				n := mustCreate(t, s, in[0], in[1])
				wantID := int64(i + 1)
				want := Note{ID: wantID, Title: in[0], Body: rawStr(t, in[1])}
				if !notesEqual(n, want) {
					t.Errorf("create %d = %+v, want %+v", i, n, want)
				}
				found, ok, err := s.Find(ctx, wantID)
				if err != nil || !ok {
					t.Fatalf("Find(%d) = %v, %v", wantID, ok, err)
				}
				if !notesEqual(found, n) {
					t.Errorf("Find = %+v, Create returned %+v", found, n)
				}
				if d := parseFile(t, path); d.NextID != wantID+1 {
					t.Errorf("next_id = %d, want %d", d.NextID, wantID+1)
				}
			}
			all, err := s.All(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(all) != len(tt.inputs) {
				t.Errorf("All len = %d, want %d", len(all), len(tt.inputs))
			}
		})
	}
}

func TestCreateReturnedNoteHasExactlyIDTitleBody(t *testing.T) {
	s, _ := newStore(t)
	n := mustCreate(t, s, "t", "b")
	b, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	want := map[string]interface{}{"id": float64(1), "title": "t", "body": "b"}
	if !reflect.DeepEqual(m, want) {
		t.Errorf("got %v, want %v", m, want)
	}
}

func TestCreateIDsNeverReused(t *testing.T) {
	tests := []struct {
		name     string
		create   int
		deleteID []int64
		wantNext int64
	}{
		{"delete last", 3, []int64{3}, 4},
		{"delete all", 2, []int64{1, 2}, 3},
		{"delete middle", 3, []int64{2}, 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			s, _ := newStore(t)
			var maxID int64
			for i := 0; i < tt.create; i++ {
				n := mustCreate(t, s, "t", "b")
				if n.ID <= maxID {
					t.Fatalf("ids not strictly increasing: %d after %d", n.ID, maxID)
				}
				maxID = n.ID
			}
			for _, id := range tt.deleteID {
				if ok, err := s.Delete(ctx, id); err != nil || !ok {
					t.Fatalf("Delete(%d) = %v, %v", id, ok, err)
				}
			}
			n := mustCreate(t, s, "new", "n")
			if n.ID != tt.wantNext || n.ID <= maxID {
				t.Errorf("new id = %d, want %d (> %d)", n.ID, tt.wantNext, maxID)
			}
		})
	}
}

func TestCreateCreatesMissingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b", "private")
	s := NewFileStore(dir)
	mustCreate(t, s, "t", "b")
	if !fileExists(filepath.Join(dir, FileName)) {
		t.Error("notes.json not created")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("unexpected leftover files: %v", entries)
	}
}

func TestCreateWithLegacyListAndMissingNextID(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name    string
		data    string
		wantID  int64
		wantLen int
	}{
		{"empty list, explicit next_id", `{"next_id":5,"notes":[]}`, 5, 1},
		{"missing next_id defaults to 1", `{"notes":[]}`, 1, 1},
		{"null notes", `{"next_id":3,"notes":null}`, 3, 1},
		{"object notes", `{"next_id":4,"notes":{"2":{"id":2,"title":"x","body":"y"}}}`, 4, 2},
		{"list notes index as key", `{"next_id":2,"notes":[{"id":0,"title":"z","body":"q"}]}`, 2, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, path := newStore(t)
			if err := os.WriteFile(path, []byte(tt.data), 0o644); err != nil {
				t.Fatal(err)
			}
			n := mustCreate(t, s, "t", "b")
			if n.ID != tt.wantID {
				t.Errorf("id = %d, want %d", n.ID, tt.wantID)
			}
			all, err := s.All(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(all) != tt.wantLen {
				t.Errorf("len = %d, want %d", len(all), tt.wantLen)
			}
		})
	}
}

// ---------- Update ----------

func TestUpdate(t *testing.T) {
	tests := []struct {
		name      string
		id        int64
		patch     Patch
		wantOK    bool
		wantTitle string
		wantBody  string
	}{
		{"title only", 1, Patch{Title: strptr("new")}, true, "new", `"b1"`},
		{"body only", 1, Patch{Body: json.RawMessage(`"nb"`)}, true, "t1", `"nb"`},
		{"both", 1, Patch{Title: strptr("T"), Body: json.RawMessage(`"B"`)}, true, "T", `"B"`},
		{"body non-string JSON", 1, Patch{Body: json.RawMessage(`{"k":1}`)}, true, "t1", `{"k":1}`},
		{"body null JSON", 1, Patch{Body: json.RawMessage(`null`)}, true, "t1", `null`},
		{"empty title", 1, Patch{Title: strptr("")}, true, "", `"b1"`},
		{"empty patch", 1, Patch{}, true, "t1", `"b1"`},
		{"missing note", 99, Patch{Title: strptr("x")}, false, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			s, path := newStore(t)
			mustCreate(t, s, "t1", "b1")
			other := mustCreate(t, s, "t2", "b2")
			before := parseFile(t, path)

			n, ok, err := s.Update(ctx, tt.id, tt.patch)
			if err != nil {
				t.Fatalf("Update: %v", err)
			}
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			after := parseFile(t, path)
			if after.NextID != before.NextID {
				t.Errorf("next_id changed %d -> %d", before.NextID, after.NextID)
			}
			if !ok {
				if !reflect.DeepEqual(n, Note{}) {
					t.Errorf("expected zero Note, got %+v", n)
				}
				if _, found, _ := s.Find(ctx, tt.id); found {
					t.Error("update created a note")
				}
				if len(after.Notes) != 2 {
					t.Errorf("notes count = %d", len(after.Notes))
				}
				return
			}
			want := Note{ID: tt.id, Title: tt.wantTitle, Body: json.RawMessage(tt.wantBody)}
			if !notesEqual(n, want) {
				t.Errorf("returned %+v (body %s), want %+v", n, n.Body, want)
			}
			found, _, err := s.Find(ctx, tt.id)
			if err != nil {
				t.Fatal(err)
			}
			if !notesEqual(found, want) {
				t.Errorf("persisted %+v (body %s), want %+v", found, found.Body, want)
			}
			o, _, _ := s.Find(ctx, other.ID)
			if !notesEqual(o, other) {
				t.Errorf("other note changed: %+v", o)
			}
		})
	}
}

func TestUpdateEmptyPatchStillRewritesFile(t *testing.T) {
	ctx := context.Background()
	s, path := newStore(t)
	mustCreate(t, s, "t", "b")
	// Replace with a legal but differently formatted document.
	alt := "{ \"next_id\" : 2 , \"notes\" : { \"1\" : {\"id\":1,\"title\":\"t\",\"body\":\"b\"} } }"
	if err := os.WriteFile(path, []byte(alt), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.Update(ctx, 1, Patch{}); err != nil || !ok {
		t.Fatalf("Update = %v, %v", ok, err)
	}
	if got := string(readFile(t, path)); got == alt {
		t.Error("file was not rewritten")
	}
}

func TestUpdateInvalidBody(t *testing.T) {
	ctx := context.Background()
	s, path := newStore(t)
	mustCreate(t, s, "t", "b")
	before := readFile(t, path)
	_, ok, err := s.Update(ctx, 1, Patch{Title: strptr("x"), Body: json.RawMessage(`{bad`)})
	if err == nil || ok {
		t.Fatalf("Update = %v, %v; want error", ok, err)
	}
	if string(readFile(t, path)) != string(before) {
		t.Error("file modified on invalid body")
	}
}

func TestUpdateCopiesBody(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	mustCreate(t, s, "t", "b")
	body := json.RawMessage(`"abc"`)
	n, _, err := s.Update(ctx, 1, Patch{Body: body})
	if err != nil {
		t.Fatal(err)
	}
	body[1] = 'z'
	if string(n.Body) != `"abc"` {
		t.Errorf("returned body aliases input: %s", n.Body)
	}
}

func TestUpdateIDFieldOverwrittenStaysUnderOriginalKey(t *testing.T) {
	ctx := context.Background()
	s, path := newStore(t)
	data := `{"next_id":2,"notes":{"1":{"id":1,"title":"t","body":"b"}}}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	// Simulate a note whose id field was changed by a prior update.
	data = `{"next_id":2,"notes":{"1":{"id":77,"title":"t","body":"b"}}}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	n, ok, err := s.Update(ctx, 1, Patch{Title: strptr("u")})
	if err != nil || !ok {
		t.Fatalf("Update = %v, %v", ok, err)
	}
	if n.ID != 77 || n.Title != "u" {
		t.Errorf("got %+v", n)
	}
	d := parseFile(t, path)
	if _, ok := d.Notes["1"]; !ok {
		t.Errorf("note not under original key: %v", d.Notes)
	}
	if _, ok := d.Notes["77"]; ok {
		t.Error("note moved to new key")
	}
}

// ---------- Delete ----------

func TestDelete(t *testing.T) {
	tests := []struct {
		name      string
		creates   int
		deletes   []int64
		wantOK    []bool
		remaining []int64
	}{
		{"existing", 2, []int64{1}, []bool{true}, []int64{2}},
		{"never existed", 1, []int64{5}, []bool{false}, []int64{1}},
		{"twice", 2, []int64{2, 2}, []bool{true, false}, []int64{1}},
		{"all", 3, []int64{1, 2, 3}, []bool{true, true, true}, []int64{}},
		{"middle keeps others", 3, []int64{2}, []bool{true}, []int64{1, 3}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			s, path := newStore(t)
			created := map[int64]Note{}
			for i := 0; i < tt.creates; i++ {
				n := mustCreate(t, s, "t", "b")
				created[n.ID] = n
			}
			nextBefore := parseFile(t, path).NextID
			for i, id := range tt.deletes {
				ok, err := s.Delete(ctx, id)
				if err != nil {
					t.Fatalf("Delete(%d): %v", id, err)
				}
				if ok != tt.wantOK[i] {
					t.Errorf("Delete(%d) = %v, want %v", id, ok, tt.wantOK[i])
				}
				if _, found, _ := s.Find(ctx, id); found {
					t.Errorf("Find(%d) after delete found note", id)
				}
			}
			d := parseFile(t, path)
			if d.NextID != nextBefore {
				t.Errorf("next_id changed %d -> %d", nextBefore, d.NextID)
			}
			if len(tt.remaining) == 0 && strings.TrimSpace(string(readFile(t, path))) != `{"next_id":`+jsonInt(nextBefore)+`,"notes":[]}` {
				t.Errorf("empty file format = %s", readFile(t, path))
			}
			all, err := s.All(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(all) != len(tt.remaining) {
				t.Fatalf("remaining len = %d, want %d", len(all), len(tt.remaining))
			}
			for i, id := range tt.remaining {
				if !notesEqual(all[i], created[id]) {
					t.Errorf("remaining[%d] = %+v, want %+v", i, all[i], created[id])
				}
			}
		})
	}
}

func jsonInt(i int64) string {
	b, _ := json.Marshal(i)
	return string(b)
}

func TestDeleteMissingFile(t *testing.T) {
	s, path := newStore(t)
	ok, err := s.Delete(context.Background(), 1)
	if err != nil || ok {
		t.Fatalf("Delete = %v, %v", ok, err)
	}
	if fileExists(path) {
		t.Error("file created")
	}
}

// ---------- Persistence / format ----------

func TestPersistsAcrossInstances(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s1 := NewFileStore(dir)
	n1 := mustCreate(t, s1, "a", "b")

	s2 := NewFileStore(dir)
	got, ok, err := s2.Find(ctx, n1.ID)
	if err != nil || !ok || !notesEqual(got, n1) {
		t.Fatalf("Find = %+v, %v, %v", got, ok, err)
	}
	n2 := mustCreate(t, s2, "c", "d")
	if n2.ID != 2 {
		t.Errorf("id = %d, want 2", n2.ID)
	}
}

func TestFileFormat(t *testing.T) {
	ctx := context.Background()
	s, path := newStore(t)
	mustCreate(t, s, "a", "x")
	mustCreate(t, s, "b", "y")
	want := `{"next_id":3,"notes":{"1":{"id":1,"title":"a","body":"x"},"2":{"id":2,"title":"b","body":"y"}}}`
	if got := string(readFile(t, path)); got != want {
		t.Errorf("file =\n%s\nwant\n%s", got, want)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o644 {
		t.Errorf("perm = %v, want 0644", st.Mode().Perm())
	}
	if _, err := s.Delete(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Delete(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if got := string(readFile(t, path)); got != `{"next_id":3,"notes":[]}` {
		t.Errorf("empty file = %s", got)
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		doc  *document
	}{
		{"empty", &document{nextID: 1, notes: map[int64]Note{}}},
		{"some", &document{nextID: 10, notes: map[int64]Note{
			3: {ID: 3, Title: "c", Body: json.RawMessage(`"x"`)},
			1: {ID: 1, Title: "a", Body: json.RawMessage(`{"k":[1]}`)},
		}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := encodeDocument(tt.doc)
			if err != nil {
				t.Fatal(err)
			}
			got, err := decodeDocument(b)
			if err != nil {
				t.Fatalf("decode: %v (%s)", err, b)
			}
			if got.nextID != tt.doc.nextID || len(got.notes) != len(tt.doc.notes) {
				t.Fatalf("got %+v", got)
			}
			for id, n := range tt.doc.notes {
				if !notesEqual(got.notes[id], n) {
					t.Errorf("note %d = %+v, want %+v", id, got.notes[id], n)
				}
			}
		})
	}
}

func TestDecodeDocument(t *testing.T) {
	tests := []struct {
		name     string
		data     string
		wantErr  bool
		wantNext int64
		wantIDs  []int64
	}{
		{"missing next_id", `{"notes":{}}`, false, 1, nil},
		{"null notes", `{"next_id":4,"notes":null}`, false, 4, nil},
		{"missing notes", `{"next_id":4}`, false, 4, nil},
		{"list with null hole", `{"next_id":3,"notes":[null,{"id":1,"title":"t","body":"b"}]}`, false, 3, []int64{1}},
		{"object", `{"next_id":3,"notes":{"2":{"id":2,"title":"t","body":"b"}}}`, false, 3, []int64{2}},
		{"invalid", `nope`, true, 0, nil},
		{"bad key", `{"notes":{"x":{}}}`, true, 0, nil},
		{"bad list", `{"notes":[1]}`, true, 0, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := decodeDocument([]byte(tt.data))
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if d.nextID != tt.wantNext {
				t.Errorf("nextID = %d, want %d", d.nextID, tt.wantNext)
			}
			if got := sortedIDs(d.notes); len(got) != len(tt.wantIDs) || (len(got) > 0 && !reflect.DeepEqual(got, tt.wantIDs)) {
				t.Errorf("ids = %v, want %v", got, tt.wantIDs)
			}
		})
	}
}

// ---------- Context ----------

func TestCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ops := []struct {
		name string
		op   func(s *FileStore) error
	}{
		{"All", func(s *FileStore) error { _, err := s.All(ctx); return err }},
		{"Find", func(s *FileStore) error { _, _, err := s.Find(ctx, 1); return err }},
		{"Create", func(s *FileStore) error { _, err := s.Create(ctx, "t", "b"); return err }},
		{"Update", func(s *FileStore) error { _, _, err := s.Update(ctx, 1, Patch{}); return err }},
		{"Delete", func(s *FileStore) error { _, err := s.Delete(ctx, 1); return err }},
	}
	for _, tt := range ops {
		t.Run(tt.name, func(t *testing.T) {
			s, path := newStore(t)
			if err := tt.op(s); !errors.Is(err, context.Canceled) {
				t.Errorf("err = %v, want context.Canceled", err)
			}
			if fileExists(path) {
				t.Error("file written with canceled context")
			}
		})
	}
}

// ---------- Concurrency (in-process lock) ----------

func TestConcurrentCreatesUniqueIDs(t *testing.T) {
	ctx := context.Background()
	s, path := newStore(t)
	const n = 30
	var wg sync.WaitGroup
	ids := make(chan int64, n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			note, err := s.Create(ctx, "t", "b")
			if err != nil {
				errs <- err
				return
			}
			ids <- note.ID
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Fatalf("Create: %v", err)
	}
	seen := map[int64]bool{}
	for id := range ids {
		if seen[id] {
			t.Errorf("duplicate id %d", id)
		}
		seen[id] = true
	}
	if len(seen) != n {
		t.Errorf("got %d ids, want %d", len(seen), n)
	}
	if d := parseFile(t, path); d.NextID != n+1 || len(d.Notes) != n {
		t.Errorf("next_id=%d notes=%d", d.NextID, len(d.Notes))
	}
}

// ---------- Interface usage ----------

func TestStoreInterface(t *testing.T) {
	var st Store = NewFileStore(t.TempDir())
	ctx := context.Background()
	n, err := st.Create(ctx, "t", "b")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := st.Find(ctx, n.ID); err != nil || !ok {
		t.Fatalf("Find = %v, %v", ok, err)
	}
}