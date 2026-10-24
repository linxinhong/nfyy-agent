// Package agent implements the core Agent runtime, including configuration,
// execution, streaming, and guardrails.
package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// parseStructuredOutput parses the model output according to the configured
// OutputSchema. It returns nil when no schema is configured.
func (a *Agent) parseStructuredOutput(content string, modelParsed any) (any, error) {
	if isNilOutputSchema(a.cfg.OutputSchema) {
		return nil, nil
	}

	var raw []byte
	var err error
	if modelParsed != nil {
		raw, err = json.Marshal(modelParsed)
	} else {
		content = stripJSONMarkdownFences(content)
		if content == "" {
			return nil, errors.New("structured output requested but model returned empty content")
		}
		raw = []byte(content)
	}
	if err != nil {
		return nil, fmt.Errorf("serialize structured output: %w", err)
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("parse structured output: %w", err)
	}
	if a.outputSchema == nil {
		compiled, compileErr := compileOutputSchema(a.cfg.OutputSchema)
		if compileErr != nil {
			return nil, fmt.Errorf("compile structured output schema: %w", compileErr)
		}
		a.outputSchema = compiled
	}
	if err := a.outputSchema.Validate(value); err != nil {
		return nil, fmt.Errorf("validate structured output: %w", err)
	}

	target, err := newSchemaTarget(a.cfg.OutputSchema)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return nil, fmt.Errorf("parse structured output: %w", err)
	}

	return reflect.ValueOf(target).Elem().Interface(), nil
}

// isNilOutputSchema 报告 schema 是否为"无 schema"：除 untyped nil 外，
// 还包含经 any 传入的 nil map / nil ptr（typed nil，== nil 判定为假）。
func isNilOutputSchema(schema any) bool {
	if schema == nil {
		return true
	}
	value := reflect.ValueOf(schema)
	switch value.Kind() {
	case reflect.Map, reflect.Slice, reflect.Pointer, reflect.Interface, reflect.Func:
		return value.IsNil()
	default:
		return false
	}
}

func compileOutputSchema(schema any) (*jsonschema.Schema, error) {
	if isNilOutputSchema(schema) {
		return nil, nil
	}
	document, err := schemaDocument(schema)
	if err != nil {
		return nil, err
	}
	if document == nil {
		return nil, nil
	}
	if err := rejectExternalSchemaReferences(document); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(document)
	if err != nil {
		return nil, err
	}
	normalized, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	const location = "urn:xingding:agent-output-schema"
	if err := compiler.AddResource(location, normalized); err != nil {
		return nil, err
	}
	return compiler.Compile(location)
}

// ValidateOutputSchema verifies one supported Agent output schema without constructing an Agent.
func ValidateOutputSchema(schema any) error {
	_, err := compileOutputSchema(schema)
	return err
}

func schemaDocument(schema any) (map[string]any, error) {
	switch value := schema.(type) {
	case map[string]any:
		if value == nil {
			return nil, nil
		}
		return value, nil
	case reflect.Type:
		if value.Kind() == reflect.Ptr {
			value = value.Elem()
		}
		return schemaForType(value), nil
	default:
		t := reflect.TypeOf(schema)
		if t == nil {
			return nil, nil
		}
		if t.Kind() == reflect.Ptr {
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct && t.Kind() != reflect.Map {
			return nil, fmt.Errorf("unsupported output schema type %T", schema)
		}
		return schemaForType(t), nil
	}
}

func rejectExternalSchemaReferences(value any) error {
	switch current := value.(type) {
	case map[string]any:
		for key, child := range current {
			if key == "$ref" {
				ref, ok := child.(string)
				if !ok || !strings.HasPrefix(ref, "#") {
					return fmt.Errorf("external schema reference %q is not allowed", ref)
				}
			}
			if err := rejectExternalSchemaReferences(child); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range current {
			if err := rejectExternalSchemaReferences(child); err != nil {
				return err
			}
		}
	}
	return nil
}

// responseFormatForModel builds a models.ResponseFormat from the configured
// OutputSchema, falling back to json_object when the model does not advertise
// native json_schema support.
func (a *Agent) responseFormatForModel(schema any) (*models.ResponseFormat, error) {
	rf, err := responseFormatForSchema(schema)
	if err != nil {
		return nil, err
	}
	if rf == nil || rf.Type != models.ResponseFormatJSONSchema {
		return rf, nil
	}
	if cap, ok := a.cfg.Model.(models.JSONSchemaCapable); ok && cap.SupportsNativeJSONSchema() {
		for _, fallback := range a.cfg.FallbackModels {
			fallbackCap, supported := fallback.(models.JSONSchemaCapable)
			if !supported || !fallbackCap.SupportsNativeJSONSchema() {
				return &models.ResponseFormat{Type: models.ResponseFormatJSON}, nil
			}
		}
		return rf, nil
	}
	return &models.ResponseFormat{Type: models.ResponseFormatJSON}, nil
}

// responseFormatForSchema builds a models.ResponseFormat from the configured
// OutputSchema so the model can be asked for structured output natively.
func responseFormatForSchema(schema any) (*models.ResponseFormat, error) {
	if schema == nil {
		return nil, nil
	}

	switch s := schema.(type) {
	case map[string]any:
		return jsonSchemaResponseFormat(s), nil
	case reflect.Type:
		if s.Kind() == reflect.Ptr {
			s = s.Elem()
		}
		if s.Kind() != reflect.Struct && s.Kind() != reflect.Map {
			return nil, fmt.Errorf("unsupported output schema type %s", s.Kind())
		}
		return jsonSchemaResponseFormat(schemaForType(s)), nil
	default:
		// If the user passed a struct value, derive the type from it.
		t := reflect.TypeOf(s)
		if t.Kind() == reflect.Ptr {
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct && t.Kind() != reflect.Map {
			return nil, fmt.Errorf("unsupported output schema type %T", s)
		}
		return jsonSchemaResponseFormat(schemaForType(t)), nil
	}
}

func jsonSchemaResponseFormat(schema map[string]any) *models.ResponseFormat {
	return &models.ResponseFormat{
		Type: models.ResponseFormatJSONSchema,
		JSONSchema: map[string]any{
			"name":   "response",
			"schema": schema,
		},
	}
}

// newSchemaTarget returns a pointer to a new value suitable for unmarshalling
// output that conforms to the configured schema.
func newSchemaTarget(schema any) (any, error) {
	switch s := schema.(type) {
	case map[string]any:
		return &map[string]any{}, nil
	case reflect.Type:
		if s.Kind() == reflect.Ptr {
			s = s.Elem()
		}
		if s.Kind() != reflect.Struct && s.Kind() != reflect.Map {
			return nil, fmt.Errorf("unsupported output schema type %s", s.Kind())
		}
		return reflect.New(s).Interface(), nil
	default:
		t := reflect.TypeOf(s)
		if t.Kind() == reflect.Ptr {
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct && t.Kind() != reflect.Map {
			return nil, fmt.Errorf("unsupported output schema type %T", s)
		}
		return reflect.New(t).Interface(), nil
	}
}

// stripJSONMarkdownFences removes ```json ... ``` wrappers from model output.
func stripJSONMarkdownFences(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```json") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimSpace(s)
		if strings.HasSuffix(s, "```") {
			s = strings.TrimSuffix(s, "```")
		}
		return strings.TrimSpace(s)
	}
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSpace(s)
		if strings.HasSuffix(s, "```") {
			s = strings.TrimSuffix(s, "```")
		}
		return strings.TrimSpace(s)
	}
	return s
}

// schemaForType builds a simple JSON schema fragment for a Go type.
// It is a minimal agent-local implementation; for complex schemas consider
// using a dedicated JSON-schema library.
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
			name := field.Name
			if tag, _, _ := strings.Cut(field.Tag.Get("json"), ","); tag != "" {
				if tag == "-" {
					continue
				}
				name = tag
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
