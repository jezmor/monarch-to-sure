// Package wizard serves the browser wizard: a single page on 127.0.0.1 that
// walks through connecting to Sure, choosing the Monarch exports, reviewing
// each account, previewing the import and uploading it. The exports are held
// in memory and never written to disk.
package wizard

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jezmor/monarch-to-sure/go/internal/importer"
	"github.com/jezmor/monarch-to-sure/go/internal/sure"
)

var (
	balanceColumns     = []string{"Date", "Balance", "Account"}
	transactionColumns = []string{"Date", "Merchant", "Category", "Account", "Amount", "Id"}
)

// UserError is a problem to show on the page as it is.
type UserError struct{ Message string }

func (e *UserError) Error() string { return e.Message }

func userErr(format string, args ...any) error { return &UserError{fmt.Sprintf(format, args...)} }

// Wizard holds the wizard's state and steps, kept apart from HTTP so they
// can be tested.
type Wizard struct {
	NewClient func(baseURL, apiKey string) sure.API
	Poll      time.Duration
	Today     func() time.Time

	mu           sync.Mutex
	client       sure.API
	balances     importer.Balances
	transactions []importer.Row
	accounts     *importer.Config
	exportEnd    time.Time
	records      []importer.Record
	job          *job
}

type job struct {
	State   string       `json:"state"`
	Log     []string     `json:"log"`
	Session sure.Session `json:"session"`
	Error   *string      `json:"error"`
}

func New() *Wizard {
	return &Wizard{
		NewClient: func(baseURL, apiKey string) sure.API { return sure.NewClient(baseURL, apiKey) },
		Poll:      5 * time.Second,
		Today: func() time.Time {
			y, m, d := time.Now().Date()
			return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
		},
	}
}

type ConnectResult struct {
	URL              string `json:"url"`
	ExistingAccounts int    `json:"existing_accounts"`
}

func (w *Wizard) Connect(baseURL, apiKey string) (*ConnectResult, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	apiKey = strings.TrimSpace(apiKey)
	parsed, err := url.Parse(baseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, userErr("enter the Sure address, such as https://sure.example.com")
	}
	if apiKey == "" {
		return nil, userErr("enter an API key")
	}
	client := w.NewClient(baseURL, apiKey)
	names, err := client.AccountNames()
	if err != nil {
		var httpErr *sure.HTTPError
		if errors.As(err, &httpErr) {
			if httpErr.Status == 401 || httpErr.Status == 403 {
				return nil, userErr("Sure rejected the API key. Check it has read/write access.")
			}
			return nil, err
		}
		return nil, userErr("couldn't reach %s: %v", baseURL, err)
	}
	w.mu.Lock()
	w.client = client
	w.mu.Unlock()
	return &ConnectResult{URL: baseURL, ExistingAccounts: len(names)}, nil
}

func readExport(text string, required []string, label string) ([]importer.Row, error) {
	text = strings.TrimPrefix(text, "\uFEFF")
	header, err := importer.Header(strings.NewReader(text))
	if err != nil {
		return nil, userErr("the %s file isn't a readable CSV: %v", label, err)
	}
	present := map[string]bool{}
	for _, column := range header {
		present[column] = true
	}
	var missing []string
	for _, column := range required {
		if !present[column] {
			missing = append(missing, column)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, userErr("the %s file is missing the columns %s. Is it the right export?", label, strings.Join(missing, ", "))
	}
	return importer.ReadRows(strings.NewReader(text))
}

type AccountRow struct {
	Name            string  `json:"name"`
	Type            string  `json:"type"`
	SuggestClosed   bool    `json:"suggest_closed"`
	LastActivity    *string `json:"last_activity"`
	LastActivityAgo *string `json:"last_activity_ago"`
	LatestBalance   *string `json:"latest_balance"`
	Transactions    int     `json:"transactions"`
	Info            string  `json:"info"`
}

type FilesResult struct {
	ExportEnd      string       `json:"export_end"`
	BalancesOnly   bool         `json:"balances_only"`
	StaleAfterDays int          `json:"stale_after_days"`
	Today          string       `json:"today"`
	Accounts       []AccountRow `json:"accounts"`
}

func (w *Wizard) LoadFiles(balancesText string, transactionsText *string) (*FilesResult, error) {
	if balancesText == "" {
		return nil, userErr("choose the balances export")
	}
	balanceRows, err := readExport(balancesText, balanceColumns, "balances")
	if err != nil {
		return nil, err
	}
	balances, err := importer.ParseBalances(balanceRows)
	if err != nil {
		return nil, userErr("%v", err)
	}
	var transactions []importer.Row
	balancesOnly := transactionsText == nil || *transactionsText == ""
	if !balancesOnly {
		if transactions, err = readExport(*transactionsText, transactionColumns, "transactions"); err != nil {
			return nil, err
		}
	}
	config, exportEnd, err := importer.InspectAccounts(balances, transactions)
	if err != nil {
		return nil, userErr("%v", err)
	}

	w.mu.Lock()
	w.balances, w.transactions, w.accounts, w.exportEnd, w.records = balances, transactions, &config, exportEnd, nil
	w.mu.Unlock()

	result := &FilesResult{
		ExportEnd: exportEnd.Format("2006-01-02"), BalancesOnly: balancesOnly,
		StaleAfterDays: importer.StaleAfterDays, Today: w.Today().Format("2006-01-02"),
		Accounts: []AccountRow{},
	}
	for _, i := range importer.ByLastActivity(config.Accounts) {
		account := config.Accounts[i]
		row := AccountRow{
			Name: account.Name, Type: account.Config.Type, SuggestClosed: account.Config.SuggestClosed,
			LastActivity: account.Config.LastActivity, LatestBalance: account.Config.LatestBalance,
			Transactions: account.Config.TxCount, Info: account.Config.Info,
		}
		if row.LastActivity != nil {
			day, _ := importer.ParseDate(*row.LastActivity)
			ago := importer.Ago(day, exportEnd)
			row.LastActivityAgo = &ago
		}
		result.Accounts = append(result.Accounts, row)
	}
	return result, nil
}

// Choice is what the person picked for one account on the page.
type Choice struct {
	Type         string  `json:"type"`
	Closed       bool    `json:"closed"`
	Include      *bool   `json:"include"`
	Transactions *bool   `json:"transactions"`
	Balance      *string `json:"balance"`
}

func orTrue(b *bool) bool { return b == nil || *b }

func (w *Wizard) configFrom(choices map[string]Choice) (importer.Config, error) {
	if w.accounts == nil {
		return importer.Config{}, userErr("choose the exports first")
	}
	config := importer.Config{Help: w.accounts.Help}
	anyIncluded := false
	for _, account := range w.accounts.Accounts {
		choice, ok := choices[account.Name]
		if !ok {
			return config, userErr("no choice was made for %q", account.Name)
		}
		cfg := account.Config
		cfg.Type, cfg.Closed = choice.Type, choice.Closed
		cfg.Status = "active"
		if cfg.Closed {
			cfg.Status = "disabled"
		}
		cfg.Include, cfg.Transactions = orTrue(choice.Include), orTrue(choice.Transactions)
		cfg.CurrentBalance = importer.Balance{}
		if choice.Balance != nil && strings.TrimSpace(*choice.Balance) != "" {
			cfg.CurrentBalance = importer.Balance{Text: strings.TrimSpace(*choice.Balance), Set: true}
		}
		anyIncluded = anyIncluded || cfg.Include
		config.Accounts = append(config.Accounts, importer.NamedAccount{Name: account.Name, Config: cfg})
	}
	if !anyIncluded {
		return config, userErr("choose at least one account to import")
	}
	if err := config.Validate(); err != nil {
		return config, &UserError{err.Error()}
	}
	return config, nil
}

type PreviewAccount struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Status     string `json:"status"`
	Balance    string `json:"balance"`
	Valuations int    `json:"valuations"`
}

type PreviewResult struct {
	Stats                map[string]int   `json:"stats"`
	SkippedAccounts      []string         `json:"skipped_accounts"`
	ExistingAccountNames []string         `json:"existing_account_names"`
	Accounts             []PreviewAccount `json:"accounts"`
}

func (w *Wizard) Preview(choices map[string]Choice, includePending bool) (*PreviewResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.client == nil {
		return nil, userErr("connect to Sure first")
	}
	config, err := w.configFrom(choices)
	if err != nil {
		return nil, err
	}
	existing, err := w.client.ExistingRecords()
	if err != nil {
		return nil, err
	}
	records, stats, err := importer.Convert(w.balances, w.transactions, config, importer.Options{
		IncludePending: includePending, Existing: existing, Today: w.Today(),
	})
	if err != nil {
		return nil, userErr("%v", err)
	}
	w.records = records

	names, err := w.client.AccountNames()
	if err != nil {
		return nil, err
	}
	result := &PreviewResult{Stats: stats, SkippedAccounts: []string{}, ExistingAccountNames: []string{}, Accounts: []PreviewAccount{}}
	valuations := map[string]int{}
	for _, r := range records {
		if v, ok := r.Data.(importer.ValuationData); ok {
			valuations[v.AccountID]++
		}
	}
	for _, r := range records {
		if a, ok := r.Data.(importer.AccountData); ok {
			result.Accounts = append(result.Accounts, PreviewAccount{a.Name, a.AccountableType, a.Status, a.Balance, valuations[a.ID]})
			if names[a.Name] {
				result.ExistingAccountNames = append(result.ExistingAccountNames, a.Name)
			}
		}
	}
	for _, account := range config.Accounts {
		if !account.Config.Include {
			result.SkippedAccounts = append(result.SkippedAccounts, account.Name)
		}
	}
	sort.Strings(result.SkippedAccounts)
	sort.Strings(result.ExistingAccountNames)
	return result, nil
}

func (w *Wizard) StartImport(allowExistingAccounts bool) (*job, error) {
	w.mu.Lock()
	if w.records == nil {
		w.mu.Unlock()
		return nil, userErr("review the import first")
	}
	if w.job != nil && w.job.State == "running" {
		w.mu.Unlock()
		return nil, userErr("an import is already running")
	}
	lines, err := importer.NDJSONLines(w.records)
	if err != nil {
		w.mu.Unlock()
		return nil, err
	}
	current := &job{State: "running", Log: []string{}}
	w.job = current
	client := w.client
	w.mu.Unlock()

	go func() {
		log := func(line string) {
			w.mu.Lock()
			current.Log = append(current.Log, line)
			w.mu.Unlock()
		}
		session, err := sure.Upload(lines, client, allowExistingAccounts, w.Poll, log)
		w.mu.Lock()
		defer w.mu.Unlock()
		if err != nil {
			message := err.Error()
			current.Error, current.State = &message, "failed"
			return
		}
		current.Session, current.State = session, "failed"
		if session.Status() == "complete" {
			current.State = "complete"
		}
	}()
	return w.Status(), nil
}

// Status reports the import job, or {"state": "idle"} before one starts.
func (w *Wizard) Status() *job {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.job == nil {
		return &job{State: "idle", Log: []string{}}
	}
	snapshot := *w.job
	snapshot.Log = append([]string{}, w.job.Log...)
	return &snapshot
}
