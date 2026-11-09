package importer

import (
	"fmt"
	"math/big"
	"strings"
)

// Amounts are exact rationals so that balances compare and add without
// floating-point drift, the way Python's Decimal does in the Python runtime.

// Amount keeps the cleaned text it was parsed from, so a balance can be shown
// as Monarch exported it.
type Amount struct {
	Rat  *big.Rat
	Text string
}

// ParseAmount returns nil for a blank value.
func ParseAmount(value string) (*Amount, error) {
	raw := strings.TrimSpace(strings.NewReplacer(",", "", "$", "").Replace(value))
	if raw == "" {
		return nil, nil
	}
	r, ok := new(big.Rat).SetString(raw)
	if !ok {
		return nil, fmt.Errorf("not a number: %q", value)
	}
	return &Amount{Rat: r, Text: raw}, nil
}

func zero() *big.Rat { return new(big.Rat) }

func add(a, b *big.Rat) *big.Rat { return new(big.Rat).Add(a, b) }

func sub(a, b *big.Rat) *big.Rat { return new(big.Rat).Sub(a, b) }

func mul(a, b *big.Rat) *big.Rat { return new(big.Rat).Mul(a, b) }

func neg(a *big.Rat) *big.Rat { return new(big.Rat).Neg(a) }

func equal(a, b *big.Rat) bool { return a.Cmp(b) == 0 }

// Money formats an amount with two decimals, rounding half to even, and
// never as -0.00.
func Money(value *big.Rat) string {
	cents := new(big.Rat).Mul(value, big.NewRat(100, 1))
	num := new(big.Int).Abs(cents.Num())
	q, rem := new(big.Int).QuoRem(num, cents.Denom(), new(big.Int))
	switch new(big.Int).Mul(rem, big.NewInt(2)).Cmp(cents.Denom()) {
	case 1:
		q.Add(q, big.NewInt(1))
	case 0:
		if q.Bit(0) == 1 {
			q.Add(q, big.NewInt(1))
		}
	}
	sign := ""
	if cents.Sign() < 0 && q.Sign() != 0 {
		sign = "-"
	}
	whole, frac := new(big.Int).QuoRem(q, big.NewInt(100), new(big.Int))
	return fmt.Sprintf("%s%s.%02d", sign, whole.String(), frac.Int64())
}
