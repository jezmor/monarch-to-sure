// Package sure talks to a Sure server's API and uploads import files as a
// chunked import session.
package sure

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jezmor/monarch-to-sure/go/internal/importer"
)

// API is what the importer needs from Sure. HTTPClient implements it; tests
// use a fake.
type API interface {
	Request(method, path string, body any) ([]byte, error)
	AccountNames() (map[string]bool, error)
	ExistingRecords() (*importer.Existing, error)
}

// HTTPError is a non-success response from Sure.
type HTTPError struct {
	Method, Path string
	Status       int
	Body         string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("%s %s failed with HTTP %d: %s", e.Method, e.Path, e.Status, e.Body)
}

// HTTPClient calls Sure with an API key.
type HTTPClient struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

func NewClient(baseURL, apiKey string) *HTTPClient {
	return &HTTPClient{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		HTTP:    &http.Client{Timeout: 300 * time.Second},
	}
}

func (c *HTTPClient) Request(method, path string, body any) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, c.BaseURL+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Api-Key", c.APIKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, &HTTPError{method, path, resp.StatusCode, string(data)}
	}
	if len(data) == 0 {
		data = []byte("{}")
	}
	return data, nil
}

func (c *HTTPClient) paginated(path, key string) ([]json.RawMessage, error) {
	var items []json.RawMessage
	for page := 1; ; page++ {
		data, err := c.Request("GET", fmt.Sprintf("%s?page=%d&per_page=100", path, page), nil)
		if err != nil {
			return nil, err
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(data, &body); err != nil {
			return nil, err
		}
		var pageItems []json.RawMessage
		if raw, ok := body[key]; ok {
			if err := json.Unmarshal(raw, &pageItems); err != nil {
				return nil, err
			}
		}
		items = append(items, pageItems...)
		var pagination struct {
			TotalPages *int `json:"total_pages"`
		}
		if raw, ok := body["pagination"]; ok {
			_ = json.Unmarshal(raw, &pagination)
		}
		if pagination.TotalPages == nil || page >= *pagination.TotalPages {
			return items, nil
		}
	}
}

func (c *HTTPClient) AccountNames() (map[string]bool, error) {
	items, err := c.paginated("/api/v1/accounts", "accounts")
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, item := range items {
		var account struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(item, &account); err != nil {
			return nil, err
		}
		names[account.Name] = true
	}
	return names, nil
}

// ExistingRecords returns the categories and merchants an import would
// otherwise overwrite: Sure's importer reuses a same-named category or
// merchant and rewrites its color, icon and logo from the import file.
func (c *HTTPClient) ExistingRecords() (*importer.Existing, error) {
	existing := &importer.Existing{Categories: map[string]importer.CategoryLook{}, Merchants: map[string]bool{}}
	items, err := c.paginated("/api/v1/categories", "categories")
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		var category struct {
			Name  string  `json:"name"`
			Color *string `json:"color"`
			Icon  *string `json:"icon"`
		}
		if err := json.Unmarshal(item, &category); err != nil {
			return nil, err
		}
		existing.Categories[category.Name] = importer.CategoryLook{Color: category.Color, Icon: category.Icon}
	}
	data, err := c.Request("GET", "/api/v1/merchants", nil)
	if err != nil {
		return nil, err
	}
	var merchants []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &merchants); err != nil {
		return nil, err
	}
	for _, m := range merchants {
		if m.Type == "FamilyMerchant" {
			existing.Merchants[m.Name] = true
		}
	}
	return existing, nil
}
