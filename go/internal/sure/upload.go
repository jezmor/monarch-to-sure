package sure

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// DefaultChunkBytes keeps chunks well under Sure's default 10 MB limit.
const DefaultChunkBytes = 4 * 1024 * 1024

// Session is Sure's import session as the API returns it.
type Session map[string]any

func (s Session) Status() string {
	status, _ := s["status"].(string)
	return status
}

func (s Session) ID() string {
	id, _ := s["id"].(string)
	return id
}

// ChunkLines groups lines into chunks of at most maxBytes, never splitting a
// line.
func ChunkLines(lines []string, maxBytes int) []string {
	var chunks []string
	var chunk strings.Builder
	for _, line := range lines {
		if chunk.Len() > 0 && chunk.Len()+len(line) > maxBytes {
			chunks = append(chunks, chunk.String())
			chunk.Reset()
		}
		chunk.WriteString(line)
	}
	if chunk.Len() > 0 {
		chunks = append(chunks, chunk.String())
	}
	return chunks
}

// SessionID is derived from the file, so re-running an interrupted upload of
// the same file resumes that session instead of importing twice.
func SessionID(content string) string {
	sum := sha256.Sum256([]byte(content))
	return "monarch-to-sure:" + hex.EncodeToString(sum[:])[:40]
}

func session(data []byte) (Session, error) {
	var body struct {
		Data Session `json:"data"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, err
	}
	return body.Data, nil
}

type lineRecord struct {
	Type string         `json:"type"`
	Data map[string]any `json:"data"`
}

// Conflicts returns reasons the import would duplicate or rewrite data
// already in Sure.
func Conflicts(lines []string, client API, allowExistingAccounts bool) ([]string, error) {
	var records []lineRecord
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var record lineRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			return nil, err
		}
		records = append(records, record)
	}

	var problems []string
	existingNames, err := client.AccountNames()
	if err != nil {
		return nil, err
	}
	var clashes []string
	for _, r := range records {
		if name, _ := r.Data["name"].(string); r.Type == "Account" && existingNames[name] {
			clashes = append(clashes, name)
		}
	}
	sort.Strings(clashes)
	if len(clashes) > 0 && !allowExistingAccounts {
		problems = append(problems, "Sure already has accounts with these names, so the import may duplicate them: "+
			strings.Join(clashes, ", ")+". Pass --allow-existing-accounts to import anyway.")
	}

	existing, err := client.ExistingRecords()
	if err != nil {
		return nil, err
	}
	var recolored, merchants []string
	for _, r := range records {
		name, _ := r.Data["name"].(string)
		switch r.Type {
		case "Category":
			look, ok := existing.Categories[name]
			if ok && (!sameText(r.Data["color"], look.Color) || !sameText(r.Data["lucide_icon"], look.Icon)) {
				recolored = append(recolored, name)
			}
		case "Merchant":
			if existing.Merchants[name] {
				merchants = append(merchants, name)
			}
		}
	}
	sort.Strings(recolored)
	sort.Strings(merchants)
	if len(recolored)+len(merchants) > 0 {
		problems = append(problems, "The import would change existing categories or merchants ("+
			strings.Join(append(recolored, merchants...), ", ")+
			"). Re-run convert against this server so it can leave them as they are.")
	}
	return problems, nil
}

func sameText(value any, want *string) bool {
	got, ok := value.(string)
	if want == nil {
		return !ok
	}
	return ok && got == *want
}

// Upload sends NDJSON lines to Sure as one import session, publishes it, and
// waits for Sure to finish processing it.
func Upload(lines []string, client API, allowExistingAccounts bool, poll time.Duration, log func(string)) (Session, error) {
	content := strings.Join(lines, "")
	chunks := ChunkLines(lines, DefaultChunkBytes)

	data, err := client.Request("POST", "/api/v1/import_sessions", map[string]any{
		"type":              "SureImport",
		"client_session_id": SessionID(content),
		"expected_chunks":   len(chunks),
	})
	if err != nil {
		return nil, err
	}
	s, err := session(data)
	if err != nil {
		return nil, err
	}

	if s.Status() == "complete" {
		log("This file was already imported; nothing to do.")
		return s, nil
	}

	// A resumed session may already have created some of these records, so the
	// checks only apply before anything has been uploaded.
	if count, _ := s["chunks_count"].(float64); s.Status() == "pending" && count == 0 {
		problems, err := Conflicts(lines, client, allowExistingAccounts)
		if err != nil {
			return nil, err
		}
		if len(problems) > 0 {
			return nil, fmt.Errorf("%s", strings.Join(problems, "\n"))
		}
	}

	if s.Status() != "importing" {
		for i, chunk := range chunks {
			sequence := i + 1
			log(fmt.Sprintf("Uploading chunk %d/%d", sequence, len(chunks)))
			if _, err := client.Request("POST", fmt.Sprintf("/api/v1/import_sessions/%s/chunks", s.ID()), map[string]any{
				"sequence":         sequence,
				"client_chunk_id":  fmt.Sprintf("chunk-%d", sequence),
				"raw_file_content": chunk,
			}); err != nil {
				return nil, err
			}
		}
		if _, err := client.Request("POST", fmt.Sprintf("/api/v1/import_sessions/%s/publish", s.ID()), nil); err != nil {
			return nil, err
		}
	}

	log("Waiting for Sure to process the import")
	for {
		data, err := client.Request("GET", "/api/v1/import_sessions/"+s.ID(), nil)
		if err != nil {
			return nil, err
		}
		if s, err = session(data); err != nil {
			return nil, err
		}
		if s.Status() == "complete" || s.Status() == "failed" {
			return s, nil
		}
		time.Sleep(poll)
	}
}
