package spinlayout

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

// Go's encoding/json matches struct fields case-insensitively, keeps the
// last of duplicate keys and replaces invalid UTF-8; Spin's serde matches
// exactly, rejects duplicates in typed structs and rejects invalid UTF-8.
// The converter must see the same document Spin sees, so every JSON input is
// checked here first and then read only as maps with exact keys.

// decodeStrict parses doc into generic values (objects as map[string]any,
// numbers as json.Number) after checking that it is valid UTF-8, a single
// value, and has no duplicate keys in any object.
func decodeStrict(doc []byte) (any, error) {
	if !utf8.Valid(doc) {
		return nil, errors.New("invalid UTF-8")
	}
	if err := checkDuplicates(doc); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data")
	}
	return v, nil
}

// checkDuplicates walks the token stream and fails on a repeated key in any
// object.
func checkDuplicates(doc []byte) error {
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.UseNumber()
	type frame struct {
		keys     map[string]bool
		isObject bool
		wantKey  bool
	}
	var stack []*frame
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		var top *frame
		if len(stack) > 0 {
			top = stack[len(stack)-1]
		}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{':
				if top != nil && top.isObject {
					top.wantKey = true
				}
				stack = append(stack, &frame{keys: map[string]bool{}, isObject: true, wantKey: true})
			case '[':
				if top != nil && top.isObject {
					top.wantKey = true
				}
				stack = append(stack, &frame{})
			case '}', ']':
				stack = stack[:len(stack)-1]
			}
		default:
			if top != nil && top.isObject && top.wantKey {
				k, ok := tok.(string)
				if !ok {
					return errors.New("non-string key")
				}
				if top.keys[k] {
					return errors.New("duplicate key")
				}
				top.keys[k] = true
				top.wantKey = false
			} else if top != nil && top.isObject {
				top.wantKey = true
			}
		}
	}
}

// object returns v as an object whose keys may only differ in case from
// none of the known struct fields: a key that equals a known field
// case-insensitively must equal it exactly. Unknown keys are allowed (serde
// ignores them).
func object(v any, known ...string) (map[string]any, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("not an object")
	}
	for k := range m {
		for _, f := range known {
			if k != f && strings.EqualFold(k, f) {
				return nil, errors.New("case variant of a known field")
			}
		}
	}
	return m, nil
}

// str returns m[k] as a string; present reports whether the key exists.
func str(m map[string]any, k string) (s string, present bool, err error) {
	v, ok := m[k]
	if !ok {
		return "", false, nil
	}
	s, ok = v.(string)
	if !ok {
		return "", true, errors.New(k + " is not a string")
	}
	return s, true, nil
}

// integer returns m[k] as a non-negative integer.
func integer(m map[string]any, k string) (int64, bool, error) {
	v, ok := m[k]
	if !ok {
		return 0, false, nil
	}
	n, ok := v.(json.Number)
	if !ok {
		return 0, true, errors.New(k + " is not a number")
	}
	i, err := n.Int64()
	if err != nil || i < 0 {
		return 0, true, errors.New(k + " is not a non-negative integer")
	}
	return i, true, nil
}

// array returns m[k] as an array.
func array(m map[string]any, k string) ([]any, bool, error) {
	v, ok := m[k]
	if !ok {
		return nil, false, nil
	}
	a, ok := v.([]any)
	if !ok {
		return nil, true, errors.New(k + " is not an array")
	}
	return a, true, nil
}

// encode marshals a generic value. Map keys come out sorted; json.Number
// values keep their text.
func encode(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
