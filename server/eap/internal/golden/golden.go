// Package golden binds contract fixtures to Go types: a test helper that
// asserts a value marshals to exactly the JSON fixture (order-insensitive).
// Protocol packages use it to freeze public wire shapes under
// contracts/schemas/<version>/.
package golden

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
)

// Check asserts that marshaling v equals the JSON at fixturePath after
// normalization (parsed and compared as generic values).
func Check(t interface {
	Helper()
	Fatalf(string, ...any)
}, fixturePath string, v any) {
	t.Helper()
	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	want, err := normalize(raw)
	if err != nil {
		t.Fatalf("fixture %s is not valid JSON: %v", fixturePath, err)
	}
	got, err := normalize(mustJSON(t, v))
	if err != nil {
		t.Fatalf("value for %s does not marshal to JSON: %v", fixturePath, err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("golden mismatch for %s\n  fixture: %s\n  produced: %s", fixturePath, compact(raw), compact(mustJSON(t, v)))
	}
}

// Unmarshal decodes the fixture into v, proving the fixture is representable
// by the contract type.
func Unmarshal(t interface {
	Helper()
	Fatalf(string, ...any)
}, fixturePath string, v any) {
	t.Helper()
	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("fixture %s does not fit contract type %T: %v", fixturePath, v, err)
	}
}

func mustJSON(t interface {
	Fatalf(string, ...any)
}, v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	return b
}

func normalize(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

func compact(b []byte) []byte {
	var out bytes.Buffer
	if err := json.Compact(&out, b); err != nil {
		return b
	}
	return out.Bytes()
}
