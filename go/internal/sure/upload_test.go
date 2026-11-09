package sure

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jezmor/monarch-to-sure/go/internal/importer"
)

type call struct {
	Method, Path string
	Body         map[string]any
}

// FakeAPI stands in for a Sure server.
type FakeAPI struct {
	Names       map[string]bool
	Status      string
	ChunksCount int
	Existing    importer.Existing
	Calls       []call
}

func NewFake() *FakeAPI {
	return &FakeAPI{
		Names: map[string]bool{}, Status: "pending",
		Existing: importer.Existing{Categories: map[string]importer.CategoryLook{}, Merchants: map[string]bool{}},
	}
}

func (f *FakeAPI) AccountNames() (map[string]bool, error)       { return f.Names, nil }
func (f *FakeAPI) ExistingRecords() (*importer.Existing, error) { return &f.Existing, nil }

func (f *FakeAPI) Request(method, path string, body any) ([]byte, error) {
	var decoded map[string]any
	if body != nil {
		data, _ := json.Marshal(body)
		_ = json.Unmarshal(data, &decoded)
	}
	f.Calls = append(f.Calls, call{method, path, decoded})
	switch {
	case method == "POST" && path == "/api/v1/import_sessions":
		return json.Marshal(map[string]any{"data": map[string]any{"id": "s1", "status": f.Status, "chunks_count": f.ChunksCount}})
	case method == "GET":
		return json.Marshal(map[string]any{"data": map[string]any{"id": "s1", "status": "complete", "summary": map[string]any{}}})
	}
	return []byte(`{"data":{}}`), nil
}

func (f *FakeAPI) paths() string {
	var paths []string
	for _, c := range f.Calls {
		paths = append(paths, c.Path)
	}
	return strings.Join(paths, " ")
}

func lines(t *testing.T) []string {
	t.Helper()
	out, err := importer.NDJSONLines([]importer.Record{
		{Type: "Account", Data: importer.NamedData{ID: "monarch-account:Checking", Name: "Checking"}},
		{Type: "Category", Data: importer.NamedData{ID: "monarch-category:Groceries", Name: "Groceries"}},
		{Type: "Merchant", Data: importer.NamedData{ID: "monarch-merchant:Grocer", Name: "Grocer"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func quiet(string) {}

func TestUploadsChunksAndPublishes(t *testing.T) {
	fake := NewFake()
	session, err := Upload(lines(t), fake, false, 0, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if session.Status() != "complete" {
		t.Fatalf("status %s", session.Status())
	}
	create := fake.Calls[0].Body
	if create["type"] != "SureImport" || !strings.HasPrefix(create["client_session_id"].(string), "monarch-to-sure:") {
		t.Fatalf("create %v", create)
	}
	for _, path := range []string{"/api/v1/import_sessions/s1/chunks", "/api/v1/import_sessions/s1/publish"} {
		if !strings.Contains(fake.paths(), path) {
			t.Fatalf("missing %s in %s", path, fake.paths())
		}
	}
}

func TestSameFileReusesSessionID(t *testing.T) {
	first, second := NewFake(), NewFake()
	_, _ = Upload(lines(t), first, false, 0, quiet)
	_, _ = Upload(lines(t), second, false, 0, quiet)
	if first.Calls[0].Body["client_session_id"] != second.Calls[0].Body["client_session_id"] {
		t.Fatal("session IDs differ")
	}
}

func TestSessionIDMatchesPython(t *testing.T) {
	// "monarch-to-sure:" + hashlib.sha256(b"line\n").hexdigest()[:40] in the Python runtime.
	if got := SessionID("line\n"); got != "monarch-to-sure:c73b73af8851e9e91bc6b4dc12e7dace0a2bfb93" {
		t.Fatalf("got %s", got)
	}
}

func TestCompletedSessionIsNotUploadedAgain(t *testing.T) {
	fake := NewFake()
	fake.Status = "complete"
	if _, err := Upload(lines(t), fake, false, 0, quiet); err != nil {
		t.Fatal(err)
	}
	if len(fake.Calls) != 1 {
		t.Fatalf("%d calls", len(fake.Calls))
	}
}

func TestRefusesWhenAccountNamesAlreadyExist(t *testing.T) {
	fake := NewFake()
	fake.Names["Checking"] = true
	if _, err := Upload(lines(t), fake, false, 0, quiet); err == nil || !strings.Contains(err.Error(), "Checking") {
		t.Fatalf("got %v", err)
	}
	fake = NewFake()
	fake.Names["Checking"] = true
	if _, err := Upload(lines(t), fake, true, 0, quiet); err != nil {
		t.Fatal(err)
	}
}

func TestRefusesToRewriteExistingCategoriesOrMerchants(t *testing.T) {
	color, icon := "#123456", "carrot"
	recolor := NewFake()
	recolor.Existing.Categories["Groceries"] = importer.CategoryLook{Color: &color, Icon: &icon}
	merchant := NewFake()
	merchant.Existing.Merchants["Grocer"] = true
	for _, fake := range []*FakeAPI{recolor, merchant} {
		if _, err := Upload(lines(t), fake, false, 0, quiet); err == nil {
			t.Fatal("upload went ahead")
		}
		if strings.Contains(fake.paths(), "/chunks") {
			t.Fatal("chunks sent despite the conflict")
		}
	}
}

func TestResumedSessionSkipsExistingDataChecks(t *testing.T) {
	fake := NewFake()
	fake.Names["Checking"] = true
	fake.Existing.Merchants["Grocer"] = true
	fake.ChunksCount = 1
	if session, err := Upload(lines(t), fake, false, 0, quiet); err != nil || session.Status() != "complete" {
		t.Fatalf("%v %v", session, err)
	}
}

func TestChunksRespectSizeAndKeepEveryLine(t *testing.T) {
	var input []string
	for i := 0; i < 20; i++ {
		input = append(input, strings.Repeat("x", 30)+"\n")
	}
	chunks := ChunkLines(input, 200)
	if len(chunks) < 2 {
		t.Fatal("expected several chunks")
	}
	for _, c := range chunks {
		if len(c) > 200 {
			t.Fatalf("chunk of %d bytes", len(c))
		}
	}
	if strings.Join(chunks, "") != strings.Join(input, "") {
		t.Fatal("chunks lost or reordered lines")
	}
}
