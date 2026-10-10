package prompt

import (
	"encoding/json"
	"errors"
	"strconv"
	"unicode/utf8"
)

// A minimal, allocation-free JSON scanner. Routing needs a handful of fields
// from bodies that are often tens of kilobytes, so instead of decoding the
// whole document it walks the bytes once and hands back sub-slices. It checks
// structure (balanced brackets, terminated strings), not full JSON validity:
// the body is forwarded unchanged, so the backend remains the judge of that.

var errSyntax = errors.New("malformed JSON")

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

func skipSpace(b []byte, i int) int {
	for i < len(b) && isSpace(b[i]) {
		i++
	}
	return i
}

// skipString returns the index just past the string starting at b[i] == '"'.
func skipString(b []byte, i int) (int, error) {
	for i++; i < len(b); i++ {
		switch b[i] {
		case '\\':
			i++
		case '"':
			return i + 1, nil
		}
	}
	return 0, errSyntax
}

// skipValue returns the index just past the value starting at b[i].
func skipValue(b []byte, i int) (int, error) {
	if i >= len(b) {
		return 0, errSyntax
	}
	switch b[i] {
	case '"':
		return skipString(b, i)
	case '{', '[':
		depth := 0
		for ; i < len(b); i++ {
			switch b[i] {
			case '"':
				end, err := skipString(b, i)
				if err != nil {
					return 0, err
				}
				i = end - 1
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1, nil
				}
			}
		}
		return 0, errSyntax
	default: // number, true, false, null
		start := i
		for i < len(b) && b[i] != ',' && b[i] != '}' && b[i] != ']' && !isSpace(b[i]) {
			i++
		}
		if i == start {
			return 0, errSyntax
		}
		return i, nil
	}
}

// eachField calls fn with each key (raw, without quotes) and value of the
// object in b. It stops early when fn returns false.
func eachField(b []byte, fn func(key, val []byte) bool) error {
	return eachFieldAt(b, func(key []byte, start, end int) bool { return fn(key, b[start:end]) })
}

// eachFieldAt is eachField with the value's position in b.
func eachFieldAt(b []byte, fn func(key []byte, start, end int) bool) error {
	i := skipSpace(b, 0)
	if i >= len(b) || b[i] != '{' {
		return errSyntax
	}
	i = skipSpace(b, i+1)
	if i < len(b) && b[i] == '}' {
		return nil
	}
	for i < len(b) {
		if b[i] != '"' {
			return errSyntax
		}
		kend, err := skipString(b, i)
		if err != nil {
			return err
		}
		key := b[i+1 : kend-1]
		i = skipSpace(b, kend)
		if i >= len(b) || b[i] != ':' {
			return errSyntax
		}
		vstart := skipSpace(b, i+1)
		vend, err := skipValue(b, vstart)
		if err != nil {
			return err
		}
		if !fn(key, vstart, vend) {
			return nil
		}
		i = skipSpace(b, vend)
		if i < len(b) && b[i] == '}' {
			return nil
		}
		if i >= len(b) || b[i] != ',' {
			return errSyntax
		}
		i = skipSpace(b, i+1)
	}
	return errSyntax
}

// eachElement calls fn with each element of the array in b.
func eachElement(b []byte, fn func(val []byte)) error {
	i := skipSpace(b, 0)
	if i >= len(b) || b[i] != '[' {
		return errSyntax
	}
	i = skipSpace(b, i+1)
	if i < len(b) && b[i] == ']' {
		return nil
	}
	for i < len(b) {
		end, err := skipValue(b, i)
		if err != nil {
			return err
		}
		fn(b[i:end])
		i = skipSpace(b, end)
		if i < len(b) && b[i] == ']' {
			return nil
		}
		if i >= len(b) || b[i] != ',' {
			return errSyntax
		}
		i = skipSpace(b, i+1)
	}
	return errSyntax
}

// field returns the value of key in object b, or nil.
func field(b []byte, key string) []byte {
	var out []byte
	eachField(b, func(k, v []byte) bool {
		if string(k) == key {
			out = v
			return false
		}
		return true
	})
	return out
}

// stringValue decodes a JSON string value; ok is false for any other type.
func stringValue(v []byte) (string, bool) {
	if len(v) < 2 || v[0] != '"' {
		return "", false
	}
	inner := v[1 : len(v)-1]
	for _, c := range inner {
		// Escapes and non-ASCII bytes take the exact decoder, which also
		// replaces invalid UTF-8 the way encoding/json does everywhere else.
		if c == '\\' || c >= utf8.RuneSelf {
			var s string
			if json.Unmarshal(v, &s) != nil {
				return "", false
			}
			return s, true
		}
	}
	return string(inner), true
}

func numberValue(v []byte) (float64, bool) {
	if len(v) == 0 || !(v[0] == '-' || (v[0] >= '0' && v[0] <= '9')) {
		return 0, false
	}
	f, err := strconv.ParseFloat(string(v), 64)
	return f, err == nil
}

// textOf is the prompt-matching form of a value: a string's contents without
// the quotes, anything else as raw JSON. Raw bytes are as good as decoded text
// for prefix matching, since a client resends earlier turns byte for byte.
func textOf(v []byte) []byte {
	if len(v) >= 2 && v[0] == '"' {
		return v[1 : len(v)-1]
	}
	if string(v) == "null" {
		return nil
	}
	return v
}

// ReplaceModel returns body with its top-level "model" string set to model,
// leaving every other byte as it was. ok is false if there is no such field.
func ReplaceModel(body []byte, model string) (out []byte, ok bool) {
	start, end := -1, -1
	eachFieldAt(body, func(k []byte, s, e int) bool {
		if string(k) == "model" && body[s] == '"' {
			start, end = s, e
			return false
		}
		return true
	})
	if start < 0 {
		return body, false
	}
	quoted, _ := json.Marshal(model)
	out = make([]byte, 0, len(body)-(end-start)+len(quoted))
	out = append(out, body[:start]...)
	out = append(out, quoted...)
	return append(out, body[end:]...), true
}
