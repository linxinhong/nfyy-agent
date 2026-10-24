package workflow

import (
	"errors"
	"fmt"
)

// ExecutionError reports a failure produced while evaluating workflow logic.
// Callers may return the accompanying RunOutput as an auditable domain result.
type ExecutionError struct {
	Cause error
}

func (e *ExecutionError) Error() string { return e.Cause.Error() }
func (e *ExecutionError) Unwrap() error { return e.Cause }

// IsExecutionError distinguishes auditable workflow failures from storage and
// configuration failures that should be treated as infrastructure errors.
func IsExecutionError(err error) bool {
	var target *ExecutionError
	return errors.As(err, &target)
}

type persistenceError struct {
	operation string
	cause     error
}

func (e *persistenceError) Error() string { return fmt.Sprintf("%s: %v", e.operation, e.cause) }
func (e *persistenceError) Unwrap() error { return e.cause }

func classifyExecutionError(err error) error {
	if err == nil || IsExecutionError(err) {
		return err
	}
	var persistence *persistenceError
	if errors.As(err, &persistence) {
		return err
	}
	return &ExecutionError{Cause: err}
}
