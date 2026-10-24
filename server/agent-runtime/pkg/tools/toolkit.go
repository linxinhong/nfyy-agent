// Package tools provides primitives for defining, registering, and invoking tools.
package tools

import (
	"fmt"
	"reflect"
	"runtime"
	"sort"
	"strings"
)

// Toolkit is a collection of tools that can be registered and queried by name.
type Toolkit struct {
	name  string
	tools map[string]Tool
}

// NewToolkit creates an empty toolkit with the given name.
func NewToolkit(name string) *Toolkit {
	if name == "" {
		name = "toolkit"
	}
	return &Toolkit{
		name:  name,
		tools: map[string]Tool{},
	}
}

// Name returns the toolkit name.
func (t *Toolkit) Name() string {
	return t.name
}

// Add registers a tool in the toolkit.
func (t *Toolkit) Add(tool Tool) {
	t.tools[tool.Name()] = tool
}

// AddFuncs creates Function tools from Go functions and adds them to the
// toolkit. Each function name is derived from its runtime symbol name; the
// description is left empty.
func (t *Toolkit) AddFuncs(fns ...any) error {
	for _, fn := range fns {
		name, err := funcName(fn)
		if err != nil {
			return err
		}
		tool, err := NewFunction(name, "", fn)
		if err != nil {
			return fmt.Errorf("register %s: %w", name, err)
		}
		t.Add(tool)
	}
	return nil
}

// Get retrieves a tool by name.
func (t *Toolkit) Get(name string) (Tool, bool) {
	tool, ok := t.tools[name]
	return tool, ok
}

// List returns all registered tools sorted by name.
func (t *Toolkit) List() []Tool {
	names := make([]string, 0, len(t.tools))
	for name := range t.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	tools := make([]Tool, 0, len(names))
	for _, name := range names {
		tools = append(tools, t.tools[name])
	}
	return tools
}

// Schemas returns the tool schemas for all registered tools, sorted by name.
func (t *Toolkit) Schemas() []map[string]any {
	tools := t.List()
	schemas := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		schemas = append(schemas, tool.Schema())
	}
	return schemas
}

func funcName(fn any) (string, error) {
	v := reflect.ValueOf(fn)
	if v.Kind() != reflect.Func {
		return "", fmt.Errorf("expected function, got %T", fn)
	}
	if v.IsNil() {
		return "", fmt.Errorf("function is nil")
	}
	rfn := runtime.FuncForPC(v.Pointer())
	if rfn == nil {
		return "", fmt.Errorf("cannot determine function name")
	}
	full := rfn.Name()
	// Strip package path and any closure suffixes.
	if i := strings.LastIndex(full, "."); i >= 0 {
		full = full[i+1:]
	}
	if i := strings.LastIndex(full, "-"); i >= 0 {
		full = full[:i]
	}
	return full, nil
}
