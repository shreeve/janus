// Package strictjson decodes one JSON value with no unknown struct fields or
// repeated object keys. Field-specific absence/null rules belong to callers.
package strictjson

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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
		seen := map[string]bool{}
		for dec.More() {
			key, err := dec.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok {
				return fmt.Errorf("expected object key")
			}
			if seen[name] {
				return fmt.Errorf("key %q appears twice", name)
			}
			seen[name] = true
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
