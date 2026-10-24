package streamtransport

import (
	"reflect"
	"testing"
)

type plannedPayload struct {
	Name    string
	Values  []int64
	Count   int32
	Next    *plannedPayload
	Dynamic any
	Mapping map[string][]byte
}

type unsupportedPlannedPayload struct {
	Callback func()
	Signal   chan struct{}
}

type paddedPlannedPayload struct {
	Small uint8
	Wide  uint64
}

func TestLogicalPayloadMeasurerMatchesFrozenReference(t *testing.T) {
	shared := &plannedPayload{Name: "shared", Values: []int64{1, 2, 3}}
	typedNil := (*plannedPayload)(nil)
	cyclic := &plannedPayload{Name: "cycle"}
	cyclic.Next = cyclic
	withinDepth := plannedPayloadChain(16)
	beyondDepth := plannedPayloadChain(17)

	for name, value := range map[string]*plannedPayload{
		"typed nil":    nil,
		"empty":        {},
		"composite":    {Name: "composite", Values: []int64{1, 2, 3, 5, 8}, Count: 13, Dynamic: typedNil},
		"aliases":      {Name: "aliases", Dynamic: []any{shared, shared}, Mapping: map[string][]byte{"a": {1, 2}, "b": {3, 4}}},
		"cycle":        cyclic,
		"depth 32":     withinDepth,
		"beyond depth": beyondDepth,
	} {
		t.Run(name, func(t *testing.T) { assertLogicalPlanMatches(t, value) })
	}
	measurer := NewLogicalPayloadMeasurer[*plannedPayload]()
	if got := measurer.Bytes(nil, 1<<20); got != int(reflect.TypeFor[*plannedPayload]().Size()) {
		t.Fatalf("typed nil size = %d, want pointer width", got)
	}
	if got := measurer.Bytes(withinDepth, 1<<20); got < 0 {
		t.Fatalf("depth-32 payload unexpectedly rejected: %d", got)
	}
	if got := measurer.Bytes(beyondDepth, 1<<20); got >= 0 {
		t.Fatalf("depth-33 payload size = %d, want fail closed", got)
	}
	if got := measurer.Bytes(cyclic, 1<<20); got >= 0 {
		t.Fatalf("cyclic payload size = %d, want fail closed", got)
	}
	t.Run("slice len excludes capacity", func(t *testing.T) {
		shortCapacity := []plannedPayload{{Name: "slice"}, {Values: make([]int64, 2)}}
		largeCapacity := []plannedPayload{{Name: "slice"}, {Values: make([]int64, 2, 32)}}
		assertLogicalPlanMatches(t, shortCapacity)
		assertLogicalPlanMatches(t, largeCapacity)
		if got, want := NewLogicalPayloadMeasurer[[]plannedPayload]().Bytes(largeCapacity, 1<<20), NewLogicalPayloadMeasurer[[]plannedPayload]().Bytes(shortCapacity, 1<<20); got != want {
			t.Fatalf("large-capacity size = %d, want same logical size %d", got, want)
		}
	})
	t.Run("map", func(t *testing.T) {
		assertLogicalPlanMatches(t, map[string]plannedPayload{"left": {Name: "one"}, "right": {Values: []int64{2, 3}}})
	})
	t.Run("unsupported func and chan", func(t *testing.T) {
		assertLogicalPlanMatches(t, unsupportedPlannedPayload{})
		assertLogicalPlanMatches(t, (func())(nil))
		assertLogicalPlanMatches(t, (chan struct{})(nil))
	})
	t.Run("padding and array repetition", func(t *testing.T) {
		value := [3]paddedPlannedPayload{{Small: 1, Wide: 2}, {}, {Small: 3}}
		assertLogicalPlanMatches(t, value)
		if got, want := NewLogicalPayloadMeasurer[[3]paddedPlannedPayload]().Bytes(value, 1<<20), frozenLogicalPayloadBytes(value, 1<<20); got != want {
			t.Fatalf("padded array size = %d, want fixed-width repeated size %d", got, want)
		}
		expected := int(reflect.TypeFor[[3]paddedPlannedPayload]().Size()) +
			3*(int(reflect.TypeFor[paddedPlannedPayload]().Size())+int(reflect.TypeFor[uint8]().Size())+int(reflect.TypeFor[uint64]().Size()))
		if got := NewLogicalPayloadMeasurer[[3]paddedPlannedPayload]().Bytes(value, 1<<20); got != expected {
			t.Fatalf("padded array size = %d, want explicit repeated-width total %d", got, expected)
		}
	})

	interfaceMeasurer := NewLogicalPayloadMeasurer[any]()
	for _, value := range []any{nil, typedNil, shared, "dynamic"} {
		if got, want := interfaceMeasurer.Bytes(value, 1<<20), frozenLogicalPayloadBytes(value, 1<<20); got != want {
			t.Fatalf("interface plan size = %d, want frozen %d for %#v", got, want, value)
		}
	}
}

func plannedPayloadChain(nodes int) *plannedPayload {
	root := &plannedPayload{}
	cursor := root
	for index := 1; index < nodes; index++ {
		cursor.Next = &plannedPayload{}
		cursor = cursor.Next
	}
	return root
}

func assertLogicalPlanMatches[T any](t *testing.T, value T) {
	t.Helper()
	measurer := NewLogicalPayloadMeasurer[T]()
	full := frozenLogicalPayloadBytes(value, 1<<20)
	limits := []int{0, 1, 7, 8, 16, 64, 512, 1 << 20}
	if full >= 0 && full < 1<<20 {
		limits = append(limits, full-1, full, full+1)
	}
	for _, limit := range limits {
		got := measurer.Bytes(value, limit)
		want := frozenLogicalPayloadBytes(value, limit)
		if got != want {
			t.Fatalf("plan size at limit %d = %d, want frozen %d", limit, got, want)
		}
	}
}

// frozenLogicalPayloadBytes is the pre-PERF-002 algorithm. It intentionally
// remains test-only and structurally independent from the compiled plan.
func frozenLogicalPayloadBytes(value any, limit int) int {
	if value == nil {
		return 0
	}
	return frozenRetainedBytes(reflect.ValueOf(value), limit, 0)
}

func frozenRetainedBytes(value reflect.Value, limit, depth int) int {
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
		add(frozenRetainedBytes(value.Elem(), limit-total, depth+1))
	case reflect.Slice:
		if value.IsNil() {
			return total
		}
		for index := 0; index < value.Len(); index++ {
			if !add(frozenRetainedBytes(value.Index(index), limit-total, depth+1)) {
				break
			}
		}
	case reflect.Array:
		for index := 0; index < value.Len(); index++ {
			if !add(frozenRetainedBytes(value.Index(index), limit-total, depth+1)) {
				break
			}
		}
	case reflect.Struct:
		for index := 0; index < value.NumField(); index++ {
			if !add(frozenRetainedBytes(value.Field(index), limit-total, depth+1)) {
				break
			}
		}
	case reflect.Map:
		if value.IsNil() {
			return total
		}
		iterator := value.MapRange()
		for iterator.Next() {
			if !add(frozenRetainedBytes(iterator.Key(), limit-total, depth+1)) ||
				!add(frozenRetainedBytes(iterator.Value(), limit-total, depth+1)) {
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
