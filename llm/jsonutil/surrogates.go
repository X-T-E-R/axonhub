// Package jsonutil provides byte-preserving JSON utilities.
package jsonutil

// NormalizeSurrogates replaces unpaired UTF-16 surrogate escapes in JSON strings
// with \ufffd, matching encoding/json's string decoding. Valid pairs and every
// other byte (including escaped backslashes, numbers and whitespace) are retained.
// It does not validate JSON. The input is never modified; unchanged input is
// returned without an allocation.
func NormalizeSurrogates(data []byte) []byte {
	out := data
	copied := false
	inString := false
	for i := 0; i < len(data); i++ {
		if data[i] == '"' {
			inString = !inString
			continue
		}
		if !inString || data[i] != '\\' {
			continue
		}
		u, ok := unicodeEscape(data[i:])
		if !ok {
			i++ // Skip an escaped quote/backslash, not the start of another escape.
			continue
		}
		if u >= 0xd800 && u <= 0xdbff {
			if low, ok := unicodeEscape(data[i+6:]); ok && low >= 0xdc00 && low <= 0xdfff {
				i += 11
				continue
			}
		} else if u < 0xdc00 || u > 0xdfff {
			i += 5
			continue
		}
		if !copied {
			out = append([]byte(nil), data...)
			copied = true
		}
		copy(out[i:i+6], `\ufffd`)
		i += 5
	}
	return out
}

func unicodeEscape(data []byte) (uint16, bool) {
	if len(data) < 6 || data[0] != '\\' || data[1] != 'u' {
		return 0, false
	}
	var value uint16
	for _, b := range data[2:6] {
		value <<= 4
		switch {
		case b >= '0' && b <= '9':
			value |= uint16(b - '0')
		case b >= 'a' && b <= 'f':
			value |= uint16(b - 'a' + 10)
		case b >= 'A' && b <= 'F':
			value |= uint16(b - 'A' + 10)
		default:
			return 0, false
		}
	}
	return value, true
}
