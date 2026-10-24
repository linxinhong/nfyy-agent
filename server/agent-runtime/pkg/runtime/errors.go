package runtime

import (
	"fmt"
	"strings"
)

// DiagnosticCode classifies a project decoding, validation, or construction problem.
type DiagnosticCode string

const (
	DiagnosticRequired                 DiagnosticCode = "required"
	DiagnosticDuplicate                DiagnosticCode = "duplicate"
	DiagnosticInvalidValue             DiagnosticCode = "invalid_value"
	DiagnosticUnknownReference         DiagnosticCode = "unknown_reference"
	DiagnosticUnsupportedSchemaVersion DiagnosticCode = "unsupported_schema_version"
	DiagnosticInvalidJSON              DiagnosticCode = "invalid_json"
	DiagnosticInvalidWorkflow          DiagnosticCode = "invalid_workflow"
	DiagnosticConflictingConfiguration DiagnosticCode = "conflicting_configuration"
	DiagnosticNotSerializable          DiagnosticCode = "not_serializable"
	DiagnosticConstructionFailed       DiagnosticCode = "construction_failed"
	DiagnosticInvalidSchema            DiagnosticCode = "invalid_schema"
)

// Diagnostic identifies one actionable problem at a stable project field path.
type Diagnostic struct {
	Code        DiagnosticCode `json:"code"`
	Path        string         `json:"path,omitempty"`
	ComponentID string         `json:"component_id,omitempty"`
	Message     string         `json:"message"`
}

// BuildError contains the ordered diagnostics produced before or during assembly.
type BuildError struct {
	Diagnostics []Diagnostic `json:"diagnostics"`
	causes      []error
}

func (e *BuildError) Error() string {
	if e == nil || len(e.Diagnostics) == 0 {
		return "runtime: project build failed"
	}
	parts := make([]string, 0, len(e.Diagnostics))
	for _, diagnostic := range e.Diagnostics {
		location := diagnostic.Path
		if location == "" {
			location = "project"
		}
		parts = append(parts, fmt.Sprintf("%s: %s", location, diagnostic.Message))
	}
	return "runtime: project build failed: " + strings.Join(parts, "; ")
}

func (e *BuildError) Unwrap() []error {
	if e == nil {
		return nil
	}
	return e.causes
}

func diagnosticError(diagnostic Diagnostic, cause error) error {
	err := &BuildError{Diagnostics: []Diagnostic{diagnostic}}
	if cause != nil {
		err.causes = []error{cause}
	}
	return err
}

func constructionError(path string, cause error) error {
	return diagnosticError(Diagnostic{
		Code: DiagnosticConstructionFailed, Path: path, Message: cause.Error(),
	}, cause)
}
