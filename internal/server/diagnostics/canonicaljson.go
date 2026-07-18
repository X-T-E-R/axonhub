package diagnostics

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"strconv"
	"strings"

	"github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"
)

var ErrRFC8785NumberOutOfDomain = errors.New("JSON number cannot be represented by RFC 8785 without integer precision loss")

// canonicalizeRFC8785 applies JSON Canonicalization Scheme (RFC 8785). JCS
// relies on the IEEE-754 binary64 number domain. Integral input values that
// would change value during binary64 conversion are rejected explicitly rather
// than silently rounded (notably integers above 2^53 that are not exactly
// representable).
func canonicalizeRFC8785(raw []byte) ([]byte, error) {
	if err := validateRFC8785NumberDomain(raw); err != nil {
		return nil, err
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, io.ErrUnexpectedEOF
	}
	if trimmed[0] == '{' || trimmed[0] == '[' {
		return jsoncanonicalizer.Transform(trimmed)
	}
	wrapped := make([]byte, 0, len(trimmed)+2)
	wrapped = append(wrapped, '[')
	wrapped = append(wrapped, trimmed...)
	wrapped = append(wrapped, ']')
	canonical, err := jsoncanonicalizer.Transform(wrapped)
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), canonical[1:len(canonical)-1]...), nil
}

func validateRFC8785NumberDomain(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		number, ok := token.(json.Number)
		if !ok {
			continue
		}
		if err := validateRFC8785Number(number.String()); err != nil {
			return err
		}
	}
}

func validateRFC8785Number(number string) error {
	value, err := strconv.ParseFloat(number, 64)
	if err != nil || math.IsInf(value, 0) || math.IsNaN(value) {
		return fmt.Errorf("%w: %s", ErrRFC8785NumberOutOfDomain, number)
	}
	exact, err := exactDecimal(number)
	if err != nil {
		return err
	}
	if !exact.IsInt() {
		return nil
	}
	represented := new(big.Rat).SetFloat64(value)
	if represented == nil || exact.Cmp(represented) != 0 {
		return fmt.Errorf("%w: %s", ErrRFC8785NumberOutOfDomain, number)
	}
	return nil
}

func exactDecimal(number string) (*big.Rat, error) {
	mantissa := number
	exponent := 0
	if index := strings.IndexAny(number, "eE"); index >= 0 {
		mantissa = number[:index]
		parsed, err := strconv.ParseInt(number[index+1:], 10, 32)
		if err != nil || parsed > 10000 || parsed < -10000 {
			return nil, fmt.Errorf("%w: %s", ErrRFC8785NumberOutOfDomain, number)
		}
		exponent = int(parsed)
	}
	value, ok := new(big.Rat).SetString(mantissa)
	if !ok {
		return nil, fmt.Errorf("invalid JSON number %q", number)
	}
	if exponent == 0 {
		return value, nil
	}
	powerExponent := exponent
	if powerExponent < 0 {
		powerExponent = -powerExponent
	}
	power := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(powerExponent)), nil)
	if exponent > 0 {
		value.Mul(value, new(big.Rat).SetInt(power))
	} else {
		value.Quo(value, new(big.Rat).SetInt(power))
	}
	return value, nil
}
