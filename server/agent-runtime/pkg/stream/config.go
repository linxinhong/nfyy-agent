// Package stream defines bounded delivery settings shared by Agent and Workflow streams.
package stream

import (
	"errors"
	"time"
)

const (
	// DefaultBufferItems is the total channel capacity, including the slot
	// reserved for a terminal event.
	DefaultBufferItems = 16
	// DefaultMaxOutputBytes is the cumulative logical-payload budget for one stream.
	DefaultMaxOutputBytes = 4 << 20
	// DefaultDeliveryTimeout bounds how long a producer waits for a stalled
	// consumer after the non-terminal portion of the buffer is full.
	DefaultDeliveryTimeout = 5 * time.Second

	minBufferItems = 2
	minOutputBytes = 512
)

var (
	// ErrOutputBudgetExceeded reports that the cumulative output byte budget
	// was exhausted. The failing payload is not emitted.
	ErrOutputBudgetExceeded = errors.New("stream output budget exceeded")
	// ErrDeliveryTimeout reports that the consumer did not make room before
	// DeliveryTimeout elapsed.
	ErrDeliveryTimeout = errors.New("stream delivery timeout")
	// ErrEndedWithoutTerminal reports an internal abnormal stream exit.
	ErrEndedWithoutTerminal = errors.New("stream ended without a terminal event")
)

// Config bounds one in-process event stream. BufferItems is the total number
// of items retained by the returned channel, including one terminal reserve.
// MaxOutputBytes limits the cumulative logical bytes charged by the runtime's
// event accounting. This deliberately is not an acknowledgement-based in-flight
// count, wire-serialization size, backing-capacity bound, or process-RSS bound:
// a bare receive-only channel exposes none of those measurements. Zero fields
// select finite defaults.
type Config struct {
	BufferItems     int
	MaxOutputBytes  int
	DeliveryTimeout time.Duration
}

// Resolve applies the finite defaults to zero fields.
func (c Config) Resolve() Config {
	if c.BufferItems == 0 {
		c.BufferItems = DefaultBufferItems
	}
	if c.MaxOutputBytes == 0 {
		c.MaxOutputBytes = DefaultMaxOutputBytes
	}
	if c.DeliveryTimeout == 0 {
		c.DeliveryTimeout = DefaultDeliveryTimeout
	}
	return c
}

// Validate checks explicit and resolved stream limits.
func (c Config) Validate() error {
	if c.BufferItems < 0 || c.MaxOutputBytes < 0 || c.DeliveryTimeout < 0 {
		return errors.New("stream limits must not be negative")
	}
	c = c.Resolve()
	if c.BufferItems < minBufferItems {
		return errors.New("stream BufferItems must be at least 2 (one data slot and one terminal slot)")
	}
	if c.MaxOutputBytes < minOutputBytes {
		return errors.New("stream MaxOutputBytes must be at least 512")
	}
	return nil
}
