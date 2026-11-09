package importer

import (
	"encoding/json"
	"math/big"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The same fixtures as the Python runtime's tests.
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

func day(s string) time.Time {
	d, err := ParseDate(s)
	if err != nil {
		panic(err)
	}
	return d
}

func rat(s string) *big.Rat {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		panic(s)
	}
	return r
}

func history(pairs ...string) History {
	h := History{}
	for i := 0; i < len(pairs); i += 2 {
		h[day(pairs[i])] = &Amount{Rat: rat(pairs[i+1]), Text: pairs[i+1]}
	}
	return h
}

func flows(pairs ...string) Flows {
	f := Flows{}
	for i := 0; i < len(pairs); i += 2 {
		f[day(pairs[i])] = rat(pairs[i+1])
	}
	return f
}

func anchorText(anchors []Anchor) []string {
	var out []string
	for _, a := range anchors {
		out = append(out, formatDate(a.Date)+"="+Money(a.Value))
	}
	return out
}

func assertEqual(t *testing.T, want, got any) {
	t.Helper()
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("want %#v\n got %#v", want, got)
	}
}

func TestNoAnchorWhenTransactionsExplainTheChange(t *testing.T) {
	anchors := BalanceAnchors(history("2024-01-01", "100", "2024-01-02", "90"), flows("2024-01-02", "-10"), false)
	assertEqual(t, []string{"2024-01-01=100.00"}, anchorText(anchors))
}

func TestAnchorWhereBalanceDriftsFromTransactions(t *testing.T) {
	anchors := BalanceAnchors(history("2024-01-01", "100", "2024-01-02", "95"), flows("2024-01-02", "-10"), false)
	assertEqual(t, []string{"2024-01-01=100.00", "2024-01-02=95.00"}, anchorText(anchors))
}

func TestOpeningBalancePrecedesEarlierTransactions(t *testing.T) {
	anchors := BalanceAnchors(history("2024-01-03", "50"), flows("2024-01-01", "-10", "2024-01-02", "5"), false)
	assertEqual(t, []string{"2023-12-31=55.00"}, anchorText(anchors))
}

func TestClosedAccountDropsToZero(t *testing.T) {
	anchors := BalanceAnchors(history("2024-01-01", "100"), nil, true)
	assertEqual(t, []string{"2024-01-01=100.00", "2024-01-02=0.00"}, anchorText(anchors))
}

func TestReplayReproducesEveryHistoryDay(t *testing.T) {
	h := history("2024-01-01", "100", "2024-01-02", "90", "2024-01-03", "93", "2024-01-05", "80")
	f := flows("2024-01-02", "-10", "2024-01-03", "4", "2024-01-04", "-13")
	end := day("2024-01-05")
	replayed := Replay(BalanceAnchors(h, f, false), f, &end)
	for d, v := range h {
		if !equal(replayed[d], v.Rat) {
			t.Fatalf("%s: want %s got %s", formatDate(d), Money(v.Rat), Money(replayed[d]))
		}
	}
}

func TestMoneyRoundsHalfToEven(t *testing.T) {
	for in, want := range map[string]string{
		"1234.565": "1234.56", "1234.575": "1234.58", "-0.001": "0.00", "-2.5": "-2.50", "0": "0.00", "1e3": "1000.00",
	} {
		if got := Money(rat(in)); got != want {
			t.Errorf("Money(%s) = %s, want %s", in, got, want)
		}
	}
}

type fixture struct {
	balances     Balances
	transactions []Row
	config       Config
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	balanceRows, err := ReadRows(strings.NewReader(balancesCSV))
	if err != nil {
		t.Fatal(err)
	}
	balances, err := ParseBalances(balanceRows)
	if err != nil {
		t.Fatal(err)
	}
	transactions, err := ReadRows(strings.NewReader(transactionsCSV))
	if err != nil {
		t.Fatal(err)
	}
	config, _, err := InspectAccounts(balances, transactions)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{balances, transactions, config}
}

func (f *fixture) convert(t *testing.T, opts Options) ([]Record, map[string]int) {
	t.Helper()
	records, stats, err := Convert(f.balances, f.transactions, f.config, opts)
	if err != nil {
		t.Fatal(err)
	}
	return records, stats
}

func dataOf[T any](records []Record) map[string]T {
	out := map[string]T{}
	for _, r := range records {
		if d, ok := r.Data.(T); ok {
			out[reflect.ValueOf(d).FieldByName("ID").String()] = d
		}
	}
	return out
}

func TestInspectGuessesTypesAndCountsTransactions(t *testing.T) {
	f := newFixture(t)
	types := map[string]string{}
	for _, a := range f.config.Accounts {
		types[a.Name] = a.Config.Type
	}
	assertEqual(t, map[string]string{"Checking": "Depository", "Card": "CreditCard", "Old Savings": "Investment"}, types)
	assertEqual(t, 4, f.config.Account("Checking").TxCount) // rows including pending, not days
}

func TestTransactionsFlipSignAndLinkReferences(t *testing.T) {
	f := newFixture(t)
	records, _ := f.convert(t, Options{})
	tx := dataOf[TransactionData](records)["monarch-transaction:1"]
	assertEqual(t, "10.00", tx.Amount)
	assertEqual(t, "monarch-category:Groceries", tx.CategoryID)
	assertEqual(t, "monarch-merchant:Grocer", tx.MerchantID)
	assertEqual(t, "weekly shop", *tx.Notes)
}

func TestTagsAndTransferKinds(t *testing.T) {
	f := newFixture(t)
	records, _ := f.convert(t, Options{})
	txs := dataOf[TransactionData](records)
	assertEqual(t, []string{"monarch-tag:Food", "monarch-tag:Treats"}, txs["monarch-transaction:3"].TagIDs)
	assertEqual(t, "cc_payment", txs["monarch-transaction:4"].Kind)
	assertEqual(t, "", txs["monarch-transaction:4"].CategoryID)
}

func TestPendingTransactionsAreOptional(t *testing.T) {
	f := newFixture(t)
	records, _ := f.convert(t, Options{})
	if _, ok := dataOf[TransactionData](records)["monarch-transaction:5"]; ok {
		t.Fatal("pending transaction imported")
	}
	records, _ = f.convert(t, Options{IncludePending: true})
	if _, ok := dataOf[TransactionData](records)["monarch-transaction:5"]; !ok {
		t.Fatal("pending transaction missing")
	}
}

func TestLiabilityBalancesArePositiveInSure(t *testing.T) {
	f := newFixture(t)
	records, _ := f.convert(t, Options{})
	assertEqual(t, "70.00", dataOf[AccountData](records)["monarch-account:Card"].Balance)
}

func TestClosedAccountsDropToZeroAndAreDisabled(t *testing.T) {
	f := newFixture(t)
	cfg := f.config.Account("Old Savings")
	cfg.Closed, cfg.Status = true, "disabled"
	records, _ := f.convert(t, Options{})
	assertEqual(t, "0.00", dataOf[ValuationData](records)["monarch-valuation:Old Savings:2024-01-02"].Amount)
	assertEqual(t, "disabled", dataOf[AccountData](records)["monarch-account:Old Savings"].Status)
}

func TestExcludedAccountsAreLeftOut(t *testing.T) {
	f := newFixture(t)
	f.config.Account("Card").Include = false
	records, stats := f.convert(t, Options{})
	if _, ok := dataOf[AccountData](records)["monarch-account:Card"]; ok {
		t.Fatal("excluded account imported")
	}
	assertEqual(t, 1, stats["transactions_skipped_excluded_account"])
}

func TestAccountsCanSkipTheirTransactions(t *testing.T) {
	f := newFixture(t)
	f.config.Account("Checking").Transactions = false
	records, stats := f.convert(t, Options{})
	for _, tx := range dataOf[TransactionData](records) {
		if tx.AccountID == "monarch-account:Checking" {
			t.Fatal("transaction imported for a balances-only account")
		}
	}
	assertEqual(t, 3, stats["transactions_skipped_balances_only"])
	var amounts []string
	for _, r := range records {
		if v, ok := r.Data.(ValuationData); ok && v.AccountID == "monarch-account:Checking" {
			amounts = append(amounts, v.Amount)
		}
	}
	assertEqual(t, []string{"100.00", "90.00", "95.00"}, amounts)
}

func TestCurrentBalanceAddsAValuationToday(t *testing.T) {
	f := newFixture(t)
	f.config.Account("Checking").CurrentBalance = Balance{Text: "120", Set: true}
	f.config.Account("Card").CurrentBalance = Balance{Text: "80", Set: true} // owed, as Sure shows it
	records, _ := f.convert(t, Options{Today: day("2024-01-10")})
	valuations, accounts := dataOf[ValuationData](records), dataOf[AccountData](records)
	assertEqual(t, "120.00", valuations["monarch-valuation:Checking:2024-01-10"].Amount)
	assertEqual(t, "80.00", valuations["monarch-valuation:Card:2024-01-10"].Amount)
	assertEqual(t, "120.00", accounts["monarch-account:Checking"].Balance)
	assertEqual(t, "80.00", accounts["monarch-account:Card"].Balance)
}

func TestUnchangedCurrentBalanceAddsNothing(t *testing.T) {
	f := newFixture(t)
	before, _ := f.convert(t, Options{Today: day("2024-01-10")})
	f.config.Account("Checking").CurrentBalance = Balance{Text: "95.00", Set: true}
	f.config.Account("Card").CurrentBalance = Balance{Text: "70", Set: true}
	after, _ := f.convert(t, Options{Today: day("2024-01-10")})
	assertEqual(t, dataOf[ValuationData](before), dataOf[ValuationData](after))
}

func TestClosedAccountsIgnoreCurrentBalance(t *testing.T) {
	f := newFixture(t)
	cfg := f.config.Account("Checking")
	cfg.Closed, cfg.CurrentBalance = true, Balance{Text: "500", Set: true}
	records, _ := f.convert(t, Options{Today: day("2024-01-10")})
	if _, ok := dataOf[ValuationData](records)["monarch-valuation:Checking:2024-01-10"]; ok {
		t.Fatal("closed account got a current balance")
	}
}

func TestExistingCategoriesKeepTheirLookAndMerchantsAreNotSent(t *testing.T) {
	f := newFixture(t)
	color, icon := "#123456", "carrot"
	records, stats := f.convert(t, Options{Existing: &Existing{
		Categories: map[string]CategoryLook{"Groceries": {&color, &icon}},
		Merchants:  map[string]bool{"Grocer": true},
	}})
	categories := dataOf[CategoryData](records)
	assertEqual(t, "#123456", *categories["monarch-category:Groceries"].Color)
	assertEqual(t, 1, stats["categories_reusing_existing"])
	if _, ok := dataOf[NamedData](records)["monarch-merchant:Grocer"]; ok {
		t.Fatal("existing merchant sent")
	}
	assertEqual(t, "", dataOf[TransactionData](records)["monarch-transaction:1"].MerchantID)
}

func TestRecordsAreInDependencyOrder(t *testing.T) {
	f := newFixture(t)
	records, _ := f.convert(t, Options{})
	rank := map[string]int{}
	for i, recordType := range RecordOrder {
		rank[recordType] = i
	}
	for i := 1; i < len(records); i++ {
		if rank[records[i].Type] < rank[records[i-1].Type] {
			t.Fatalf("%s after %s", records[i].Type, records[i-1].Type)
		}
	}
}

func TestUnknownAccountsAreReported(t *testing.T) {
	f := newFixture(t)
	f.config.Accounts = f.config.Accounts[:1]
	if _, _, err := Convert(f.balances, f.transactions, f.config, Options{}); err == nil || !strings.Contains(err.Error(), "re-run init-config") {
		t.Fatalf("got %v", err)
	}
}

func TestConfigRoundTripsInOrderWithDefaults(t *testing.T) {
	config, err := LoadConfig([]byte(`{"accounts": {"Zed": {"type": "Loan"}, "Abe": {"type": "Depository", "current_balance": 0}}}`))
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, "Zed", config.Accounts[0].Name)
	zed := config.Accounts[0].Config
	assertEqual(t, []any{true, true, "active"}, []any{zed.Include, zed.Transactions, zed.Status})
	if v, _ := config.Accounts[1].Config.CurrentBalance.Value(); v == nil || v.Sign() != 0 {
		t.Fatal("numeric zero balance should be set")
	}
	data, _ := json.Marshal(config)
	again, err := LoadConfig(data)
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, config.Accounts, again.Accounts)
}

func TestConfigRejectsBadValues(t *testing.T) {
	for _, doc := range []string{
		`{"accounts": {"A": {"type": "Boat"}}}`,
		`{"accounts": {"A": {"type": "Loan", "status": "gone"}}}`,
		`{"accounts": {"A": {"type": "Loan", "closed": "yes"}}}`,
		`{"accounts": {"A": {"type": "Loan", "current_balance": "lots"}}}`,
	} {
		if _, err := LoadConfig([]byte(doc)); err == nil {
			t.Errorf("accepted %s", doc)
		}
	}
}

func TestNDJSONEscapesLikePython(t *testing.T) {
	lines, err := NDJSONLines([]Record{{"Merchant", NamedData{"m", "Caf\u00e9 <&> \U0001F355"}}})
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, `{"type":"Merchant","data":{"id":"m","name":"Caf\u00e9 <&> \ud83c\udf55"}}`+"\n", lines[0])
}

func TestSelectionParsing(t *testing.T) {
	selected, err := ParseSelection("1, 4-6", 6)
	assertEqual(t, nil, err)
	assertEqual(t, "1,4-6", FormatSelection(selected))
	if _, err := ParseSelection("7", 6); err == nil {
		t.Fatal("accepted 7 of 6")
	}
	if _, err := ParseSelection("1, 2. 3", 6); err == nil || !strings.Contains(err.Error(), `"2.3" isn't a number or range`) {
		t.Fatalf("got %v", err)
	}
}

func TestReviewAcceptsSuggestionsAndAnswers(t *testing.T) {
	end := day("2024-12-31")
	newConfig := func() Config {
		return Config{Accounts: []NamedAccount{
			{"Recent", GuessAccount(history("2024-12-30", "5"), nil, 0, end)},
			{"Old", GuessAccount(history("2023-01-01", "5"), nil, 0, end)},
		}}
	}
	review := func(answers ...string) Config {
		config := newConfig()
		ask := func(string) (string, error) {
			answer := answers[0]
			answers = answers[1:]
			return answer, nil
		}
		if err := ReviewAccounts(&config, end, ask, func(string) {}); err != nil {
			t.Fatal(err)
		}
		return config
	}

	config := review("", "")
	assertEqual(t, []any{false, "active"}, []any{config.Account("Recent").Closed, config.Account("Recent").Status})
	assertEqual(t, []any{true, "disabled"}, []any{config.Account("Old").Closed, config.Account("Old").Status})

	// Accounts are listed newest first: 1 is Recent, 2 is Old.
	config = review("1", "2")
	assertEqual(t, []any{true, false, false}, []any{config.Account("Recent").Closed, config.Account("Old").Closed, config.Account("Old").Include})

	config = review("9", "none", "")
	assertEqual(t, false, config.Account("Old").Closed)
}
