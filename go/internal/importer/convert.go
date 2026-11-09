package importer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// transferCategoryKinds are Monarch categories that describe money moving
// between accounts. Sure models these as transaction kinds rather than
// categories, so they stay out of budgets.
var transferCategoryKinds = map[string]string{
	"Transfer":            "funds_movement",
	"Credit Card Payment": "cc_payment",
}

// RecordOrder lists record types in dependency order, so every chunk only
// references records from the same or an earlier chunk.
var RecordOrder = []string{"Account", "Category", "Tag", "Merchant", "Transaction", "Valuation"}

// Record is one line of Sure's NDJSON import format.
type Record struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}

// The data structs list fields in the same order as the Python runtime so
// both write identical NDJSON.

type AccountData struct {
	ID                       string            `json:"id"`
	Name                     string            `json:"name"`
	AccountableType          string            `json:"accountable_type"`
	Accountable              map[string]string `json:"accountable"`
	Subtype                  *string           `json:"subtype"`
	Balance                  string            `json:"balance"`
	Status                   string            `json:"status"`
	SkipOpeningBalanceImport bool              `json:"skip_opening_balance_import"`
	Currency                 string            `json:"currency,omitempty"`
}

type TransactionData struct {
	ID         string   `json:"id"`
	AccountID  string   `json:"account_id"`
	Date       string   `json:"date"`
	Amount     string   `json:"amount"`
	Name       string   `json:"name"`
	Notes      *string  `json:"notes"`
	Kind       string   `json:"kind"`
	Currency   string   `json:"currency,omitempty"`
	CategoryID string   `json:"category_id,omitempty"`
	MerchantID string   `json:"merchant_id,omitempty"`
	TagIDs     []string `json:"tag_ids,omitempty"`
}

type ValuationData struct {
	ID        string `json:"id"`
	AccountID string `json:"account_id"`
	Date      string `json:"date"`
	Amount    string `json:"amount"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Currency  string `json:"currency,omitempty"`
}

type CategoryData struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Color      *string `json:"color,omitempty"`
	LucideIcon *string `json:"lucide_icon,omitempty"`
}

type NamedData struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// CategoryLook is an existing Sure category's color and icon.
type CategoryLook struct {
	Color *string
	Icon  *string
}

// Existing is the data already in Sure that an import must not rewrite.
type Existing struct {
	Categories map[string]CategoryLook
	Merchants  map[string]bool
}

// Options adjust a conversion.
type Options struct {
	IncludePending bool
	// Existing categories keep their color and icon; existing merchants are
	// left out, so those transactions keep the name but no merchant link.
	Existing *Existing
	// Today dates a changed current balance. Defaults to the current date.
	Today time.Time
}

func AccountSourceID(name string) string { return "monarch-account:" + name }

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Convert builds the import records for the included accounts and returns
// them in RecordOrder with counts by kind.
func Convert(balances Balances, transactions []Row, config Config, opts Options) ([]Record, map[string]int, error) {
	existing := opts.Existing
	if existing == nil {
		existing = &Existing{Categories: map[string]CategoryLook{}, Merchants: map[string]bool{}}
	}
	today := opts.Today
	if today.IsZero() {
		y, m, d := time.Now().Date()
		today = time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	}
	transactions = WithoutPending(transactions, opts.IncludePending)

	included := map[string]*AccountConfig{}
	known := map[string]bool{}
	for i := range config.Accounts {
		known[config.Accounts[i].Name] = true
		if config.Accounts[i].Config.Include {
			included[config.Accounts[i].Name] = &config.Accounts[i].Config
		}
	}
	unknownSet := map[string]bool{}
	for _, row := range transactions {
		if !known[row["Account"]] {
			unknownSet[row["Account"]] = true
		}
	}
	if len(unknownSet) > 0 {
		var unknown []string
		for name := range unknownSet {
			unknown = append(unknown, name)
		}
		sort.Strings(unknown)
		return nil, nil, fmt.Errorf("accounts missing from config (re-run init-config): %s", strings.Join(unknown, ", "))
	}

	records := map[string][]Record{}
	categories, tags, merchants := map[string]bool{}, map[string]bool{}, map[string]bool{}
	flows := map[string]Flows{}
	stats := map[string]int{}

	for _, row := range transactions {
		name := row["Account"]
		cfg, ok := included[name]
		if !ok {
			stats["transactions_skipped_excluded_account"]++
			continue
		}
		if !cfg.Transactions {
			stats["transactions_skipped_balances_only"]++
			continue
		}
		amount, err := ParseAmount(row["Amount"])
		if err != nil {
			return nil, nil, err
		}
		if amount == nil {
			stats["transactions_skipped_no_amount"]++
			continue
		}
		day, err := ParseDate(row["Date"])
		if err != nil {
			return nil, nil, err
		}
		if flows[name] == nil {
			flows[name] = Flows{}
		}
		if prior, ok := flows[name][day]; ok {
			flows[name][day] = add(prior, amount.Rat)
		} else {
			flows[name][day] = amount.Rat
		}

		category := strings.TrimSpace(row["Category"])
		kind, transfer := transferCategoryKinds[category]
		if !transfer {
			kind = "standard"
		}
		merchant := strings.TrimSpace(row["Merchant"])
		txName := merchant
		if txName == "" {
			txName = strings.TrimSpace(row["Original Statement"])
		}
		if txName == "" {
			txName = "Imported transaction"
		}
		data := TransactionData{
			ID:        "monarch-transaction:" + row["Id"],
			AccountID: AccountSourceID(name),
			Date:      formatDate(day),
			// Sure records outflows as positive amounts; Monarch as negative.
			Amount: Money(neg(amount.Rat)),
			Name:   txName,
			Notes:  optional(strings.TrimSpace(row["Notes"])),
			Kind:   kind,
		}
		if cfg.Currency != nil && *cfg.Currency != "" {
			data.Currency = *cfg.Currency
		}
		if category != "" && !transfer {
			categories[category] = true
			data.CategoryID = "monarch-category:" + category
		}
		if existing.Merchants[merchant] {
			stats["transactions_unlinked_from_existing_merchant"]++
		} else if merchant != "" {
			merchants[merchant] = true
			data.MerchantID = "monarch-merchant:" + merchant
		}
		for _, tag := range splitTags(row["Tags"]) {
			tags[tag] = true
			data.TagIDs = append(data.TagIDs, "monarch-tag:"+tag)
		}
		records["Transaction"] = append(records["Transaction"], Record{"Transaction", data})
	}

	for _, account := range config.Accounts {
		name, cfg := account.Name, account.Config
		if !cfg.Include {
			continue
		}
		sign := big.NewRat(1, 1)
		if IsLiability(cfg.Type) {
			sign = big.NewRat(-1, 1) // Sure stores amounts owed as positive
		}
		accountFlows := flows[name]
		anchors := BalanceAnchors(balances[name], accountFlows, cfg.Closed)
		override, err := cfg.CurrentBalance.Value()
		if err != nil {
			return nil, nil, fmt.Errorf("account %q: %w", name, err)
		}
		if override != nil && !cfg.Closed {
			anchors = WithCurrentBalance(anchors, accountFlows, today, mul(override, sign))
		}
		current := latest(Replay(anchors, accountFlows, nil))

		data := AccountData{
			ID:                       AccountSourceID(name),
			Name:                     name,
			AccountableType:          cfg.Type,
			Accountable:              map[string]string{},
			Subtype:                  cfg.Subtype,
			Balance:                  Money(mul(current, sign)),
			Status:                   cfg.Status,
			SkipOpeningBalanceImport: true, // history comes from the valuations below
		}
		if cfg.Subtype != nil && *cfg.Subtype != "" {
			data.Accountable["subtype"] = *cfg.Subtype
		}
		if cfg.Currency != nil && *cfg.Currency != "" {
			data.Currency = *cfg.Currency
		}
		records["Account"] = append(records["Account"], Record{"Account", data})

		for i, anchor := range anchors {
			valuation := ValuationData{
				ID:        fmt.Sprintf("monarch-valuation:%s:%s", name, formatDate(anchor.Date)),
				AccountID: AccountSourceID(name),
				Date:      formatDate(anchor.Date),
				Amount:    Money(mul(anchor.Value, sign)),
				Kind:      "reconciliation",
				Name:      "Balance from Monarch",
			}
			if i == 0 {
				valuation.Kind, valuation.Name = "opening_anchor", "Opening balance"
			}
			if cfg.Currency != nil && *cfg.Currency != "" {
				valuation.Currency = *cfg.Currency
			}
			records["Valuation"] = append(records["Valuation"], Record{"Valuation", valuation})
		}
	}

	stats["categories_reusing_existing"] = 0
	for _, name := range sortedKeys(categories) {
		data := CategoryData{ID: "monarch-category:" + name, Name: name}
		if look, ok := existing.Categories[name]; ok {
			data.Color, data.LucideIcon = look.Color, look.Icon
			stats["categories_reusing_existing"]++
		}
		records["Category"] = append(records["Category"], Record{"Category", data})
	}
	for _, name := range sortedKeys(tags) {
		records["Tag"] = append(records["Tag"], Record{"Tag", NamedData{"monarch-tag:" + name, name}})
	}
	for _, name := range sortedKeys(merchants) {
		records["Merchant"] = append(records["Merchant"], Record{"Merchant", NamedData{"monarch-merchant:" + name, name}})
	}

	var ordered []Record
	for _, recordType := range RecordOrder {
		ordered = append(ordered, records[recordType]...)
		stats[recordType] = len(records[recordType])
	}
	return ordered, stats, nil
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// NDJSONLines encodes records one per line, escaping like Python's
// json.dumps (non-ASCII as \u escapes, HTML characters left alone) so both
// runtimes hash the same file to the same import session.
func NDJSONLines(records []Record) ([]string, error) {
	lines := make([]string, 0, len(records))
	for _, record := range records {
		var buf bytes.Buffer
		encoder := json.NewEncoder(&buf)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(record); err != nil {
			return nil, err
		}
		lines = append(lines, asciiOnly(buf.String()))
	}
	return lines, nil
}

func asciiOnly(s string) string {
	var out strings.Builder
	for _, r := range s {
		switch {
		case r < utf8.RuneSelf:
			out.WriteRune(r)
		case r > 0xFFFF:
			r -= 0x10000
			fmt.Fprintf(&out, `\u%04x\u%04x`, 0xD800+(r>>10), 0xDC00+(r&0x3FF))
		default:
			fmt.Fprintf(&out, `\u%04x`, r)
		}
	}
	return out.String()
}
