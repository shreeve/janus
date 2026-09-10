package strictjson

import (
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
