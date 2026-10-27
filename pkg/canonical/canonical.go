// SPDX-License-Identifier: Apache-2.0
// Package canonical provides RFC 8785 Canonical JSON (JCS) serialization,
// UTF-16 code unit key sorting, zero-float enforcement, and
// balanced binary Merkle tree root calculation with odd-leaf duplication.

package canonical

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/shopspring/decimal"
)

var jsonNumberType = reflect.TypeOf(json.Number(""))

// isRepresentableIntFloat returns true if f is a finite float with no fractional
// part that fits safely within the signed 64-bit integer range [math.MinInt64, math.MaxInt64].
// Note: float64(math.MaxInt64) rounds up to 2^63 (9223372036854775808.0), which exceeds
// math.MaxInt64. Therefore, f must be strictly less than 9223372036854775808.0.
func isRepresentableIntFloat(f float64) bool {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return false
	}
	if f < float64(math.MinInt64) || f >= 9223372036854775808.0 {
		return false
	}
	return f == float64(int64(f))
}

// SHA256Bytes returns the lowercase hexadecimal SHA-256 digest of b.
func SHA256Bytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// SHA256Of serializes v to RFC 8785 canonical JSON and returns its SHA-256 hex digest.
// If canonical serialization fails, it deterministically hashes the error message.
func SHA256Of(v any) string {
	b, err := Marshal(v)
	if err != nil {
		sum := sha256.Sum256([]byte("canonical-error:" + err.Error()))
		return hex.EncodeToString(sum[:])
	}
	return SHA256Bytes(b)
}

// MerkleRoot computes a deterministic balanced binary Merkle tree root from an ordered
// slice of leaf SHA-256 hex digests. If leafHashes is empty, it returns the SHA-256 of empty bytes.
// If an odd number of hashes occurs at any level, the last hash is duplicated to form a pair.
func MerkleRoot(leafHashes []string) string {
	if len(leafHashes) == 0 {
		return SHA256Bytes(nil)
	}
	if len(leafHashes) == 1 {
		return leafHashes[0]
	}

	layer := make([]string, len(leafHashes))
	copy(layer, leafHashes)

	for len(layer) > 1 {
		next := make([]string, 0, (len(layer)+1)/2)
		for i := 0; i < len(layer); i += 2 {
			left := layer[i]
			right := left
			if i+1 < len(layer) {
				right = layer[i+1]
			}
			sum := sha256.Sum256([]byte(left + right))
			next = append(next, hex.EncodeToString(sum[:]))
		}
		layer = next
	}
	return layer[0]
}

// Marshal serializes v into RFC 8785 Canonical JSON format.
// Keys are sorted in UTF-16 code unit lexicographical order.
// Non-integer IEEE-754 floats are rejected with an error.
func Marshal(v any) ([]byte, error) {
	var b strings.Builder
	if err := writeValue(&b, v); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

func writeValue(b *strings.Builder, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
		return nil
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
		return nil
	case string:
		writeString(b, x)
		return nil
	case json.Number:
		s := string(x)
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || !isRepresentableIntFloat(f) {
			return fmt.Errorf("IEEE float forbidden in canonical JSON")
		}
		b.WriteString(strconv.FormatInt(int64(f), 10))
		return nil
	case []byte:
		writeString(b, hex.EncodeToString(x))
		return nil
	case decimal.Decimal:
		writeString(b, x.String())
		return nil
	case *decimal.Decimal:
		if x == nil {
			b.WriteString("null")
			return nil
		}
		writeString(b, x.String())
		return nil
	case int:
		b.WriteString(strconv.Itoa(x))
		return nil
	case int8:
		b.WriteString(strconv.FormatInt(int64(x), 10))
		return nil
	case int16:
		b.WriteString(strconv.FormatInt(int64(x), 10))
		return nil
	case int32:
		b.WriteString(strconv.FormatInt(int64(x), 10))
		return nil
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
		return nil
	case uint:
		b.WriteString(strconv.FormatUint(uint64(x), 10))
		return nil
	case uint8:
		b.WriteString(strconv.FormatUint(uint64(x), 10))
		return nil
	case uint16:
		b.WriteString(strconv.FormatUint(uint64(x), 10))
		return nil
	case uint32:
		b.WriteString(strconv.FormatUint(uint64(x), 10))
		return nil
	case uint64:
		b.WriteString(strconv.FormatUint(x, 10))
		return nil
	case float64:
		if isRepresentableIntFloat(x) {
			b.WriteString(strconv.FormatInt(int64(x), 10))
			return nil
		}
		return fmt.Errorf("IEEE float forbidden in canonical JSON")
	case float32:
		f64 := float64(x)
		if isRepresentableIntFloat(f64) {
			b.WriteString(strconv.FormatInt(int64(f64), 10))
			return nil
		}
		return fmt.Errorf("IEEE float forbidden in canonical JSON")
	case []any:
		b.WriteByte('[')
		for i, item := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeValue(b, item); err != nil {
				return err
			}
		}
		b.WriteByte(']')
		return nil
	case []string:
		b.WriteByte('[')
		for i, item := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			writeString(b, item)
		}
		b.WriteByte(']')
		return nil
	case []int:
		b.WriteByte('[')
		for i, item := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(strconv.FormatInt(int64(item), 10))
		}
		b.WriteByte(']')
		return nil
	case []int64:
		b.WriteByte('[')
		for i, item := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(strconv.FormatInt(item, 10))
		}
		b.WriteByte(']')
		return nil
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			return utf16Less(keys[i], keys[j])
		})
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writeString(b, k)
			b.WriteByte(':')
			if err := writeValue(b, x[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
		return nil
	default:
		// Universal reflection fallback for structs, typed maps, typed slices, and aliases
		return writeReflectValue(b, reflect.ValueOf(v))
	}
}

func writeReflectValue(b *strings.Builder, val reflect.Value) error {
	if !val.IsValid() {
		b.WriteString("null")
		return nil
	}

	switch val.Kind() {
	case reflect.Pointer, reflect.Interface:
		if val.IsNil() {
			b.WriteString("null")
			return nil
		}
		return writeReflectValue(b, val.Elem())
	case reflect.Bool:
		if val.Bool() {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
		return nil
	case reflect.String:
		if val.Type() == jsonNumberType {
			s := val.String()
			f, err := strconv.ParseFloat(s, 64)
			if err != nil || !isRepresentableIntFloat(f) {
				return fmt.Errorf("IEEE float forbidden in canonical JSON")
			}
			b.WriteString(strconv.FormatInt(int64(f), 10))
			return nil
		}
		writeString(b, val.String())
		return nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		b.WriteString(strconv.FormatInt(val.Int(), 10))
		return nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		b.WriteString(strconv.FormatUint(val.Uint(), 10))
		return nil
	case reflect.Float32, reflect.Float64:
		f := val.Float()
		if isRepresentableIntFloat(f) {
			b.WriteString(strconv.FormatInt(int64(f), 10))
			return nil
		}
		return fmt.Errorf("IEEE float forbidden in canonical JSON")
	case reflect.Slice, reflect.Array:
		b.WriteByte('[')
		for i := 0; i < val.Len(); i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeReflectValue(b, val.Index(i)); err != nil {
				return err
			}
		}
		b.WriteByte(']')
		return nil
	case reflect.Map:
		if val.Type().Key().Kind() != reflect.String {
			return fmt.Errorf("unsupported canonical map key type %s", val.Type().Key().Kind())
		}
		keys := make([]string, 0, val.Len())
		for _, k := range val.MapKeys() {
			keys = append(keys, k.String())
		}
		sort.Slice(keys, func(i, j int) bool {
			return utf16Less(keys[i], keys[j])
		})
		keyType := val.Type().Key()
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writeString(b, k)
			b.WriteByte(':')
			keyVal := reflect.ValueOf(k)
			if keyVal.Type() != keyType {
				keyVal = keyVal.Convert(keyType)
			}
			v := val.MapIndex(keyVal)
			if err := writeReflectValue(b, v); err != nil {
				return err
			}
		}
		b.WriteByte('}')
		return nil
	case reflect.Struct:
		type fieldEntry struct {
			key   string
			value reflect.Value
		}
		t := val.Type()
		var entries []fieldEntry
		for i := 0; i < val.NumField(); i++ {
			sf := t.Field(i)
			if sf.PkgPath != "" {
				continue // unexported field
			}
			tag := sf.Tag.Get("json")
			if tag == "-" {
				continue
			}
			parts := strings.Split(tag, ",")
			name := parts[0]
			if name == "" {
				name = sf.Name
			}
			fVal := val.Field(i)
			if len(parts) > 1 && parts[1] == "omitempty" {
				if fVal.IsZero() {
					continue
				}
			}
			entries = append(entries, fieldEntry{key: name, value: fVal})
		}
		sort.Slice(entries, func(i, j int) bool {
			return utf16Less(entries[i].key, entries[j].key)
		})
		b.WriteByte('{')
		for i, entry := range entries {
			if i > 0 {
				b.WriteByte(',')
			}
			writeString(b, entry.key)
			b.WriteByte(':')
			if err := writeReflectValue(b, entry.value); err != nil {
				return err
			}
		}
		b.WriteByte('}')
		return nil
	default:
		return fmt.Errorf("unsupported canonical type %s", val.Type().String())
	}
}

type utf16Iter struct {
	s   string
	pos int
	low uint16
}

func (it *utf16Iter) next() (uint16, bool) {
	if it.low != 0 {
		v := it.low
		it.low = 0
		return v, true
	}
	if it.pos >= len(it.s) {
		return 0, false
	}
	b := it.s[it.pos]
	if b < 0x80 {
		it.pos++
		return uint16(b), true
	}
	r, size := utf8.DecodeRuneInString(it.s[it.pos:])
	it.pos += size
	if r <= 0xFFFF {
		return uint16(r), true
	}
	r -= 0x10000
	it.low = uint16(0xDC00 + (r & 0x3FF))
	return uint16(0xD800 + (r >> 10)), true
}

func utf16Less(a, b string) bool {
	itA := utf16Iter{s: a}
	itB := utf16Iter{s: b}
	for {
		uA, okA := itA.next()
		uB, okB := itB.next()
		if !okA || !okB {
			return !okA && okB
		}
		if uA != uB {
			return uA < uB
		}
	}
}

const hexDigits = "0123456789abcdef"

func writeString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				b.WriteString(`\u00`)
				b.WriteByte(hexDigits[(r>>4)&0x0F])
				b.WriteByte(hexDigits[r&0x0F])
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

// ForbidFloat walks a value tree and errors if a non-integer float (or NaN/Inf) is present.
func ForbidFloat(v any, path string) error {
	if v == nil {
		return nil
	}
	val := reflect.ValueOf(v)
	return checkFloatValue(val, path)
}

func checkFloatValue(val reflect.Value, path string) error {
	if !val.IsValid() {
		return nil
	}

	switch val.Kind() {
	case reflect.Pointer, reflect.Interface:
		if val.IsNil() {
			return nil
		}
		return checkFloatValue(val.Elem(), path)
	case reflect.String:
		if val.Type() == jsonNumberType {
			s := val.String()
			f, err := strconv.ParseFloat(s, 64)
			if err != nil || !isRepresentableIntFloat(f) {
				if path == "" {
					return fmt.Errorf("IEEE float detected")
				}
				return fmt.Errorf("IEEE float at %s", path)
			}
		}
		return nil
	case reflect.Float32, reflect.Float64:
		f := val.Float()
		if !isRepresentableIntFloat(f) {
			if path == "" {
				return fmt.Errorf("IEEE float detected")
			}
			return fmt.Errorf("IEEE float at %s", path)
		}
		return nil
	case reflect.Slice, reflect.Array:
		for i := 0; i < val.Len(); i++ {
			subPath := fmt.Sprintf("%s[%d]", path, i)
			if err := checkFloatValue(val.Index(i), subPath); err != nil {
				return err
			}
		}
	case reflect.Map:
		for _, k := range val.MapKeys() {
			subPath := k.String()
			if path != "" {
				subPath = path + "." + subPath
			}
			if err := checkFloatValue(val.MapIndex(k), subPath); err != nil {
				return err
			}
		}
	case reflect.Struct:
		t := val.Type()
		for i := 0; i < val.NumField(); i++ {
			sf := t.Field(i)
			if sf.PkgPath != "" {
				continue
			}
			name := sf.Name
			tag := sf.Tag.Get("json")
			if tag == "-" {
				continue
			}
			if tag != "" {
				parts := strings.Split(tag, ",")
				if parts[0] != "" {
					name = parts[0]
				}
			}
			subPath := name
			if path != "" {
				subPath = path + "." + name
			}
			if err := checkFloatValue(val.Field(i), subPath); err != nil {
				return err
			}
		}
	}
	return nil
}

// IsFiniteFloat returns true if v is neither NaN nor Infinite.
func IsFiniteFloat(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

// NormalizeLF converts all CRLF (\r\n) and CR (\r) line endings to uniform LF (\n) in memory.
// This prevents cross-platform desynchronization, line drift, and unified diff misalignment.
func NormalizeLF(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

// NormalizeLFBytes converts all CRLF and CR line endings in a byte slice to uniform LF bytes.
func NormalizeLFBytes(b []byte) []byte {
	return []byte(NormalizeLF(string(b)))
}

