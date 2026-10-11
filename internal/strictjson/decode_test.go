package strictjson

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestDecodePreservesAbsentAndNull(t *testing.T) {
	var value struct {
		Field json.RawMessage `json:"field"`
	}
	if err := Decode([]byte(`{}`), &value); err != nil || value.Field != nil {
		t.Fatalf("absent: %s %v", value.Field, err)
	}
	if err := Decode([]byte(`{"field":null}`), &value); err != nil || string(value.Field) != "null" {
		t.Fatalf("null: %s %v", value.Field, err)
	}
}

func TestDecodeRejectsMalformedAndNestedDuplicates(t *testing.T) {
	for _, body := range []string{`{"a":{"x":1,"x":2}}`, `[{"x":1,"x":2}]`, `{} {}`, `{}]`, `[}`, `{"x":}`, strings.Repeat("[", 10001) + strings.Repeat("]", 10001)} {
		var value any
		if err := Decode([]byte(body), &value); err == nil {
			t.Errorf("accepted %.80s", body)
		}
	}
	var value struct {
		Name string `json:"name"`
	}
	if err := Decode([]byte(`{"unknown":1}`), &value); err == nil {
		t.Fatal("accepted unknown field")
	}
}

func TestDecodeRejectsCaseFoldedDuplicates(t *testing.T) {
	for _, body := range []string{
		`{"upstreams":[1],"UPSTREAMS":[2]}`,
		`{"upstreams":[1],"Upstreams":[2]}`,
		`{"a":{"Name":1,"nAME":2}}`,
		`[{"x":1},{"y":{"k":1,"K":2}}]`, // Kelvin sign folds to k
		`{"s":1,"ſ":2}`,                 // long s folds to s
	} {
		var value any
		if err := Decode([]byte(body), &value); err == nil {
			t.Errorf("accepted %s", body)
		}
	}
	var target struct {
		Upstreams []int `json:"upstreams"`
	}
	if err := Decode([]byte(`{"upstreams":[1],"UPSTREAMS":[2]}`), &target); err == nil {
		t.Fatalf("decoded a case-folded repeat into a struct: %+v", target)
	}
	for _, body := range []string{`{"a":1,"b":2}`, `{"a":{"a":1},"A2":[{"a":1}]}`, `{"ab":1,"ba":2}`} {
		var value any
		if err := Decode([]byte(body), &value); err != nil {
			t.Errorf("rejected distinct keys %s: %v", body, err)
		}
	}
}

// foldedDuplicate is the fuzz oracle, written independently of checkValue:
// it walks the token stream and compares every pair of keys in each object
// with strings.EqualFold.
func foldedDuplicate(data []byte) (string, bool) {
	type frame struct {
		object  bool
		keyNext bool
		keys    []string
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var stack []*frame
	valueDone := func() {
		if n := len(stack); n > 0 && stack[n-1].object {
			stack[n-1].keyNext = true
		}
	}
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", false
		}
		if n := len(stack); n > 0 && stack[n-1].object && stack[n-1].keyNext {
			top := stack[n-1]
			if tok == json.Delim('}') {
				stack = stack[:n-1]
				valueDone()
				continue
			}
			key, _ := tok.(string)
			for _, prev := range top.keys {
				if strings.EqualFold(prev, key) {
					return key, true
				}
			}
			top.keys = append(top.keys, key)
			top.keyNext = false
			continue
		}
		switch tok {
		case json.Delim('{'):
			stack = append(stack, &frame{object: true, keyNext: true})
		case json.Delim('['):
			stack = append(stack, &frame{})
		case json.Delim(']'), json.Delim('}'):
			stack = stack[:len(stack)-1]
			valueDone()
		default:
			valueDone()
		}
	}
}

// FuzzDecodeNeverAcceptsFoldedDuplicates asserts Decode never accepts a
// document holding two keys of one object that strings.EqualFold — and so
// encoding/json's field matching — treats as the same, at any depth.
func FuzzDecodeNeverAcceptsFoldedDuplicates(f *testing.F) {
	for _, seed := range []string{
		`{}`, `[]`, `{"a":1}`, `{"a":1,"A":2}`,
		`{"upstreams":[{"path":"/run/a.sock"}],"UPSTREAMS":[]}`,
		`{"a":{"b":{"c":[{"d":1,"D":2}]}}}`,
		`[{"k":1,"K":2}]`, `{"s":{},"ſ":[]}`,
		`{"name":"x","site":{"aliases":{"A.test":"a","a.TEST":"b"}}}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var value any
		if Decode(data, &value) != nil {
			return
		}
		if key, dup := foldedDuplicate(data); dup {
			t.Fatalf("accepted %q with folded duplicate key %q", data, key)
		}
	})
}
