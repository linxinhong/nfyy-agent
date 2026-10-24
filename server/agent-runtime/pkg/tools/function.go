// Package tools provides primitives for defining, registering, and invoking tools.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"runtime"
	"strings"
)

// FuncOpt customizes a Function built by NewFunction.
type FuncOpt func(*Function)

// WithStrict enables strict JSON-schema mode for the function parameters.
func WithStrict(strict bool) FuncOpt {
	return func(f *Function) {
		f.strict = strict
	}
}

// Function is a Tool backed by a Go function.
type Function struct {
	name        string
	description string
	fn          any
	fnValue     reflect.Value
	fnType      reflect.Type
	hasContext  bool
	params      []paramInfo
	inline      bool // single struct/map arg decoded directly from the args object
	strict      bool
	parameters  map[string]any
}

type paramInfo struct {
	name string
	typ  reflect.Type
	idx  int
}

// NewFunction builds a Tool from a Go function using reflection.
//
// The function may optionally accept context.Context as its first argument.
// Supported return signatures are: (any, error), error, any, or none.
// Parameter schemas are generated from the function signature; struct fields
// use their json tags as property names. A field is required when its Go type
// is non-pointer.
func NewFunction(name, description string, fn any, opts ...FuncOpt) (*Function, error) {
	if name == "" {
		return nil, errors.New("function name is required")
	}
	v := reflect.ValueOf(fn)
	if v.Kind() != reflect.Func {
		return nil, errors.New("fn must be a function")
	}
	if v.IsNil() {
		return nil, errors.New("fn must not be nil")
	}
	ft := v.Type()

	f := &Function{
		name:        name,
		description: description,
		fn:          fn,
		fnValue:     v,
		fnType:      ft,
		parameters:  map[string]any{"type": "object", "properties": map[string]any{}, "required": []string{}},
	}
	for _, opt := range opts {
		opt(f)
	}

	// Detect optional context.Context first argument.
	paramStart := 0
	if ft.NumIn() > 0 && ft.In(0) == reflect.TypeOf((*context.Context)(nil)).Elem() {
		f.hasContext = true
		paramStart = 1
	}

	nonCtxCount := ft.NumIn() - paramStart

	// Try to discover parameter names from source so we can map args by name.
	names, astErr := getParamNames(fn)
	if astErr != nil && nonCtxCount > 1 {
		return nil, fmt.Errorf("could not determine parameter names for %q: %w", name, astErr)
	}
	if names != nil && len(names) != ft.NumIn() {
		// AST names include context if present; they should match total ins.
		names = nil
	}

	for i := paramStart; i < ft.NumIn(); i++ {
		pname := ""
		if names != nil {
			pname = names[i]
		}
		if pname == "" {
			// Fallback for a single non-context argument.
			if nonCtxCount == 1 {
				pname = "value"
			} else {
				return nil, fmt.Errorf("could not determine name for parameter %d of %q", i, name)
			}
		}
		f.params = append(f.params, paramInfo{name: pname, typ: ft.In(i), idx: i})
	}

	// A single struct or map argument is decoded directly from the flat args
	// object, matching the inlined JSON schema.
	if len(f.params) == 1 {
		k := f.params[0].typ.Kind()
		if k == reflect.Struct || k == reflect.Map {
			f.inline = true
		}
	}

	f.parameters = f.buildParameters()
	return f, nil
}

// Name returns the function name.
func (f *Function) Name() string {
	return f.name
}

// Description returns the function description.
func (f *Function) Description() string {
	return f.description
}

// Schema returns the tool definition for the model.
func (f *Function) Schema() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        f.name,
			"description": f.description,
			"parameters":  f.parameters,
		},
	}
}

// Parameters returns the JSON schema object describing the function parameters.
func (f *Function) Parameters() map[string]any {
	return f.parameters
}

// Call decodes args into the function parameters, invokes the function and
// returns the result or error.
func (f *Function) Call(ctx context.Context, args map[string]any) (any, error) {
	if args == nil {
		args = map[string]any{}
	}

	in := make([]reflect.Value, 0, f.fnType.NumIn())
	if f.hasContext {
		in = append(in, reflect.ValueOf(ctx))
	}

	if f.inline && len(f.params) == 1 {
		dv, err := decodeValue(args, f.params[0].typ)
		if err != nil {
			return nil, fmt.Errorf("argument %q: %w", f.params[0].name, err)
		}
		in = append(in, dv)
	} else {
		for _, p := range f.params {
			v, ok := args[p.name]
			if !ok {
				if p.typ.Kind() != reflect.Ptr {
					return nil, fmt.Errorf("missing required argument %q", p.name)
				}
				// Optional pointer argument defaults to nil.
				in = append(in, reflect.Zero(p.typ))
				continue
			}
			dv, err := decodeValue(v, p.typ)
			if err != nil {
				return nil, fmt.Errorf("argument %q: %w", p.name, err)
			}
			in = append(in, dv)
		}
	}

	out := f.fnValue.Call(in)
	return f.processResults(out)
}

func (f *Function) buildParameters() map[string]any {
	props := map[string]any{}
	var required []string

	// If there is exactly one non-context parameter and it is a struct, inline
	// its properties directly into the parameters object. This avoids an
	// artificial wrapper key and matches common Go handler conventions.
	if len(f.params) == 1 {
		p := f.params[0]
		if p.typ.Kind() == reflect.Struct {
			return f.schemaForStruct(p.typ, f.strict)
		}
		if p.typ.Kind() == reflect.Map {
			return map[string]any{
				"type":                 "object",
				"additionalProperties": schemaForType(p.typ.Elem()),
			}
		}
	}

	for _, p := range f.params {
		props[p.name] = schemaForType(p.typ)
		if p.typ.Kind() != reflect.Ptr {
			required = append(required, p.name)
		}
	}

	schema := map[string]any{
		"type":       "object",
		"properties": props,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	if f.strict {
		schema["additionalProperties"] = false
	}
	return schema
}

func (f *Function) schemaForStruct(t reflect.Type, strict bool) map[string]any {
	props := map[string]any{}
	var required []string
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "" {
			name = field.Name
		}
		if name == "-" {
			continue
		}
		props[name] = schemaForType(field.Type)
		if field.Type.Kind() != reflect.Ptr {
			required = append(required, name)
		}
	}
	schema := map[string]any{
		"type":       "object",
		"properties": props,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	if strict {
		schema["additionalProperties"] = false
	}
	return schema
}

func (f *Function) processResults(out []reflect.Value) (any, error) {
	if len(out) == 0 {
		return nil, nil
	}
	if len(out) == 1 {
		if f.fnType.Out(0) == reflect.TypeOf((*error)(nil)).Elem() {
			if err := out[0]; !err.IsNil() {
				return nil, err.Interface().(error)
			}
			return nil, nil
		}
		return out[0].Interface(), nil
	}
	// Two or more results: the final error is special; everything before it is
	// returned as a single value (or as a slice when there are multiple).
	last := out[len(out)-1]
	if f.fnType.Out(len(out)-1) == reflect.TypeOf((*error)(nil)).Elem() {
		if !last.IsNil() {
			return nil, last.Interface().(error)
		}
		out = out[:len(out)-1]
	}
	switch len(out) {
	case 0:
		return nil, nil
	case 1:
		return out[0].Interface(), nil
	default:
		res := make([]any, len(out))
		for i, v := range out {
			res[i] = v.Interface()
		}
		return res, nil
	}
}

// schemaForType builds a JSON schema fragment for a Go type.
func schemaForType(t reflect.Type) map[string]any {
	switch t.Kind() {
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Ptr:
		return schemaForType(t.Elem())
	case reflect.Slice, reflect.Array:
		if t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8 {
			return map[string]any{"type": "string", "format": "binary"}
		}
		return map[string]any{"type": "array", "items": schemaForType(t.Elem())}
	case reflect.Map:
		return map[string]any{
			"type":                 "object",
			"additionalProperties": schemaForType(t.Elem()),
		}
	case reflect.Struct:
		props := map[string]any{}
		var required []string
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			if !field.IsExported() {
				continue
			}
			name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
			if name == "" {
				name = field.Name
			}
			if name == "-" {
				continue
			}
			props[name] = schemaForType(field.Type)
			if field.Type.Kind() != reflect.Ptr {
				required = append(required, name)
			}
		}
		s := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			s["required"] = required
		}
		return s
	default:
		return map[string]any{"type": "object"}
	}
}

// decodeValue converts a JSON-like value into a reflect.Value of target type t.
func decodeValue(v any, t reflect.Type) (reflect.Value, error) {
	if v == nil {
		return reflect.Zero(t), nil
	}

	switch t.Kind() {
	case reflect.Bool:
		b, ok := v.(bool)
		if !ok {
			return reflect.Value{}, fmt.Errorf("expected bool, got %T", v)
		}
		return reflect.ValueOf(b).Convert(t), nil
	case reflect.String:
		s, ok := v.(string)
		if !ok {
			return reflect.Value{}, fmt.Errorf("expected string, got %T", v)
		}
		return reflect.ValueOf(s).Convert(t), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := toInt64(v)
		if err != nil {
			return reflect.Value{}, err
		}
		return reflect.ValueOf(n).Convert(t), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := toUint64(v)
		if err != nil {
			return reflect.Value{}, err
		}
		return reflect.ValueOf(n).Convert(t), nil
	case reflect.Float32, reflect.Float64:
		n, err := toFloat64(v)
		if err != nil {
			return reflect.Value{}, err
		}
		return reflect.ValueOf(n).Convert(t), nil
	case reflect.Struct:
		b, err := json.Marshal(v)
		if err != nil {
			return reflect.Value{}, err
		}
		ptr := reflect.New(t)
		if err := json.Unmarshal(b, ptr.Interface()); err != nil {
			return reflect.Value{}, err
		}
		return ptr.Elem(), nil
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			s, ok := v.(string)
			if !ok {
				return reflect.Value{}, fmt.Errorf("expected base64 string for []byte, got %T", v)
			}
			return reflect.ValueOf([]byte(s)), nil
		}
		arr, ok := v.([]any)
		if !ok {
			return reflect.Value{}, fmt.Errorf("expected array, got %T", v)
		}
		sl := reflect.MakeSlice(t, len(arr), len(arr))
		for i, item := range arr {
			elem, err := decodeValue(item, t.Elem())
			if err != nil {
				return reflect.Value{}, err
			}
			sl.Index(i).Set(elem)
		}
		return sl, nil
	case reflect.Array:
		arr, ok := v.([]any)
		if !ok {
			return reflect.Value{}, fmt.Errorf("expected array, got %T", v)
		}
		if len(arr) != t.Len() {
			return reflect.Value{}, fmt.Errorf("expected array of length %d, got %d", t.Len(), len(arr))
		}
		a := reflect.New(t).Elem()
		for i, item := range arr {
			elem, err := decodeValue(item, t.Elem())
			if err != nil {
				return reflect.Value{}, err
			}
			a.Index(i).Set(elem)
		}
		return a, nil
	case reflect.Map:
		m, ok := v.(map[string]any)
		if !ok {
			return reflect.Value{}, fmt.Errorf("expected object, got %T", v)
		}
		mv := reflect.MakeMap(t)
		kt := t.Key()
		vt := t.Elem()
		for k, val := range m {
			keyVal := reflect.ValueOf(k)
			if kt.Kind() != reflect.String {
				var err error
				keyVal, err = decodeValue(k, kt)
				if err != nil {
					return reflect.Value{}, fmt.Errorf("map key %q: %w", k, err)
				}
			}
			elem, err := decodeValue(val, vt)
			if err != nil {
				return reflect.Value{}, err
			}
			mv.SetMapIndex(keyVal.Convert(kt), elem)
		}
		return mv, nil
	case reflect.Ptr:
		ptr := reflect.New(t.Elem())
		elem, err := decodeValue(v, t.Elem())
		if err != nil {
			return reflect.Value{}, err
		}
		ptr.Elem().Set(elem)
		return ptr, nil
	default:
		return reflect.Value{}, fmt.Errorf("unsupported parameter kind %s", t.Kind())
	}
}

func toInt64(v any) (int64, error) {
	switch n := v.(type) {
	case int:
		return int64(n), nil
	case int8:
		return int64(n), nil
	case int16:
		return int64(n), nil
	case int32:
		return int64(n), nil
	case int64:
		return n, nil
	case uint:
		return int64(n), nil
	case uint8:
		return int64(n), nil
	case uint16:
		return int64(n), nil
	case uint32:
		return int64(n), nil
	case uint64:
		return int64(n), nil
	case float32:
		return int64(n), nil
	case float64:
		return int64(n), nil
	case json.Number:
		return n.Int64()
	default:
		return 0, fmt.Errorf("expected number, got %T", v)
	}
}

func toUint64(v any) (uint64, error) {
	switch n := v.(type) {
	case int:
		return uint64(n), nil
	case int8:
		return uint64(n), nil
	case int16:
		return uint64(n), nil
	case int32:
		return uint64(n), nil
	case int64:
		return uint64(n), nil
	case uint:
		return uint64(n), nil
	case uint8:
		return uint64(n), nil
	case uint16:
		return uint64(n), nil
	case uint32:
		return uint64(n), nil
	case uint64:
		return n, nil
	case float32:
		return uint64(n), nil
	case float64:
		return uint64(n), nil
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return 0, err
		}
		return uint64(f), nil
	default:
		return 0, fmt.Errorf("expected number, got %T", v)
	}
}

func toFloat64(v any) (float64, error) {
	switch n := v.(type) {
	case int:
		return float64(n), nil
	case int8:
		return float64(n), nil
	case int16:
		return float64(n), nil
	case int32:
		return float64(n), nil
	case int64:
		return float64(n), nil
	case uint:
		return float64(n), nil
	case uint8:
		return float64(n), nil
	case uint16:
		return float64(n), nil
	case uint32:
		return float64(n), nil
	case uint64:
		return float64(n), nil
	case float32:
		return float64(n), nil
	case float64:
		return n, nil
	case json.Number:
		return n.Float64()
	default:
		return 0, fmt.Errorf("expected number, got %T", v)
	}
}

// getParamNames attempts to read the source parameter names for a function
// value using go/ast. It returns the names in declaration order, or an error
// if the source cannot be parsed or the declaration cannot be located.
func getParamNames(fn any) ([]string, error) {
	v := reflect.ValueOf(fn)
	rfn := runtime.FuncForPC(v.Pointer())
	if rfn == nil {
		return nil, errors.New("cannot get runtime function info")
	}
	file, line := rfn.FileLine(rfn.Entry())
	if file == "" || line == 0 {
		return nil, errors.New("cannot determine function source location")
	}

	fset := token.NewFileSet()
	af, err := parser.ParseFile(fset, file, nil, parser.ParseComments)
	if err != nil {
		return nil, err
	}

	var names []string
	ast.Inspect(af, func(n ast.Node) bool {
		switch fd := n.(type) {
		case *ast.FuncDecl:
			if line >= fset.Position(fd.Pos()).Line && line <= fset.Position(fd.End()).Line {
				names = extractFieldNames(fd.Type.Params)
				return false
			}
		case *ast.FuncLit:
			if line >= fset.Position(fd.Pos()).Line && line <= fset.Position(fd.End()).Line {
				names = extractFieldNames(fd.Type.Params)
				return false
			}
		}
		return true
	})

	if names == nil {
		return nil, fmt.Errorf("function declaration not found at %s:%d", file, line)
	}
	return names, nil
}

func extractFieldNames(fl *ast.FieldList) []string {
	if fl == nil {
		return nil
	}
	var names []string
	for _, field := range fl.List {
		if len(field.Names) == 0 {
			// Anonymous parameter; use an empty placeholder.
			names = append(names, "")
			continue
		}
		for _, name := range field.Names {
			names = append(names, name.Name)
		}
	}
	return names
}
