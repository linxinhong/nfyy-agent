package tools

import (
	"context"
	"testing"
)

func doubleFn(n int) int {
	return n * 2
}

func upperFn(s string) string {
	return s
}

func TestToolkit_AddAndGet(t *testing.T) {
	tk := NewToolkit("test")
	f, err := NewFunction("double", "Double a number.", doubleFn)
	if err != nil {
		t.Fatalf("NewFunction failed: %v", err)
	}
	tk.Add(f)

	tool, ok := tk.Get("double")
	if !ok {
		t.Fatal("expected to find double tool")
	}
	if tool.Name() != "double" {
		t.Fatalf("expected double, got %s", tool.Name())
	}
}

func TestToolkit_AddFuncs(t *testing.T) {
	tk := NewToolkit("test")
	if err := tk.AddFuncs(doubleFn, upperFn); err != nil {
		t.Fatalf("AddFuncs failed: %v", err)
	}

	if _, ok := tk.Get("doubleFn"); !ok {
		t.Fatal("expected doubleFn tool")
	}
	if _, ok := tk.Get("upperFn"); !ok {
		t.Fatal("expected upperFn tool")
	}
}

func TestToolkit_List(t *testing.T) {
	tk := NewToolkit("test")
	tk.AddFuncs(doubleFn, upperFn)

	tools := tk.List()
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}
	if tools[0].Name() != "doubleFn" || tools[1].Name() != "upperFn" {
		t.Fatalf("unexpected order: %v", tools)
	}
}

func TestToolkit_Schemas(t *testing.T) {
	tk := NewToolkit("test")
	tk.AddFuncs(doubleFn, upperFn)

	schemas := tk.Schemas()
	if len(schemas) != 2 {
		t.Fatalf("expected 2 schemas, got %d", len(schemas))
	}

	for _, s := range schemas {
		if s["type"] != "function" {
			t.Fatalf("expected function type, got %v", s["type"])
		}
		fn := s["function"].(map[string]any)
		if fn["name"] == "" {
			t.Fatal("expected function name")
		}
		params := fn["parameters"].(map[string]any)
		if params["type"] != "object" {
			t.Fatalf("expected object parameters, got %v", params["type"])
		}
	}
}

func TestToolkit_CallRoundTrip(t *testing.T) {
	tk := NewToolkit("test")
	tk.AddFuncs(doubleFn)

	tool, ok := tk.Get("doubleFn")
	if !ok {
		t.Fatal("expected doubleFn tool")
	}

	res, err := tool.Call(context.Background(), map[string]any{"n": 21})
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}
	if res != 42 {
		t.Fatalf("expected 42, got %v", res)
	}
}
