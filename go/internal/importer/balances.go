package importer

import (
	"math/big"
	"sort"
	"time"
)

// Flows maps a day to an account's net transaction amount, inflows positive.
type Flows map[time.Time]*big.Rat

// Anchor is a balance valuation: the end-of-day balance on Date.
type Anchor struct {
	Date  time.Time
	Value *big.Rat
}

// BalanceAnchors returns the valuations Sure needs to reproduce a Monarch
// balance history.
//
// Both inputs use Monarch signs. Sure carries a balance forward by applying
// transactions and resets it on any valuation, so an anchor is only emitted
// where Monarch's balance differs from that running total. The opening
// anchor comes first.
func BalanceAnchors(history History, flows Flows, closeAfter bool) []Anchor {
	if len(history) == 0 && len(flows) == 0 {
		return nil
	}
	balanceDays, flowDays := sortedDays(history), sortedDays(flows)

	var anchors []Anchor
	var running *big.Rat

	if len(flowDays) > 0 && (len(balanceDays) == 0 || flowDays[0].Before(balanceDays[0])) {
		// Transactions predate the balance history: work backwards from the
		// first known balance so earlier transactions land on the right total.
		opening := zero()
		if len(balanceDays) > 0 {
			first := balanceDays[0]
			opening = new(big.Rat).Set(history[first].Rat)
			for d, v := range flows {
				if !d.After(first) {
					opening = sub(opening, v)
				}
			}
		}
		anchors = append(anchors, Anchor{flowDays[0].AddDate(0, 0, -1), opening})
		running = opening
	}

	days := map[time.Time]bool{}
	for _, d := range balanceDays {
		days[d] = true
	}
	for _, d := range flowDays {
		days[d] = true
	}
	for _, day := range sortedDays(days) {
		var expected *big.Rat
		if running != nil {
			expected = running
			if flow, ok := flows[day]; ok {
				expected = add(running, flow)
			}
		}
		actual, ok := history[day]
		if ok && (expected == nil || !equal(actual.Rat, expected)) {
			anchors = append(anchors, Anchor{day, actual.Rat})
			running = actual.Rat
		} else {
			running = expected
		}
	}

	if closeAfter && len(balanceDays) > 0 {
		last := balanceDays[len(balanceDays)-1]
		laterFlows := len(flowDays) > 0 && flowDays[len(flowDays)-1].After(last)
		if history[last].Rat.Sign() != 0 && !laterFlows {
			anchors = append(anchors, Anchor{nextDay(last), zero()})
		}
	}
	return anchors
}

// Replay returns each day's balance the way Sure's forward calculator derives
// it: a valuation sets the end-of-day balance outright, and any other day adds
// that day's transactions to the previous balance. end extends the replay to
// at least that day when set.
func Replay(anchors []Anchor, flows Flows, end *time.Time) map[time.Time]*big.Rat {
	if len(anchors) == 0 {
		return map[time.Time]*big.Rat{}
	}
	valuations := map[time.Time]*big.Rat{}
	start := anchors[0].Date
	last := start
	for _, a := range anchors {
		valuations[a.Date] = a.Value
		if a.Date.After(last) {
			last = a.Date
		}
	}
	for d := range flows {
		if d.After(last) {
			last = d
		}
	}
	if end != nil && end.After(last) {
		last = *end
	}

	balances := map[time.Time]*big.Rat{}
	running := zero()
	for day := start; !day.After(last); day = nextDay(day) {
		if v, ok := valuations[day]; ok {
			running = v
		} else if flow, ok := flows[day]; ok {
			running = add(running, flow)
		}
		balances[day] = running
	}
	return balances
}

// WithCurrentBalance adds an anchor so the replayed balance on asOf equals
// balance, leaving anchors unchanged when the history already ends there.
func WithCurrentBalance(anchors []Anchor, flows Flows, asOf time.Time, balance *big.Rat) []Anchor {
	if len(anchors) > 0 {
		if current, ok := Replay(anchors, flows, &asOf)[asOf]; ok && equal(current, balance) {
			return anchors
		}
	}
	var updated []Anchor
	for _, a := range anchors {
		if !a.Date.Equal(asOf) {
			updated = append(updated, a)
		}
	}
	updated = append(updated, Anchor{asOf, balance})
	sort.Slice(updated, func(i, j int) bool { return updated[i].Date.Before(updated[j].Date) })
	return updated
}

func latest(balances map[time.Time]*big.Rat) *big.Rat {
	days := sortedDays(balances)
	if len(days) == 0 {
		return zero()
	}
	return balances[days[len(days)-1]]
}
