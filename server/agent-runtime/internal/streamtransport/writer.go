// Package streamtransport owns bounded, cancellation-aware channel delivery.
package streamtransport

import (
	"context"
	"reflect"
	"sync"
	"time"

	streamcfg "github.com/linxinhong/xingding-agent-runtime/pkg/stream"
)

const (
	terminalByteReserve = 256
	fullBufferPoll      = time.Millisecond
)

// Kind tells Writer whether an event is the one terminal event for the stream.
type Kind uint8

const (
	Data Kind = iota
	Critical
	Terminal
)

// Writer serializes all sends to one bounded output channel. It deliberately
// keeps the channel API: therefore bytes are accounted cumulatively, while the
// channel capacity is the exact buffered-item bound.
type Writer[T any] struct {
	mu       sync.Mutex
	out      chan T
	ctx      context.Context
	cancel   context.CancelFunc
	cfg      streamcfg.Config
	measure  func(T, int) int
	failure  func(error) T
	timer    *time.Timer
	bytes    int
	terminal bool
	closed   bool
}

// New returns a writer and its receive-only bounded channel. cancel must
// cancel ctx so a delivery failure propagates to cooperative producers.
func New[T any](ctx context.Context, cancel context.CancelFunc, cfg streamcfg.Config, measure func(T, int) int, failure func(error) T) (Writer[T], <-chan T) {
	cfg = cfg.Resolve()
	out := make(chan T, cfg.BufferItems)
	return Writer[T]{ctx: ctx, cancel: cancel, cfg: cfg, out: out, measure: measure, failure: failure}, out
}

// Emit admits one event. Calls may be concurrent; a single mutex owns ordering,
// cumulative accounting, terminal selection, and the one reusable wait timer.
func (w *Writer[T]) Emit(event T, kind Kind) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.terminal {
		return false
	}

	limit := w.cfg.MaxOutputBytes
	if kind != Terminal {
		limit -= terminalByteReserve
	}
	size := w.measure(event, limit-w.bytes)
	if size < 0 || size > limit-w.bytes {
		w.failLocked(streamcfg.ErrOutputBudgetExceeded)
		return false
	}

	if kind != Terminal {
		if err := w.waitForDataSlotLocked(); err != nil {
			w.failLocked(err)
			return false
		}
	} else if len(w.out) >= cap(w.out) {
		// Non-terminal events never consume the final slot, so this is only
		// reachable if the writer invariant is broken. Fail closed.
		w.failLocked(streamcfg.ErrDeliveryTimeout)
		return false
	}

	w.out <- event
	w.bytes += size
	if kind == Terminal {
		w.terminal = true
	}
	return true
}

// Finish guarantees one recognizable terminal event and closes the channel.
func (w *Writer[T]) Finish(cause error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	if !w.terminal {
		if cause == nil {
			cause = streamcfg.ErrEndedWithoutTerminal
		}
		w.failLocked(cause)
	}
	w.closed = true
	if w.timer != nil {
		if !w.timer.Stop() {
			select {
			case <-w.timer.C:
			default:
			}
		}
	}
	w.cancel()
	close(w.out)
}

func (w *Writer[T]) waitForDataSlotLocked() error {
	if len(w.out) < cap(w.out)-1 {
		return w.ctx.Err()
	}

	deadline := time.Now().Add(w.cfg.DeliveryTimeout)
	for len(w.out) >= cap(w.out)-1 {
		if err := w.ctx.Err(); err != nil {
			return err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return streamcfg.ErrDeliveryTimeout
		}
		wait := min(remaining, fullBufferPoll)
		if w.timer == nil {
			w.timer = time.NewTimer(wait)
		} else {
			w.timer.Reset(wait)
		}
		select {
		case <-w.timer.C:
		case <-w.ctx.Done():
			if !w.timer.Stop() {
				select {
				case <-w.timer.C:
				default:
				}
			}
			return w.ctx.Err()
		}
	}
	return w.ctx.Err()
}

func (w *Writer[T]) failLocked(cause error) {
	if w.terminal {
		return
	}
	event := w.failure(cause)
	size := w.measure(event, min(terminalByteReserve, w.cfg.MaxOutputBytes-w.bytes))
	if size < 0 || size > terminalByteReserve || size > w.cfg.MaxOutputBytes-w.bytes {
		// Failure factories are intentionally bounded. If a caller violates
		// that contract, use the canonical smallest failure instead.
		event = w.failure(streamcfg.ErrOutputBudgetExceeded)
		size = w.measure(event, min(terminalByteReserve, w.cfg.MaxOutputBytes-w.bytes))
		if size < 0 || size > terminalByteReserve || size > w.cfg.MaxOutputBytes-w.bytes {
			// The generic caller violated the bounded failure-factory contract.
			// Cancel and close at Finish; never emit an unmeasured value or
			// corrupt accounting by adding a negative size.
			w.terminal = true
			w.cancel()
			return
		}
	}
	// The data path reserves one item and terminalByteReserve bytes.
	w.out <- event
	w.bytes += size
	w.terminal = true
	w.cancel()
}

const maxMeasureDepth = 32

// LogicalPayloadBytes charges the recursively visible Go value: fixed value
// widths plus string lengths and the logical elements/entries of arrays,
// slices, and maps. Aliases are charged repeatedly. It intentionally excludes
// unused backing capacity, allocator/RSS overhead, and wire serialization.
// Cycles, unsupported runtime values, excessive depth, and values over limit
// fail closed with -1 or a value greater than limit.
func LogicalPayloadBytes(value any, limit int) int {
	if value == nil {
		return 0
	}
	return retainedBytes(reflect.ValueOf(value), limit, 0)
}

func retainedBytes(value reflect.Value, limit, depth int) int {
	if !value.IsValid() {
		return 0
	}
	if depth > maxMeasureDepth {
		return -1
	}
	typeSize := int(value.Type().Size())
	if typeSize > limit {
		return limit + 1
	}
	total := typeSize
	add := func(size int) bool {
		if size < 0 {
			total = -1
			return false
		}
		if size > limit-total {
			total = limit + 1
			return false
		}
		total += size
		return true
	}
	switch value.Kind() {
	case reflect.String:
		if value.Len() > limit-total {
			return limit + 1
		}
		return total + value.Len()
	case reflect.Interface, reflect.Pointer:
		if value.IsNil() {
			return total
		}
		add(retainedBytes(value.Elem(), limit-total, depth+1))
	case reflect.Slice:
		if value.IsNil() {
			return total
		}
		for index := 0; index < value.Len(); index++ {
			if !add(retainedBytes(value.Index(index), limit-total, depth+1)) {
				break
			}
		}
	case reflect.Array:
		for index := 0; index < value.Len(); index++ {
			if !add(retainedBytes(value.Index(index), limit-total, depth+1)) {
				break
			}
		}
	case reflect.Struct:
		for index := 0; index < value.NumField(); index++ {
			if !add(retainedBytes(value.Field(index), limit-total, depth+1)) {
				break
			}
		}
	case reflect.Map:
		if value.IsNil() {
			return total
		}
		iterator := value.MapRange()
		for iterator.Next() {
			if !add(retainedBytes(iterator.Key(), limit-total, depth+1)) ||
				!add(retainedBytes(iterator.Value(), limit-total, depth+1)) {
				break
			}
		}
	case reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128:
		return total
	default:
		return -1
	}
	return total
}
