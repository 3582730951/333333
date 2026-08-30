package pricing

import (
	"bytes"
	"encoding/json"
	"errors"
	"math/big"
)

const (
	MicroUSDScale     int64 = 1_000_000
	MilliCreditScale  int64 = 1_000
	PerMillionDenom   int64 = 1_000_000
	MultiplierDenom   int64 = 1_000
	DefaultMultiplier int64 = 1_000
)

var (
	ErrNegativeAmount = errors.New("pricing values must be non-negative")
	ErrAmountOverflow = errors.New("pricing result exceeds int64")
	ErrInvalidScale   = errors.New("pricing scale must be positive")
)

// MicroUSD and MilliCredit are intentionally distinct types. A ChatGPT credit
// multiplier can therefore never be applied accidentally to an API dollar value.
type MicroUSD int64
type MilliCredit int64

func (value MicroUSD) MarshalJSON() ([]byte, error)    { return json.Marshal(int64(value)) }
func (value MilliCredit) MarshalJSON() ([]byte, error) { return json.Marshal(int64(value)) }

func (value *MicroUSD) UnmarshalJSON(raw []byte) error {
	parsed, err := parseJSONInt64(raw)
	if err == nil {
		*value = MicroUSD(parsed)
	}
	return err
}

func (value *MilliCredit) UnmarshalJSON(raw []byte) error {
	parsed, err := parseJSONInt64(raw)
	if err == nil {
		*value = MilliCredit(parsed)
	}
	return err
}

func parseJSONInt64(raw []byte) (int64, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var number json.Number
	if err := decoder.Decode(&number); err != nil {
		return 0, err
	}
	return number.Int64()
}

type WeightedComponent struct {
	Units int64
	Rate  int64
}

// RoundWeightedSumEven computes sum(units*rate)/denominator with exact integer
// intermediates and IEEE-style ties-to-even rounding. math/big is deliberate:
// one unusually large context must not overflow before the final int64 guard.
func RoundWeightedSumEven(components []WeightedComponent, denominator int64) (int64, error) {
	if denominator <= 0 {
		return 0, ErrInvalidScale
	}
	numerator := new(big.Int)
	for _, component := range components {
		if component.Units < 0 || component.Rate < 0 {
			return 0, ErrNegativeAmount
		}
		term := new(big.Int).Mul(big.NewInt(component.Units), big.NewInt(component.Rate))
		numerator.Add(numerator, term)
	}
	return roundBigRatioEven(numerator, big.NewInt(denominator))
}

func MultiplyRatioEven(value, multiplier, denominator int64) (int64, error) {
	if value < 0 || multiplier < 0 {
		return 0, ErrNegativeAmount
	}
	if denominator <= 0 {
		return 0, ErrInvalidScale
	}
	numerator := new(big.Int).Mul(big.NewInt(value), big.NewInt(multiplier))
	return roundBigRatioEven(numerator, big.NewInt(denominator))
}

func roundBigRatioEven(numerator, denominator *big.Int) (int64, error) {
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(numerator, denominator, remainder)
	twiceRemainder := new(big.Int).Lsh(new(big.Int).Set(remainder), 1)
	comparison := twiceRemainder.Cmp(denominator)
	if comparison > 0 || (comparison == 0 && quotient.Bit(0) == 1) {
		quotient.Add(quotient, big.NewInt(1))
	}
	if !quotient.IsInt64() {
		return 0, ErrAmountOverflow
	}
	return quotient.Int64(), nil
}
