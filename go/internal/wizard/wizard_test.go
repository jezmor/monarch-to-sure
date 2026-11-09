package wizard

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jezmor/monarch-to-sure/go/internal/importer"
	"github.com/jezmor/monarch-to-sure/go/internal/sure"
)

const balancesCSV = `Date,Balance,Account
2024-01-01,100.00,Checking
2024-01-02,90.00,Checking
2024-01-03,95.00,Checking
2024-01-04,95.00,Checking
2024-01-01,-50.00,Card
2024-01-02,-70.00,Card
2024-01-03,-70.00,Card
2024-01-04,-70.00,Card
2024-01-01,10.00,Old Savings
`

const transactionsCSV = `Date,Merchant,Category,Account,Original Statement,Notes,Amount,Tags,Owner,Reviewed,Reviewed At,Reviewed By,Pending,Id
2024-01-02,Grocer,Groceries,Checking,GROCER #12,weekly shop,-10.00,Food,Me,true,,Me,false,1
2024-01-04,Employer,Paychecks,Checking,PAYROLL,,7.00,,Me,false,,,false,2
2024-01-02,Cafe,Restaurants,Card,CAFE,,-20.00,"Food, Treats",Me,false,,,false,3
2024-01-04,Payment,Credit Card Payment,Checking,CC PMT,,-1.00,,Me,false,,,false,4
2024-01-04,Later,Groceries,Checking,LATER,,-3.00,,Me,false,,,true,5
`

type fakeAPI struct {
	names  map[string]bool
	apiKey string
	chunks []string
}

func (f *fakeAPI) AccountNames() (map[string]bool, error) {
	if f.apiKey == "bad" {
		return nil, &sure.HTTPError{Method: "GET", Path: "/api/v1/accounts", Status: 401}
	}
	return f.names, nil
}

func (f *fakeAPI) ExistingRecords() (*importer.Existing, error) {
	return &importer.Existing{Categories: map[string]importer.CategoryLook{}, Merchants: map[string]bool{}}, nil
}

func (f *fakeAPI) Request(method, path string, body any) ([]byte, error) {
	if strings.HasSuffix(path, "/chunks") {
		f.chunks = append(f.chunks, body.(map[string]any)["raw_file_content"].(string))
	}
	if method == "POST" && path == "/api/v1/import_sessions" {
		return []byte(`{"data":{"id":"s1","status":"pending","chunks_count":0}}`), nil
	}
	return []byte(`{"data":{"id":"s1","status":"complete","summary":{}}}`), nil
}

func newWizard(names ...string) (*Wizard, *fakeAPI) {
	fake := &fakeAPI{names: map[string]bool{}}
	for _, n := range names {
		fake.names[n] = true
	}
	w := New()
	w.Poll = 0
	w.NewClient = func(_, apiKey string) sure.API { fake.apiKey = apiKey; return fake }
	return w, fake
}

func loaded(t *testing.T, names ...string) (*Wizard, *fakeAPI, *FilesResult) {
	t.Helper()
	w, fake := newWizard(names...)
	if _, err := w.Connect("http://sure.test/", "key"); err != nil {
		t.Fatal(err)
	}
	transactions := transactionsCSV
	files, err := w.LoadFiles(balancesCSV, &transactions)
	if err != nil {
		t.Fatal(err)
	}
	return w, fake, files
}

func choicesFrom(files *FilesResult, change func(name string, c *Choice)) map[string]Choice {
	choices := map[string]Choice{}
	for _, a := range files.Accounts {
		c := Choice{Type: a.Type, Closed: a.SuggestClosed}
		if change != nil {
			change(a.Name, &c)
		}
		choices[a.Name] = c
	}
	return choices
}

func wait(t *testing.T, w *Wizard) *job {
	t.Helper()
	for i := 0; i < 200; i++ {
		if status := w.Status(); status.State != "running" {
			return status
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("import didn't finish")
	return nil
}

func ptr[T any](v T) *T { return &v }

func TestConnectChecksURLAndKey(t *testing.T) {
	w, _ := newWizard()
	var userError *UserError
	if _, err := w.Connect("sure.test", "key"); !errors.As(err, &userError) {
		t.Fatalf("got %v", err)
	}
	if _, err := w.Connect("http://sure.test", "bad"); err == nil || !strings.Contains(err.Error(), "rejected the API key") {
		t.Fatalf("got %v", err)
	}
	result, err := w.Connect("http://sure.test/", "key")
	if err != nil || result.URL != "http://sure.test" {
		t.Fatalf("%v %v", result, err)
	}
}

func TestFilesListAccountsWithSuggestions(t *testing.T) {
	_, _, files := loaded(t)
	if files.BalancesOnly || len(files.Accounts) != 3 || files.Accounts[2].Name != "Old Savings" {
		t.Fatalf("%+v", files)
	}
	for _, a := range files.Accounts {
		if a.Name == "Checking" && a.Transactions != 4 {
			t.Fatalf("Checking has %d transactions", a.Transactions)
		}
	}
}

func TestWrongFilesAreRejected(t *testing.T) {
	w, _ := newWizard()
	if _, err := w.LoadFiles(transactionsCSV, nil); err == nil || !strings.Contains(err.Error(), "balances file is missing") {
		t.Fatalf("got %v", err)
	}
	balances := balancesCSV
	if _, err := w.LoadFiles(balancesCSV, &balances); err == nil || !strings.Contains(err.Error(), "transactions file is missing") {
		t.Fatalf("got %v", err)
	}
	files, err := w.LoadFiles(balancesCSV, nil)
	if err != nil || !files.BalancesOnly {
		t.Fatalf("%v %v", files, err)
	}
}

func TestChoicesBecomeTheConfig(t *testing.T) {
	w, _, files := loaded(t)
	config, err := w.configFrom(choicesFrom(files, func(name string, c *Choice) {
		switch name {
		case "Card":
			c.Type, c.Closed = "Loan", true
		case "Checking":
			c.Transactions, c.Balance = ptr(false), ptr(" 120 ")
		case "Old Savings":
			c.Include = ptr(false)
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	card, checking := config.Account("Card"), config.Account("Checking")
	if card.Type != "Loan" || card.Status != "disabled" || checking.Transactions || checking.CurrentBalance.Text != "120" || config.Account("Old Savings").Include {
		t.Fatalf("%+v", config)
	}
	if _, err := w.configFrom(choicesFrom(files, func(_ string, c *Choice) { c.Type = "Boat" })); err == nil {
		t.Fatal("accepted an invalid type")
	}
}

func TestPreviewCountsAndFlagsExistingAccounts(t *testing.T) {
	w, _, files := loaded(t, "Checking")
	preview, err := w.Preview(choicesFrom(files, func(name string, c *Choice) {
		if name == "Old Savings" {
			c.Include = ptr(false)
		}
	}), false)
	if err != nil {
		t.Fatal(err)
	}
	got := fmt.Sprint(preview.Stats["Account"], preview.Stats["Transaction"], preview.ExistingAccountNames, preview.SkippedAccounts)
	if got != "2 4 [Checking] [Old Savings]" {
		t.Fatalf("got %s", got)
	}
}

func TestImportUploadsThePreviewedRecords(t *testing.T) {
	w, fake, files := loaded(t)
	if _, err := w.Preview(choicesFrom(files, nil), false); err != nil {
		t.Fatal(err)
	}
	if _, err := w.StartImport(false); err != nil {
		t.Fatal(err)
	}
	if status := wait(t, w); status.State != "complete" {
		t.Fatalf("%+v", status)
	}
	lines, _ := importer.NDJSONLines(w.records)
	if strings.Join(fake.chunks, "") != strings.Join(lines, "") {
		t.Fatal("uploaded content differs from the preview")
	}
}

func TestImportReportsRefusals(t *testing.T) {
	w, _, files := loaded(t, "Checking")
	_, _ = w.Preview(choicesFrom(files, nil), false)
	_, _ = w.StartImport(false)
	status := wait(t, w)
	if status.State != "failed" || status.Error == nil || !strings.Contains(*status.Error, "Checking") {
		t.Fatalf("%+v", status)
	}
}

func TestImportNeedsAPreview(t *testing.T) {
	w, _ := newWizard()
	if _, err := w.StartImport(false); err == nil {
		t.Fatal("started without a preview")
	}
}

func serve(t *testing.T) (string, int) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	server := &http.Server{Handler: Handler(New(), "secret", func() int { return port })}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return fmt.Sprintf("http://127.0.0.1:%d", port), port
}

func send(t *testing.T, method, url, host, token, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if host != "" {
		req.Host = host
	}
	if token != "" {
		req.Header.Set("X-Wizard-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

func TestServerServesThePage(t *testing.T) {
	base, _ := serve(t)
	status, body := send(t, "GET", base+"/", "", "", "")
	if status != 200 || !strings.Contains(body, "Monarch to Sure") {
		t.Fatalf("%d", status)
	}
}

func TestServerNeedsTheToken(t *testing.T) {
	base, _ := serve(t)
	if status, _ := send(t, "POST", base+"/api/status", "", "", ""); status != 403 {
		t.Fatalf("%d without token", status)
	}
	status, body := send(t, "POST", base+"/api/status", "", "secret", "")
	var result map[string]any
	_ = json.Unmarshal([]byte(body), &result)
	if status != 200 || result["state"] != "idle" {
		t.Fatalf("%d %s", status, body)
	}
}

func TestServerRefusesOtherHosts(t *testing.T) {
	base, _ := serve(t)
	for _, method := range []string{"GET", "POST"} {
		if status, _ := send(t, method, base+"/", "evil.test", "secret", ""); status != 403 {
			t.Fatalf("%s from another host: %d", method, status)
		}
	}
}

func TestServerReturnsErrorsAsMessages(t *testing.T) {
	base, _ := serve(t)
	status, body := send(t, "POST", base+"/api/import", "", "secret", "{}")
	if status != 400 || !strings.Contains(body, "review the import first") {
		t.Fatalf("%d %s", status, body)
	}
}
