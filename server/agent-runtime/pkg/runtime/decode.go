package runtime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// DecodeSpec strictly decodes one JSON ProjectSpec document. Persisted JSON
// must declare its schema version explicitly.
func DecodeSpec(reader io.Reader) (ProjectSpec, error) {
	raw, err := io.ReadAll(reader)
	if err != nil {
		return ProjectSpec{}, diagnosticError(Diagnostic{Code: DiagnosticInvalidJSON, Message: fmt.Sprintf("read project spec: %v", err)}, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	var spec ProjectSpec
	if err := decoder.Decode(&spec); err != nil {
		return ProjectSpec{}, diagnosticError(Diagnostic{
			Code: DiagnosticInvalidJSON, Message: fmt.Sprintf("decode project spec: %v", err),
		}, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple JSON documents")
		}
		return ProjectSpec{}, diagnosticError(Diagnostic{
			Code: DiagnosticInvalidJSON, Message: fmt.Sprintf("decode project spec: %v", err),
		}, err)
	}
	if spec.SchemaVersion == 0 {
		return ProjectSpec{}, diagnosticError(Diagnostic{
			Code: DiagnosticRequired, Path: "schema_version", Message: "schema_version is required",
		}, nil)
	}
	if !isSupportedSchemaVersion(spec.SchemaVersion) {
		return ProjectSpec{}, diagnosticError(Diagnostic{
			Code: DiagnosticUnsupportedSchemaVersion, Path: "schema_version",
			Message: fmt.Sprintf("schema_version %d is not supported", spec.SchemaVersion),
		}, nil)
	}
	// v1: reject v2+ fields.
	if spec.SchemaVersion == LegacySchemaVersion {
		var document struct {
			Agents []map[string]json.RawMessage `json:"agents"`
		}
		if err := json.Unmarshal(raw, &document); err == nil {
			for i, item := range document.Agents {
				for _, field := range []string{"output_schema", "tool_timeout_ms", "run_timeout_ms", "max_concurrent_runs", "hooks_ref"} {
					if _, exists := item[field]; exists {
						return ProjectSpec{}, diagnosticError(Diagnostic{
							Code: DiagnosticInvalidValue, Path: fmt.Sprintf("agents[%d].%s", i, field),
							Message: field + " requires schema_version 2",
						}, nil)
					}
				}
			}
		}
	}
	// v1/v2: reject v3 fields.
	if spec.SchemaVersion < SchemaVersion3 {
		var document struct {
			Agents []map[string]json.RawMessage `json:"agents"`
		}
		if err := json.Unmarshal(raw, &document); err == nil {
			for i, item := range document.Agents {
				for _, field := range []string{"context_window", "token_counter_ref", "summarizer_ref"} {
					if _, exists := item[field]; exists {
						return ProjectSpec{}, diagnosticError(Diagnostic{
							Code: DiagnosticInvalidValue, Path: fmt.Sprintf("agents[%d].%s", i, field),
							Message: field + " requires schema_version 3",
						}, nil)
					}
				}
			}
		}
	}
	// v1-v3: reject v4 fields.
	if spec.SchemaVersion < SchemaVersion4 {
		var document struct {
			Agents []map[string]json.RawMessage `json:"agents"`
		}
		if err := json.Unmarshal(raw, &document); err == nil {
			for i, item := range document.Agents {
				for _, field := range []string{"skill_dirs", "skill_roots"} {
					if _, exists := item[field]; exists {
						return ProjectSpec{}, diagnosticError(Diagnostic{
							Code: DiagnosticInvalidValue, Path: fmt.Sprintf("agents[%d].%s", i, field),
							Message: field + " requires schema_version 4",
						}, nil)
					}
				}
			}
		}
	}
	// v1-v4: reject v5 fields.
	if spec.SchemaVersion < SchemaVersion5 {
		var document struct {
			Workflows []map[string]json.RawMessage `json:"workflows"`
		}
		if err := json.Unmarshal(raw, &document); err == nil {
			for i, item := range document.Workflows {
				if _, exists := item["limits"]; exists {
					return ProjectSpec{}, diagnosticError(Diagnostic{
						Code: DiagnosticInvalidValue, Path: fmt.Sprintf("workflows[%d].limits", i),
						Message: "limits requires schema_version 5",
					}, nil)
				}
			}
		}
	}
	// v1-v5: reject v6 Memory even when its value is null. The explicit field
	// gate keeps old fixtures disabled instead of silently accepting new policy.
	if spec.SchemaVersion < SchemaVersion6 {
		var document map[string]json.RawMessage
		if err := json.Unmarshal(raw, &document); err == nil {
			if _, exists := document["memory"]; exists {
				return ProjectSpec{}, diagnosticError(Diagnostic{Code: DiagnosticInvalidValue, Path: "memory", Message: "memory requires schema_version 6"}, nil)
			}
		}
	}
	return spec, nil
}
