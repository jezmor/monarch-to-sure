package importer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"
)

var (
	AssetTypes      = []string{"Depository", "Investment", "Crypto", "Property", "Vehicle", "OtherAsset"}
	LiabilityTypes  = []string{"CreditCard", "Loan", "OtherLiability"}
	accountStatuses = []string{"active", "disabled"}
)

// StaleAfterDays is how long without activity before an account is
// suggested as closed.
const StaleAfterDays = 180

func contains(list []string, value string) bool {
	for _, v := range list {
		if v == value {
			return true
		}
	}
	return false
}

func IsLiability(accountType string) bool { return contains(LiabilityTypes, accountType) }

func AccountTypes() []string {
	types := append(append([]string{}, AssetTypes...), LiabilityTypes...)
	sort.Strings(types)
	return types
}

// Balance is an optional number that accepts a JSON string or number.
type Balance struct {
	Text string
	Set  bool
}

func (b *Balance) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if bytes.Equal(data, []byte("null")) {
		*b = Balance{}
		return nil
	}
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		text = string(data)
	}
	*b = Balance{Text: text, Set: true}
	return nil
}

func (b Balance) MarshalJSON() ([]byte, error) {
	if !b.Set {
		return []byte("null"), nil
	}
	return json.Marshal(b.Text)
}

// Value parses the balance, or returns nil when none was given.
func (b Balance) Value() (*big.Rat, error) {
	if !b.Set {
		return nil, nil
	}
	amount, err := ParseAmount(b.Text)
	if err != nil || amount == nil {
		return nil, err
	}
	return amount.Rat, nil
}

// AccountConfig is one account in accounts.json. Fields starting with an
// underscore are information for the person reviewing the file.
type AccountConfig struct {
	Type           string  `json:"type"`
	Closed         bool    `json:"closed"`
	Status         string  `json:"status"`
	Subtype        *string `json:"subtype"`
	Include        bool    `json:"include"`
	Transactions   bool    `json:"transactions"`
	CurrentBalance Balance `json:"current_balance"`
	Currency       *string `json:"currency"`

	SuggestClosed bool    `json:"_suggest_closed"`
	LastActivity  *string `json:"_last_activity"`
	LatestBalance *string `json:"_latest_balance"`
	TxCount       int     `json:"_transactions"`
	Info          string  `json:"_info"`
}

func (a *AccountConfig) UnmarshalJSON(data []byte) error {
	type plain AccountConfig
	decoded := plain{Status: "active", Include: true, Transactions: true}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*a = AccountConfig(decoded)
	return nil
}

// NamedAccount pairs an account's Monarch name with its config.
type NamedAccount struct {
	Name   string
	Config AccountConfig
}

// Config is accounts.json. Accounts keep the file's order.
type Config struct {
	Help     string
	Accounts []NamedAccount
}

func (c *Config) Account(name string) *AccountConfig {
	for i := range c.Accounts {
		if c.Accounts[i].Name == name {
			return &c.Accounts[i].Config
		}
	}
	return nil
}

func (c Config) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(`{"_help":`)
	help, _ := json.Marshal(c.Help)
	buf.Write(help)
	buf.WriteString(`,"accounts":{`)
	for i, account := range c.Accounts {
		if i > 0 {
			buf.WriteByte(',')
		}
		name, _ := json.Marshal(account.Name)
		buf.Write(name)
		buf.WriteByte(':')
		data, err := json.Marshal(account.Config)
		if err != nil {
			return nil, err
		}
		buf.Write(data)
	}
	buf.WriteString("}}")
	return buf.Bytes(), nil
}

func (c *Config) UnmarshalJSON(data []byte) error {
	var top struct {
		Help     string          `json:"_help"`
		Accounts json.RawMessage `json:"accounts"`
	}
	if err := json.Unmarshal(data, &top); err != nil {
		return err
	}
	c.Help = top.Help
	c.Accounts = nil
	decoder := json.NewDecoder(bytes.NewReader(top.Accounts))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return errors.New("accounts must be an object of account names")
	}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		name := token.(string)
		var account AccountConfig
		if err := decoder.Decode(&account); err != nil {
			return fmt.Errorf("account %q: %w", name, err)
		}
		c.Accounts = append(c.Accounts, NamedAccount{Name: name, Config: account})
	}
	return nil
}

// LoadConfig parses and validates accounts.json.
func LoadConfig(data []byte) (Config, error) {
	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return config, err
	}
	return config, config.Validate()
}

// Validate checks the included accounts' settings.
func (c Config) Validate() error {
	for _, account := range c.Accounts {
		cfg := account.Config
		if !cfg.Include {
			continue
		}
		if !contains(AccountTypes(), cfg.Type) {
			return fmt.Errorf("account %q has invalid type %q", account.Name, cfg.Type)
		}
		if !contains(accountStatuses, cfg.Status) {
			return fmt.Errorf("account %q has invalid status %q", account.Name, cfg.Status)
		}
		if _, err := cfg.CurrentBalance.Value(); err != nil {
			return fmt.Errorf("account %q has current_balance %q, which isn't a number", account.Name, cfg.CurrentBalance.Text)
		}
	}
	return nil
}

// ConfigHelp explains accounts.json to the person editing it.
func ConfigHelp() string {
	return "Review each account before converting. type is one of " + strings.Join(AccountTypes(), ", ") +
		". The account types are guesses: check every one. closed sets the balance to " +
		"zero the day after the last Monarch balance. status is active or disabled; " +
		"disabled accounts are hidden from current balances but still count toward net " +
		"worth history. Set include to false to skip an account, or transactions to false to " +
		"import only its balance history. current_balance sets today's balance as Sure shows " +
		"it, with amounts owed as positive numbers; null keeps Monarch's latest balance. " +
		"currency defaults to the Sure family currency."
}

// LastActivity is the last day an account's money moved: its latest
// transaction, or the latest day its balance changed. Monarch keeps reporting
// a balance for accounts long after they stop changing, so the last balance
// date alone says little.
func LastActivity(history History, txDates map[time.Time]bool) *time.Time {
	days := sortedDays(history)
	var candidates []time.Time
	var lastChange *time.Time
	for i := 1; i < len(days); i++ {
		if !equal(history[days[i]].Rat, history[days[i-1]].Rat) {
			d := days[i]
			lastChange = &d
		}
	}
	if lastChange != nil {
		candidates = append(candidates, *lastChange)
	}
	if len(days) > 0 {
		candidates = append(candidates, days[0])
	}
	for d := range txDates {
		candidates = append(candidates, d)
	}
	if len(candidates) == 0 {
		return nil
	}
	latest := candidates[0]
	for _, d := range candidates[1:] {
		if d.After(latest) {
			latest = d
		}
	}
	return &latest
}

// GuessAccount suggests a type and closure from the balances alone. The
// guesses are only suggestions for the person to confirm.
func GuessAccount(history History, txDates map[time.Time]bool, txCount int, exportEnd time.Time) AccountConfig {
	days := sortedDays(history)
	var nonzero, negative int
	for _, d := range days {
		if sign := history[d].Rat.Sign(); sign != 0 {
			nonzero++
			if sign < 0 {
				negative++
			}
		}
	}
	accountType := "Investment"
	switch {
	case nonzero > 0 && float64(negative)/float64(nonzero) > 0.5:
		accountType = "Loan"
		if len(txDates) > 0 {
			accountType = "CreditCard"
		}
	case len(txDates) > 0:
		accountType = "Depository"
	}

	cfg := AccountConfig{
		Type: accountType, Status: "active", Include: true, Transactions: true, TxCount: txCount,
		Info: fmt.Sprintf("%d balance days", len(days)),
	}
	if active := LastActivity(history, txDates); active != nil {
		text := formatDate(*active)
		cfg.LastActivity = &text
		cfg.SuggestClosed = int(exportEnd.Sub(*active).Hours()/24) >= StaleAfterDays
	}
	if len(days) > 0 {
		latest := history[days[len(days)-1]].Text
		cfg.LatestBalance = &latest
		cfg.Info += fmt.Sprintf(" (%s to %s)", formatDate(days[0]), formatDate(days[len(days)-1]))
	}
	return cfg
}

// InspectAccounts suggests a config for every account in the exports and
// returns the last date they cover. transactions should include pending rows.
func InspectAccounts(balances Balances, transactions []Row) (Config, time.Time, error) {
	txDates := map[string]map[time.Time]bool{}
	txCounts := map[string]int{}
	var exportEnd time.Time
	found := false
	for _, row := range transactions {
		day, err := ParseDate(row["Date"])
		if err != nil {
			return Config{}, exportEnd, err
		}
		if txDates[row["Account"]] == nil {
			txDates[row["Account"]] = map[time.Time]bool{}
		}
		txDates[row["Account"]][day] = true
		txCounts[row["Account"]]++
		if !found || day.After(exportEnd) {
			exportEnd, found = day, true
		}
	}
	names := map[string]bool{}
	for name, history := range balances {
		names[name] = true
		for day := range history {
			if !found || day.After(exportEnd) {
				exportEnd, found = day, true
			}
		}
	}
	if !found {
		return Config{}, exportEnd, errors.New("the exports contain no balances or transactions")
	}
	for name := range txDates {
		names[name] = true
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)

	config := Config{Help: ConfigHelp()}
	for _, name := range sorted {
		config.Accounts = append(config.Accounts, NamedAccount{
			Name:   name,
			Config: GuessAccount(balances[name], txDates[name], txCounts[name], exportEnd),
		})
	}
	return config, exportEnd, nil
}
