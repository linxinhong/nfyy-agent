package store

import "encoding/json"

// jsonMarshal keeps the audit writer readable without importing encoding/json
// into every file that touches the store.
func jsonMarshal(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}
