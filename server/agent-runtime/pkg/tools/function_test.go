package tools

import (
	"context"
	"errors"
	"testing"
)

type addInput struct {
	A float64 `json:"a"`
	B float64 `json:"b"`
}

type personInput struct {
	Name  string         `json:"name"`
	Age   int            `json:"age"`
	Email *string        `json:"email,omitempty"`
	Tags  []string       `json:"tags,omitempty"`
	Meta  map[string]int `json:"meta,omitempty"`
}

func addFn(ctx context.Context, input addInput) (float64, error) {
	return input.A + input.B, nil
}

func echoFn(msg string) string {
	return msg
}

func multiplyFn(a, b int) int {
	return a * b
}

func describePerson(_ context.Context, p personInput) (string, error) {
	return p.Name, nil
}

func optionalFn(required string, optional *int) (string, error) {
	if optional != nil {
		return required + ":" + string(rune(*optional)), nil
	}
	return required, nil
}

func errFn() error {
	return errors.New("boom")
}

func sumTagsFn(_ context.Context, tags []string, weights map[string]int) (int, error) {
	total := 0
	for _, t := range tags {
		total += weights[t]
	}
	return total, nil
}

func TestNewFunction_RequiresName(t *testing.T) {
	_, err := NewFunction("", "", echoFn)
	if err == nil {
		t.Fatal("expected error for empty name")
	}
}

func TestNewFunction_SchemaStruct(t *testing.T) {
	f, err := NewFunction("add", "Add two numbers.", addFn)
	if err != nil {
		t.Fatalf("NewFunction failed: %v", err)
	}

	schema := f.Schema()
	fn := schema["function"].(map[string]any)
	if fn["name"] != "add" {
		t.Fatalf("expected name add, got %v", fn["name"])
	}

	params := fn["parameters"].(map[string]any)
	if params["type"] != "object" {
		t.Fatalf("expected object, got %v", params["type"])
	}

	props := params["properties"].(map[string]any)
	if _, ok := props["a"]; !ok {
		t.Fatalf("expected property a, got %v", props)
	}
	if _, ok := props["b"]; !ok {
		t.Fatalf("expected property b, got %v", props)
	}

	required := params["required"].([]string)
	if len(required) != 2 {
		t.Fatalf("expected 2 required fields, got %v", required)
	}
}

func TestNewFunction_SchemaMultiplePrimitive(t *testing.T) {
	f, err := NewFunction("multiply", "Multiply two integers.", multiplyFn)
	if err != nil {
		t.Fatalf("NewFunction failed: %v", err)
	}

	params := f.Parameters()
	props := params["properties"].(map[string]any)
	if len(props) != 2 {
		t.Fatalf("expected 2 properties, got %v", props)
	}
	if props["a"].(map[string]any)["type"] != "integer" {
		t.Fatalf("expected integer for a, got %v", props["a"])
	}
	required := params["required"].([]string)
	if len(required) != 2 {
		t.Fatalf("expected 2 required params, got %v", required)
	}
}

func TestNewFunction_SchemaComplexStruct(t *testing.T) {
	f, err := NewFunction("describe_person", "Describe a person.", describePerson)
	if err != nil {
		t.Fatalf("NewFunction failed: %v", err)
	}

	params := f.Parameters()
	props := params["properties"].(map[string]any)

	nameSchema := props["name"].(map[string]any)
	if nameSchema["type"] != "string" {
		t.Fatalf("expected string for name, got %v", nameSchema)
	}

	ageSchema := props["age"].(map[string]any)
	if ageSchema["type"] != "integer" {
		t.Fatalf("expected integer for age, got %v", ageSchema)
	}

	emailSchema := props["email"].(map[string]any)
	if emailSchema["type"] != "string" {
		t.Fatalf("expected string for email pointer, got %v", emailSchema)
	}

	tagsSchema := props["tags"].(map[string]any)
	if tagsSchema["type"] != "array" {
		t.Fatalf("expected array for tags, got %v", tagsSchema)
	}

	metaSchema := props["meta"].(map[string]any)
	if metaSchema["type"] != "object" {
		t.Fatalf("expected object for meta, got %v", metaSchema)
	}

	required := params["required"].([]string)
	gotRequired := map[string]bool{}
	for _, r := range required {
		gotRequired[r] = true
	}
	if len(gotRequired) != 4 || !gotRequired["name"] || !gotRequired["age"] || !gotRequired["tags"] || !gotRequired["meta"] {
		t.Fatalf("expected required name/age/tags/meta, got %v", required)
	}
}

func TestNewFunction_SchemaOptionalPointer(t *testing.T) {
	f, err := NewFunction("optional", "Optional pointer argument.", optionalFn)
	if err != nil {
		t.Fatalf("NewFunction failed: %v", err)
	}

	params := f.Parameters()
	required := params["required"].([]string)
	if len(required) != 1 || required[0] != "required" {
		t.Fatalf("expected only 'required' required, got %v", required)
	}
}

func TestFunction_CallStruct(t *testing.T) {
	f, err := NewFunction("add", "", addFn)
	if err != nil {
		t.Fatalf("NewFunction failed: %v", err)
	}

	res, err := f.Call(context.Background(), map[string]any{"a": 2.0, "b": 3.0})
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}
	if res != 5.0 {
		t.Fatalf("expected 5, got %v", res)
	}
}

func TestFunction_CallMultiplePrimitive(t *testing.T) {
	f, err := NewFunction("multiply", "", multiplyFn)
	if err != nil {
		t.Fatalf("NewFunction failed: %v", err)
	}

	res, err := f.Call(context.Background(), map[string]any{"a": 6, "b": 7})
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}
	if res != 42 {
		t.Fatalf("expected 42, got %v", res)
	}
}

func TestFunction_CallSinglePrimitive(t *testing.T) {
	f, err := NewFunction("echo", "", echoFn)
	if err != nil {
		t.Fatalf("NewFunction failed: %v", err)
	}

	res, err := f.Call(context.Background(), map[string]any{"msg": "hello"})
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}
	if res != "hello" {
		t.Fatalf("expected hello, got %v", res)
	}
}

func TestFunction_CallOptionalPointer(t *testing.T) {
	f, err := NewFunction("optional", "", optionalFn)
	if err != nil {
		t.Fatalf("NewFunction failed: %v", err)
	}

	res, err := f.Call(context.Background(), map[string]any{"required": "x"})
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}
	if res != "x" {
		t.Fatalf("expected x, got %v", res)
	}

	res, err = f.Call(context.Background(), map[string]any{"required": "x", "optional": 65})
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}
	if res != "x:A" {
		t.Fatalf("expected x:A, got %v", res)
	}
}

func TestFunction_CallErrorReturn(t *testing.T) {
	f, err := NewFunction("err", "", errFn)
	if err != nil {
		t.Fatalf("NewFunction failed: %v", err)
	}

	res, err := f.Call(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if res != nil {
		t.Fatalf("expected nil result, got %v", res)
	}
}

func TestFunction_CallMissingRequired(t *testing.T) {
	f, err := NewFunction("multiply", "", multiplyFn)
	if err != nil {
		t.Fatalf("NewFunction failed: %v", err)
	}

	_, err = f.Call(context.Background(), map[string]any{"a": 1})
	if err == nil {
		t.Fatal("expected error for missing argument")
	}
}

func TestNewFunction_SchemaSliceAndMap(t *testing.T) {
	f, err := NewFunction("sum_tags", "Sum weights for selected tags.", sumTagsFn)
	if err != nil {
		t.Fatalf("NewFunction failed: %v", err)
	}

	params := f.Parameters()
	props := params["properties"].(map[string]any)

	tagsSchema := props["tags"].(map[string]any)
	if tagsSchema["type"] != "array" {
		t.Fatalf("expected array for tags, got %v", tagsSchema)
	}
	itemsSchema := tagsSchema["items"].(map[string]any)
	if itemsSchema["type"] != "string" {
		t.Fatalf("expected string items, got %v", itemsSchema)
	}

	weightsSchema := props["weights"].(map[string]any)
	if weightsSchema["type"] != "object" {
		t.Fatalf("expected object for weights, got %v", weightsSchema)
	}
	addProps := weightsSchema["additionalProperties"].(map[string]any)
	if addProps["type"] != "integer" {
		t.Fatalf("expected integer additionalProperties, got %v", addProps)
	}

	required := params["required"].([]string)
	if len(required) != 2 {
		t.Fatalf("expected 2 required params, got %v", required)
	}
}

func TestFunction_CallSliceAndMap(t *testing.T) {
	f, err := NewFunction("sum_tags", "", sumTagsFn)
	if err != nil {
		t.Fatalf("NewFunction failed: %v", err)
	}

	res, err := f.Call(context.Background(), map[string]any{
		"tags":    []any{"a", "b"},
		"weights": map[string]any{"a": 10, "b": 20},
	})
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}
	if res != 30 {
		t.Fatalf("expected 30, got %v", res)
	}
}
