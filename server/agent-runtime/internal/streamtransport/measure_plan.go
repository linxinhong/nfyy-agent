package streamtransport

import "reflect"

// LogicalPayloadMeasurer compiles the finite type graph of T once, avoiding
// repeated Kind, Type, Size, and NumField reflection while preserving
// LogicalPayloadBytes accounting. Interface payloads remain dynamic and use
// the generic walker. The plan contains only type metadata reachable from T;
// it never caches runtime or user-provided types.
type LogicalPayloadMeasurer[T any] struct {
	root *logicalPayloadPlan
}

type logicalPayloadPlan struct {
	kind   reflect.Kind
	size   int
	elem   *logicalPayloadPlan
	key    *logicalPayloadPlan
	fields []*logicalPayloadPlan
}

// NewLogicalPayloadMeasurer builds a bounded, immutable measurement plan for
// one statically known payload type.
func NewLogicalPayloadMeasurer[T any]() LogicalPayloadMeasurer[T] {
	return LogicalPayloadMeasurer[T]{root: compileLogicalPayloadPlan(reflect.TypeFor[T](), make(map[reflect.Type]*logicalPayloadPlan))}
}

// Bytes returns the same cumulative logical-byte result as
// LogicalPayloadBytes for a value of T and the same limit.
func (m LogicalPayloadMeasurer[T]) Bytes(value T, limit int) int {
	// reflect.ValueOf unwraps an interface T to its dynamic type, so the
	// statically compiled interface plan cannot apply. Keep that uncommon case
	// on the generic path; concrete T values use the plan below.
	if m.root.kind == reflect.Interface {
		return LogicalPayloadBytes(value, limit)
	}
	return m.root.bytes(reflect.ValueOf(value), limit, 0)
}

func compileLogicalPayloadPlan(valueType reflect.Type, seen map[reflect.Type]*logicalPayloadPlan) *logicalPayloadPlan {
	if plan := seen[valueType]; plan != nil {
		return plan
	}
	plan := &logicalPayloadPlan{kind: valueType.Kind(), size: int(valueType.Size())}
	seen[valueType] = plan
	switch plan.kind {
	case reflect.Pointer, reflect.Slice, reflect.Array:
		plan.elem = compileLogicalPayloadPlan(valueType.Elem(), seen)
	case reflect.Map:
		plan.key = compileLogicalPayloadPlan(valueType.Key(), seen)
		plan.elem = compileLogicalPayloadPlan(valueType.Elem(), seen)
	case reflect.Struct:
		plan.fields = make([]*logicalPayloadPlan, valueType.NumField())
		for index := range plan.fields {
			plan.fields[index] = compileLogicalPayloadPlan(valueType.Field(index).Type, seen)
		}
	}
	return plan
}

func (p *logicalPayloadPlan) bytes(value reflect.Value, limit, depth int) int {
	if depth > maxMeasureDepth {
		return -1
	}
	if p.size > limit {
		return limit + 1
	}
	total := p.size
	switch p.kind {
	case reflect.String:
		length := len(value.String())
		if length > limit-total {
			return limit + 1
		}
		return total + length
	case reflect.Interface:
		if value.IsNil() {
			return total
		}
		return addLogicalPayloadBytes(total, retainedBytes(value.Elem(), limit-total, depth+1), limit)
	case reflect.Pointer:
		if value.IsNil() {
			return total
		}
		return addLogicalPayloadBytes(total, p.elem.bytes(value.Elem(), limit-total, depth+1), limit)
	case reflect.Slice:
		if value.IsNil() {
			return total
		}
		for index := 0; index < value.Len(); index++ {
			var ok bool
			total, ok = addLogicalPayloadBytesOK(total, p.elem.bytes(value.Index(index), limit-total, depth+1), limit)
			if !ok {
				break
			}
		}
	case reflect.Array:
		for index := 0; index < value.Len(); index++ {
			var ok bool
			total, ok = addLogicalPayloadBytesOK(total, p.elem.bytes(value.Index(index), limit-total, depth+1), limit)
			if !ok {
				break
			}
		}
	case reflect.Struct:
		for index, field := range p.fields {
			var ok bool
			total, ok = addLogicalPayloadBytesOK(total, field.bytes(value.Field(index), limit-total, depth+1), limit)
			if !ok {
				break
			}
		}
	case reflect.Map:
		if value.IsNil() {
			return total
		}
		iterator := value.MapRange()
		for iterator.Next() {
			var ok bool
			total, ok = addLogicalPayloadBytesOK(total, p.key.bytes(iterator.Key(), limit-total, depth+1), limit)
			if !ok {
				break
			}
			total, ok = addLogicalPayloadBytesOK(total, p.elem.bytes(iterator.Value(), limit-total, depth+1), limit)
			if !ok {
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

func addLogicalPayloadBytes(total, size, limit int) int {
	next, _ := addLogicalPayloadBytesOK(total, size, limit)
	return next
}

func addLogicalPayloadBytesOK(total, size, limit int) (int, bool) {
	if size < 0 {
		return -1, false
	}
	if size > limit-total {
		return limit + 1, false
	}
	return total + size, true
}
