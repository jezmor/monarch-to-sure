package importer

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ParseSelection parses "1, 4-6" into {1, 4, 5, 6}, rejecting numbers
// outside 1..count.
func ParseSelection(text string, count int) (map[int]bool, error) {
	selected := map[int]bool{}
	for _, part := range strings.Split(strings.ReplaceAll(text, " ", ""), ",") {
		if part == "" {
			continue
		}
		lowText, highText, isRange := strings.Cut(part, "-")
		if !isRange || highText == "" {
			highText = lowText
		}
		low, errLow := strconv.Atoi(lowText)
		high, errHigh := strconv.Atoi(highText)
		if errLow != nil || errHigh != nil {
			return nil, fmt.Errorf("%q isn't a number or range (separate them with commas)", part)
		}
		if !(1 <= low && low <= high && high <= count) {
			return nil, fmt.Errorf("%s is not between 1 and %d", part, count)
		}
		for n := low; n <= high; n++ {
			selected[n] = true
		}
	}
	return selected, nil
}

// FormatSelection is the reverse of ParseSelection: {1, 4, 5, 6} is "1,4-6".
func FormatSelection(selected map[int]bool) string {
	numbers := make([]int, 0, len(selected))
	for n := range selected {
		numbers = append(numbers, n)
	}
	sort.Ints(numbers)
	var parts []string
	for i := 0; i < len(numbers); {
		j := i
		for j+1 < len(numbers) && numbers[j+1] == numbers[j]+1 {
			j++
		}
		if i == j {
			parts = append(parts, strconv.Itoa(numbers[i]))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", numbers[i], numbers[j]))
		}
		i = j + 1
	}
	return strings.Join(parts, ",")
}

// Ago describes how long before today a day was, such as "3mo ago".
func Ago(day, today time.Time) string {
	days := int(today.Sub(day).Hours() / 24)
	switch {
	case days < 60:
		return fmt.Sprintf("%dd ago", days)
	case days < 730:
		return fmt.Sprintf("%dmo ago", days/30)
	default:
		return fmt.Sprintf("%dy ago", days/365)
	}
}

// ByLastActivity returns account indexes, most recently active first.
func ByLastActivity(accounts []NamedAccount) []int {
	order := make([]int, len(accounts))
	for i := range order {
		order[i] = i
	}
	activity := func(i int) string {
		if a := accounts[i].Config.LastActivity; a != nil {
			return *a
		}
		return ""
	}
	sort.SliceStable(order, func(a, b int) bool { return activity(order[a]) > activity(order[b]) })
	return order
}

// ReviewAccounts asks which accounts are closed and which to skip.
//
// A closed account is imported disabled, and its balance drops to zero the
// day after Monarch's last balance for it. Sure hides disabled accounts from
// current balances but keeps their history in net worth. Long-inactive
// accounts are only suggested as closed, since inactivity alone doesn't
// prove an account was closed.
func ReviewAccounts(config *Config, exportEnd time.Time, ask func(string) (string, error), out func(string)) error {
	order := ByLastActivity(config.Accounts)
	width := len("Account")
	for _, account := range config.Accounts {
		if len(account.Name) > width {
			width = len(account.Name)
		}
	}
	out(fmt.Sprintf("\n  #  %-*s  %-14s  %-22s  %14s  Txns", width, "Account", "Type", "Last activity", "Latest balance"))
	for n, i := range order {
		cfg := config.Accounts[i].Config
		when := "never"
		if cfg.LastActivity != nil {
			day, _ := ParseDate(*cfg.LastActivity)
			when = fmt.Sprintf("%s (%s)", *cfg.LastActivity, Ago(day, exportEnd))
		}
		latestBalance := ""
		if cfg.LatestBalance != nil {
			latestBalance = *cfg.LatestBalance
		}
		out(fmt.Sprintf("%3d  %-*s  %-14s  %-22s  %14s  %d", n+1, width, config.Accounts[i].Name, cfg.Type, when, latestBalance, cfg.TxCount))
	}

	choose := func(prompt string, suggested map[int]bool) (map[int]bool, error) {
		def := FormatSelection(suggested)
		if def == "" {
			def = "none"
		}
		for {
			answer, err := ask(fmt.Sprintf("%s [%s]: ", prompt, def))
			if err != nil {
				return nil, err
			}
			answer = strings.TrimSpace(answer)
			switch {
			case answer == "":
				return suggested, nil
			case strings.EqualFold(answer, "none"):
				return map[int]bool{}, nil
			}
			selected, err := ParseSelection(answer, len(order))
			if err == nil {
				return selected, nil
			}
			out(fmt.Sprintf("  %s. Use numbers and ranges like 1,4-6, or none.", err))
		}
	}

	out("\nClosed accounts drop to zero after their last Monarch balance and are imported disabled:")
	out("hidden from current balances, but still part of net worth history. Other accounts stay")
	out("active and keep their last balance.")
	out(fmt.Sprintf("Suggested: accounts with no activity in the last %d days. Check each one.", StaleAfterDays))
	suggested := map[int]bool{}
	for n, i := range order {
		if config.Accounts[i].Config.SuggestClosed {
			suggested[n+1] = true
		}
	}
	closed, err := choose("Which accounts are closed?", suggested)
	if err != nil {
		return err
	}
	skipped, err := choose("Which accounts should be skipped entirely?", map[int]bool{})
	if err != nil {
		return err
	}
	for n, i := range order {
		cfg := &config.Accounts[i].Config
		cfg.Closed = closed[n+1]
		cfg.Status = "active"
		if cfg.Closed {
			cfg.Status = "disabled"
		}
		cfg.Include = !skipped[n+1]
	}
	return nil
}
