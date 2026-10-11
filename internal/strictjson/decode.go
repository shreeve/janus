// Package strictjson decodes one JSON value with no unknown struct fields or
// repeated object keys (compared the way encoding/json matches field names:
// case-insensitively). Field-specific absence/null rules belong to callers.
package strictjson

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode"
)

func Decode(data []byte, value any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := checkValue(dec, 0); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("trailing data after JSON value")
	}
	dec = json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(value)
}

// foldKey maps a key to one representative of its case-folding class: each
// rune becomes the smallest rune in its unicode.SimpleFold orbit. Two keys
// share a fold key exactly when strings.EqualFold reports them equal, which
// is how encoding/json matches object keys to struct fields — so
// {"upstreams":…,"UPSTREAMS":…} (or a Kelvin sign standing in for "k") is a
// repeated key, never a silent last-wins.
func foldKey(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		least := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			least = min(least, f)
		}
		b.WriteRune(least)
	}
	return b.String()
}

func checkValue(dec *json.Decoder, depth int) error {
	// Match encoding/json's nesting limit without allowing an unbounded
	// recursive walk through attacker-controlled control bodies.
	if depth > 10000 {
		return fmt.Errorf("JSON nesting exceeds maximum depth")
	}
	token, err := dec.Token()
	if err != nil {
		return err
	}
	switch token {
	case json.Delim('{'):
		seen := map[string]string{} // fold key → first spelling
		for dec.More() {
			key, err := dec.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok {
				return fmt.Errorf("expected object key")
			}
			folded := foldKey(name)
			if first, dup := seen[folded]; dup {
				if first == name {
					return fmt.Errorf("key %q appears twice", name)
				}
				return fmt.Errorf("key %q repeats key %q (object keys match field names case-insensitively)", name, first)
			}
			seen[folded] = name
			if err := checkValue(dec, depth+1); err != nil {
				return err
			}
		}
		_, err = dec.Token()
	case json.Delim('['):
		for dec.More() {
			if err := checkValue(dec, depth+1); err != nil {
				return err
			}
		}
		_, err = dec.Token()
	}
	return err
}
