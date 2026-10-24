package agent

import (
	"reflect"
	"testing"
)

type weatherResponse struct {
	City        string  `json:"city"`
	Temperature float64 `json:"temperature"`
	Unit        string  `json:"unit"`
}

func TestParseStructuredOutput_Struct(t *testing.T) {
	ag := &Agent{cfg: AgentConfig{OutputSchema: weatherResponse{}}}

	content := `{"city":"Paris","temperature":25.5,"unit":"C"}`
	parsed, err := ag.parseStructuredOutput(content, nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	got, ok := parsed.(weatherResponse)
	if !ok {
		t.Fatalf("expected weatherResponse, got %T", parsed)
	}
	if got.City != "Paris" || got.Temperature != 25.5 || got.Unit != "C" {
		t.Fatalf("unexpected parsed value: %+v", got)
	}
}

func TestParseStructuredOutput_MapSchema(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"answer": map[string]any{"type": "string"},
		},
	}
	ag := &Agent{cfg: AgentConfig{OutputSchema: schema}}

	parsed, err := ag.parseStructuredOutput(`{"answer":"42"}`, nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	m, ok := parsed.(map[string]any)
	if !ok {
		t.Fatalf("expected map, got %T", parsed)
	}
	if m["answer"] != "42" {
		t.Fatalf("unexpected map: %+v", m)
	}
}

func TestParseStructuredOutput_MarkdownFences(t *testing.T) {
	ag := &Agent{cfg: AgentConfig{OutputSchema: weatherResponse{}}}

	content := "```json\n{\"city\":\"London\",\"temperature\":18,\"unit\":\"C\"}\n```"
	parsed, err := ag.parseStructuredOutput(content, nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := parsed.(weatherResponse)
	if got.City != "London" {
		t.Fatalf("unexpected city: %s", got.City)
	}
}

func TestResponseFormatForSchema_Struct(t *testing.T) {
	rf, err := responseFormatForSchema(weatherResponse{})
	if err != nil {
		t.Fatalf("responseFormatForSchema: %v", err)
	}
	if rf == nil {
		t.Fatal("expected non-nil response format")
	}
	if rf.Type != "json_schema" {
		t.Fatalf("expected json_schema, got %s", rf.Type)
	}
	js := rf.JSONSchema
	schema, ok := js["schema"].(map[string]any)
	if !ok {
		t.Fatalf("expected schema map, got %T", js["schema"])
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("expected properties, got %T", schema["properties"])
	}
	if _, ok := props["city"]; !ok {
		t.Fatal("missing city property")
	}
}

func TestResponseFormatForSchema_Map(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"x": map[string]any{"type": "integer"},
		},
	}
	rf, err := responseFormatForSchema(schema)
	if err != nil {
		t.Fatalf("responseFormatForSchema: %v", err)
	}
	if !reflect.DeepEqual(rf.JSONSchema["schema"], schema) {
		t.Fatalf("schema mismatch: %+v", rf.JSONSchema["schema"])
	}
}

// 回归：AgentSpec.OutputSchema（map[string]any）未配置时是 nil map，经 any
// 传入 AgentConfig 后成为 typed nil，`== nil` 判定为假。此前会误入结构化
// 解析分支，对 nil schema 做 Validate 直接 panic，无 schema Agent 完全不可用。
func TestParseStructuredOutput_TypedNilSchemaMeansNoSchema(t *testing.T) {
	var nilMap map[string]any
	ag := &Agent{cfg: AgentConfig{OutputSchema: any(nilMap)}}
	parsed, err := ag.parseStructuredOutput(`不是 JSON 的自由文本`, nil)
	if err != nil {
		t.Fatalf("typed-nil schema must be treated as no schema: %v", err)
	}
	if parsed != nil {
		t.Fatalf("parsed = %v, want nil", parsed)
	}
	if schema, err := compileOutputSchema(any(nilMap)); err != nil || schema != nil {
		t.Fatalf("compileOutputSchema(typed-nil) = %v, %v; want nil, nil", schema, err)
	}
}
