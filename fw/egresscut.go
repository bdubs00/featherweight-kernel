package fw

// This file cuts the long string values of an event that the egress
// publishes (Config.Bus.EgressMaxBytes, docs/ARCHITECTURE.md section 9.4),
// so a stream that carries fw.tool.requested and fw.tool.responded does
// not carry whole tool inputs and outputs.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

// cutStrings returns data with each string value longer than max bytes
// cut to its first max bytes, at a rune boundary, and followed by the note
// " [cut N of M bytes]", where N is the number of bytes cut and M the
// length of the whole value. It walks nested objects and arrays, and it
// leaves the keys of an object and every value that is not a string as
// they are. The result is a new slice; data stays as it is.
//
// A quick scan of the bytes comes first: data without a string literal
// longer than max returns unchanged and is never decoded, because a
// decoded string is never longer than its literal. Only data with a long
// literal is decoded and encoded again; the keys of its objects then come
// in sorted order. Data that does not decode returns unchanged.
func cutStrings(data json.RawMessage, max int) json.RawMessage {
	if max <= 0 || !longLiteral(data, max) {
		return data
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return data
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(cutValue(v, max)); err != nil {
		return data
	}
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n"))
}

// longLiteral reports whether the JSON text data holds a string literal
// whose decoded value can be longer than max bytes. The bytes of a literal
// bound its value, except for bytes that are not UTF-8, which decode to
// three bytes each.
func longLiteral(data []byte, max int) bool {
	for i := 0; i < len(data); i++ {
		if data[i] != '"' {
			continue
		}
		start := i + 1
		for i = start; i < len(data) && data[i] != '"'; i++ {
			if data[i] == '\\' {
				i++
			}
		}
		end := min(i, len(data))
		if n := end - start; n > max || (n*3 > max && !utf8.Valid(data[start:end])) {
			return true
		}
	}
	return false
}

// cutValue cuts each string of a decoded JSON value in place.
func cutValue(v any, max int) any {
	switch x := v.(type) {
	case string:
		return cutString(x, max)
	case map[string]any:
		for k, e := range x {
			x[k] = cutValue(e, max)
		}
	case []any:
		for i, e := range x {
			x[i] = cutValue(e, max)
		}
	}
	return v
}

// cutString cuts s to at most max bytes at a rune boundary and notes the
// cut.
func cutString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	i := max
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i] + fmt.Sprintf(" [cut %d of %d bytes]", len(s)-i, len(s))
}
