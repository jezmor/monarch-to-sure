// Package importer turns Monarch's CSV exports into Sure's NDJSON import
// records. It mirrors the Python runtime's monarch_to_sure.py, and the two
// produce byte-identical NDJSON for the same input.
package importer

import (
	"encoding/csv"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

const dateLayout = "2006-01-02"

// Row is one CSV record keyed by column name.
type Row map[string]string

// ReadRows reads a CSV with a header row, ignoring a UTF-8 byte order mark.
func ReadRows(r io.Reader) ([]Row, error) {
	reader := csv.NewReader(r)
	reader.FieldsPerRecord = -1
	header, err := reader.Read()
	if err == io.EOF {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(header) > 0 {
		header[0] = strings.TrimPrefix(header[0], "\ufeff")
	}
	var rows []Row
	for {
		record, err := reader.Read()
		if err == io.EOF {
			return rows, nil
		}
		if err != nil {
			return nil, err
		}
		row := make(Row, len(header))
		for i, name := range header {
			if i < len(record) {
				row[name] = record[i]
			}
		}
		rows = append(rows, row)
	}
}

// Header returns a CSV's column names.
func Header(r io.Reader) ([]string, error) {
	header, err := csv.NewReader(r).Read()
	if err == io.EOF {
		return nil, nil
	}
	if len(header) > 0 {
		header[0] = strings.TrimPrefix(header[0], "\ufeff")
	}
	return header, err
}

// ParseDate reads Monarch's ISO dates.
func ParseDate(value string) (time.Time, error) {
	d, err := time.Parse(dateLayout, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("not a date: %q", value)
	}
	return d, nil
}

func formatDate(d time.Time) string { return d.Format(dateLayout) }

func nextDay(d time.Time) time.Time { return d.AddDate(0, 0, 1) }

// History maps a day to an account's balance in Monarch's signs.
type History map[time.Time]*Amount

// Balances maps an account name to its balance history.
type Balances map[string]History

// ParseBalances reads the balances export, skipping blank balances.
func ParseBalances(rows []Row) (Balances, error) {
	balances := Balances{}
	for _, row := range rows {
		amount, err := ParseAmount(row["Balance"])
		if err != nil {
			return nil, err
		}
		if amount == nil {
			continue
		}
		day, err := ParseDate(row["Date"])
		if err != nil {
			return nil, err
		}
		if balances[row["Account"]] == nil {
			balances[row["Account"]] = History{}
		}
		balances[row["Account"]][day] = amount
	}
	return balances, nil
}

// WithoutPending drops pending transactions unless includePending is set.
func WithoutPending(rows []Row, includePending bool) []Row {
	if includePending {
		return rows
	}
	var kept []Row
	for _, row := range rows {
		if strings.ToLower(strings.TrimSpace(row["Pending"])) != "true" {
			kept = append(kept, row)
		}
	}
	return kept
}

func splitTags(value string) []string {
	var tags []string
	for _, tag := range strings.Split(value, ",") {
		if tag = strings.TrimSpace(tag); tag != "" {
			tags = append(tags, tag)
		}
	}
	return tags
}

func sortedDays[V any](m map[time.Time]V) []time.Time {
	days := make([]time.Time, 0, len(m))
	for d := range m {
		days = append(days, d)
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Before(days[j]) })
	return days
}
