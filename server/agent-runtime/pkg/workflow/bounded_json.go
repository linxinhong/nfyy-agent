package workflow

import (
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const maxDynamicPayloadDepth = 64
const maxJSONTraversalDepth = 128

var (
	jsonMarshalerType = reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	textMarshalerType = reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem()
	timeType          = reflect.TypeOf(time.Time{})
	rawMessageType    = reflect.TypeOf(json.RawMessage{})
)

type jsonSizeOverflow struct{ actual int }

func (e *jsonSizeOverflow) Error() string { return "JSON byte limit exceeded" }

type jsonVisit struct {
	typ reflect.Type
	ptr uintptr
	len int
}

// boundedJSONSizer mirrors encoding/json's compact output size while stopping
// as soon as the configured budget is crossed. Dynamic interface values are
// deliberately more restrictive than encoding/json: they never invoke user
// marshal methods and accept only the documented JSON-native value domain.
type boundedJSONSizer struct {
	limit   int
	size    int
	active  [maxJSONTraversalDepth]jsonVisit
	activeN int
}

func measureDynamicJSON(value any, limit int) (int, error) {
	s := boundedJSONSizer{limit: limit}
	if err := s.dynamic(reflect.ValueOf(value), 1); err != nil {
		return 0, err
	}
	return s.size, nil
}

func measureInternalJSON(value any, limit int) (int, error) {
	s := boundedJSONSizer{limit: limit}
	if err := s.internal(reflect.ValueOf(value)); err != nil {
		return 0, err
	}
	return s.size, nil
}

func (s *boundedJSONSizer) add(n int) error {
	if n < 0 || n > s.limit-s.size {
		actual := s.limit + 1
		if n >= 0 && s.size <= s.limit && n <= int(^uint(0)>>1)-s.size {
			actual = s.size + n
		}
		return &jsonSizeOverflow{actual: actual}
	}
	s.size += n
	return nil
}

func (s *boundedJSONSizer) literal(v string) error { return s.add(len(v)) }

func (s *boundedJSONSizer) quoted(v string) error {
	if err := s.add(1); err != nil {
		return err
	}
	for i := 0; i < len(v); {
		c := v[i]
		if c < utf8.RuneSelf {
			n := 1
			switch c {
			case '\\', '"', '\b', '\f', '\n', '\r', '\t':
				n = 2
			default:
				if c < 0x20 || c == '<' || c == '>' || c == '&' {
					n = 6
				}
			}
			if err := s.add(n); err != nil {
				return err
			}
			i++
			continue
		}
		r, n := utf8.DecodeRuneInString(v[i:])
		encoded := n
		if r == utf8.RuneError && n == 1 || r == '\u2028' || r == '\u2029' {
			encoded = 6
		}
		if err := s.add(encoded); err != nil {
			return err
		}
		i += n
	}
	return s.add(1)
}

func (s *boundedJSONSizer) dynamic(v reflect.Value, depth int) error {
	if depth > maxDynamicPayloadDepth {
		return errors.New("payload nesting exceeds 64")
	}
	if !v.IsValid() {
		return s.literal("null")
	}
	if v.Kind() == reflect.Interface {
		if v.IsNil() {
			return s.literal("null")
		}
		return s.dynamic(v.Elem(), depth)
	}

	t := v.Type()
	if t == rawMessageType || v.Kind() == reflect.Slice && v.Type().Elem().Kind() == reflect.Uint8 {
		return errors.New("[]byte and RawMessage are not allowed limited payloads")
	}
	if t == reflect.TypeOf(json.Number("")) {
		value := v.Interface().(json.Number).String()
		if len(value) > s.limit-s.size {
			return s.add(len(value))
		}
		if !validJSONNumber(value) {
			return errors.New("invalid json.Number")
		}
		return s.literal(value)
	}
	if t.PkgPath() != "" || implementsMarshaler(t) {
		return fmt.Errorf("unsupported dynamic type %s", t)
	}

	switch v.Kind() {
	case reflect.Bool:
		if v.Bool() {
			return s.literal("true")
		}
		return s.literal("false")
	case reflect.String:
		return s.quoted(v.String())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return s.add(signedDecimalSize(v.Int()))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return s.add(unsignedDecimalSize(v.Uint()))
	case reflect.Float32, reflect.Float64:
		return s.floating(v.Float(), v.Type().Bits())
	case reflect.Map:
		if v.Type().Key().Kind() != reflect.String {
			return fmt.Errorf("unsupported dynamic map key %s", v.Type().Key())
		}
		if v.IsNil() {
			return s.literal("null")
		}
		return s.dynamicMap(v, depth)
	case reflect.Slice:
		if v.IsNil() {
			return s.literal("null")
		}
		return s.dynamicSequence(v, depth)
	case reflect.Array:
		return s.dynamicSequence(v, depth)
	default:
		return fmt.Errorf("unsupported dynamic type %s", t)
	}
}

func (s *boundedJSONSizer) dynamicSequence(v reflect.Value, depth int) error {
	entered, err := s.enter(v)
	if err != nil {
		return err
	}
	if entered {
		defer s.leave()
	}
	if err := s.add(1); err != nil {
		return err
	}
	for i := 0; i < v.Len(); i++ {
		if i > 0 {
			if err := s.add(1); err != nil {
				return err
			}
		}
		if err := s.dynamic(v.Index(i), depth+1); err != nil {
			return err
		}
	}
	return s.add(1)
}

func (s *boundedJSONSizer) dynamicMap(v reflect.Value, depth int) error {
	entered, err := s.enter(v)
	if err != nil {
		return err
	}
	if entered {
		defer s.leave()
	}
	if err := s.add(1); err != nil {
		return err
	}
	iter := v.MapRange()
	first := true
	for iter.Next() {
		if !first {
			if err := s.add(1); err != nil {
				return err
			}
		}
		first = false
		if err := s.quoted(iter.Key().String()); err != nil {
			return err
		}
		if err := s.add(1); err != nil {
			return err
		}
		if err := s.dynamic(iter.Value(), depth+1); err != nil {
			return err
		}
	}
	return s.add(1)
}

func (s *boundedJSONSizer) internal(v reflect.Value) error {
	if !v.IsValid() {
		return s.literal("null")
	}
	if v.Kind() == reflect.Interface {
		if v.IsNil() {
			return s.literal("null")
		}
		return s.dynamic(v.Elem(), 1)
	}
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return s.literal("null")
		}
		entered, err := s.enter(v)
		if err != nil {
			return err
		}
		if entered {
			defer s.leave()
		}
		return s.internal(v.Elem())
	}

	t := v.Type()
	if t == timeType {
		n, err := timeJSONSize(v.Interface().(time.Time))
		if err != nil {
			return err
		}
		return s.add(n)
	}
	if t == rawMessageType {
		raw := v.Interface().(json.RawMessage)
		if raw == nil {
			return s.literal("null")
		}
		n, err := compactRawJSONSize(raw, s.limit-s.size)
		if err != nil {
			return err
		}
		return s.add(n)
	}
	if t == reflect.TypeOf(json.Number("")) {
		value := v.Interface().(json.Number).String()
		if len(value) > s.limit-s.size {
			return s.add(len(value))
		}
		if !validJSONNumber(value) {
			return errors.New("invalid json.Number")
		}
		return s.literal(value)
	}

	switch v.Kind() {
	case reflect.Bool:
		if v.Bool() {
			return s.literal("true")
		}
		return s.literal("false")
	case reflect.String:
		return s.quoted(v.String())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return s.add(signedDecimalSize(v.Int()))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return s.add(unsignedDecimalSize(v.Uint()))
	case reflect.Float32, reflect.Float64:
		return s.floating(v.Float(), v.Type().Bits())
	case reflect.Struct:
		return s.structure(v)
	case reflect.Map:
		if v.Type().Key().Kind() != reflect.String {
			return fmt.Errorf("unsupported internal map key %s", v.Type().Key())
		}
		if v.IsNil() {
			return s.literal("null")
		}
		return s.internalMap(v)
	case reflect.Slice:
		if v.IsNil() {
			return s.literal("null")
		}
		return s.internalSequence(v)
	case reflect.Array:
		return s.internalSequence(v)
	default:
		return fmt.Errorf("unsupported internal JSON type %s", t)
	}
}

func (s *boundedJSONSizer) structure(v reflect.Value) error {
	if err := s.add(1); err != nil {
		return err
	}
	first := true
	t := v.Type()
	for i := 0; i < v.NumField(); i++ {
		field := t.Field(i)
		if field.PkgPath != "" {
			continue
		}
		tag := field.Tag.Get("json")
		fieldName, options, _ := strings.Cut(tag, ",")
		if fieldName == "-" {
			continue
		}
		name := field.Name
		if fieldName != "" {
			name = fieldName
		}
		omitEmpty := false
		for options != "" {
			option, rest, found := strings.Cut(options, ",")
			if option == "omitempty" {
				omitEmpty = true
			}
			options = rest
			if !found {
				break
			}
		}
		fv := v.Field(i)
		if omitEmpty && isJSONEmptyValue(fv) {
			continue
		}
		if !first {
			if err := s.add(1); err != nil {
				return err
			}
		}
		first = false
		if err := s.quoted(name); err != nil {
			return err
		}
		if err := s.add(1); err != nil {
			return err
		}
		if fv.Kind() == reflect.Interface {
			if fv.IsNil() {
				if err := s.literal("null"); err != nil {
					return err
				}
			} else if err := s.dynamic(fv.Elem(), 1); err != nil {
				return err
			}
		} else if err := s.internal(fv); err != nil {
			return err
		}
	}
	return s.add(1)
}

func (s *boundedJSONSizer) internalSequence(v reflect.Value) error {
	entered, err := s.enter(v)
	if err != nil {
		return err
	}
	if entered {
		defer s.leave()
	}
	if err := s.add(1); err != nil {
		return err
	}
	for i := 0; i < v.Len(); i++ {
		if i > 0 {
			if err := s.add(1); err != nil {
				return err
			}
		}
		if v.Type().Elem().Kind() == reflect.Interface {
			if err := s.dynamic(v.Index(i), 1); err != nil {
				return err
			}
		} else if err := s.internal(v.Index(i)); err != nil {
			return err
		}
	}
	return s.add(1)
}

func (s *boundedJSONSizer) internalMap(v reflect.Value) error {
	entered, err := s.enter(v)
	if err != nil {
		return err
	}
	if entered {
		defer s.leave()
	}
	if err := s.add(1); err != nil {
		return err
	}
	iter := v.MapRange()
	first := true
	for iter.Next() {
		if !first {
			if err := s.add(1); err != nil {
				return err
			}
		}
		first = false
		if err := s.quoted(iter.Key().String()); err != nil {
			return err
		}
		if err := s.add(1); err != nil {
			return err
		}
		value := iter.Value()
		if v.Type().Elem().Kind() == reflect.Interface {
			if err := s.dynamic(value, 1); err != nil {
				return err
			}
		} else if err := s.internal(value); err != nil {
			return err
		}
	}
	return s.add(1)
}

func (s *boundedJSONSizer) floating(value float64, bits int) error {
	if math.IsInf(value, 0) || math.IsNaN(value) {
		return errors.New("unsupported non-finite float")
	}
	abs := math.Abs(value)
	format := byte('f')
	if abs != 0 && (abs < 1e-6 || abs >= 1e21) {
		format = 'e'
	}
	var scratch [32]byte
	encoded := strconv.AppendFloat(scratch[:0], value, format, -1, bits)
	if format == 'e' {
		if i := len(encoded) - 4; i >= 0 && encoded[i] == 'e' && encoded[i+2] == '0' {
			return s.add(len(encoded) - 1)
		}
	}
	return s.add(len(encoded))
}

func (s *boundedJSONSizer) enter(v reflect.Value) (bool, error) {
	if v.Kind() != reflect.Pointer && v.Kind() != reflect.Map && v.Kind() != reflect.Slice {
		return false, nil
	}
	if v.IsNil() {
		return false, nil
	}
	var ptr uintptr
	switch v.Kind() {
	case reflect.Pointer, reflect.Slice:
		ptr = v.Pointer()
	case reflect.Map:
		ptr = v.Pointer()
	}
	key := jsonVisit{typ: v.Type(), ptr: ptr}
	if v.Kind() == reflect.Slice {
		key.len = v.Len()
	}
	for i := 0; i < s.activeN; i++ {
		if s.active[i] == key {
			return false, errors.New("unsupported cyclic payload")
		}
	}
	if s.activeN == len(s.active) {
		return false, errors.New("internal JSON traversal exceeds bounded depth")
	}
	s.active[s.activeN] = key
	s.activeN++
	return true, nil
}

func (s *boundedJSONSizer) leave() { s.activeN-- }

func implementsMarshaler(t reflect.Type) bool {
	return t.Implements(jsonMarshalerType) || t.Implements(textMarshalerType) ||
		reflect.PointerTo(t).Implements(jsonMarshalerType) || reflect.PointerTo(t).Implements(textMarshalerType)
}

func isJSONEmptyValue(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return v.Len() == 0
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64, reflect.Interface, reflect.Pointer:
		return v.IsZero()
	}
	return false
}

func validJSONNumber(value string) bool {
	if value == "" {
		return false
	}
	i := 0
	if value[i] == '-' {
		i++
		if i == len(value) {
			return false
		}
	}
	if value[i] == '0' {
		i++
	} else if value[i] >= '1' && value[i] <= '9' {
		for i < len(value) && value[i] >= '0' && value[i] <= '9' {
			i++
		}
	} else {
		return false
	}
	if i < len(value) && value[i] == '.' {
		i++
		start := i
		for i < len(value) && value[i] >= '0' && value[i] <= '9' {
			i++
		}
		if i == start {
			return false
		}
	}
	if i < len(value) && (value[i] == 'e' || value[i] == 'E') {
		i++
		if i < len(value) && (value[i] == '+' || value[i] == '-') {
			i++
		}
		start := i
		for i < len(value) && value[i] >= '0' && value[i] <= '9' {
			i++
		}
		if i == start {
			return false
		}
	}
	return i == len(value)
}

func unsignedDecimalSize(value uint64) int {
	size := 1
	for value >= 10 {
		value /= 10
		size++
	}
	return size
}

func timeJSONSize(value time.Time) (int, error) {
	year := value.Year()
	if year < 0 || year >= 10000 {
		return 0, errors.New("time year outside of range [0,9999]")
	}
	_, offset := value.Zone()
	zone := offset
	if zone < 0 {
		zone = -zone
	}
	if zone/3600 >= 24 {
		return 0, errors.New("time zone offset hour outside of range [0,23]")
	}
	// Quotes plus YYYY-MM-DDTHH:MM:SS and either Z or ±HH:MM.
	size := 2 + len("2006-01-02T15:04:05") + 1
	if offset != 0 {
		size += 5
	}
	nanos := value.Nanosecond()
	if nanos != 0 {
		digits := 9
		for nanos%10 == 0 {
			nanos /= 10
			digits--
		}
		size += 1 + digits
	}
	return size, nil
}

// cloneLimitedState copies an already-validated JSON-native state without a
// marshal/unmarshal round trip, preserving the concrete numeric types seen by
// downstream executors.
func cloneLimitedState(state map[string]any) map[string]any {
	if state == nil {
		return map[string]any{}
	}
	return cloneLimitedJSONValue(reflect.ValueOf(state)).Interface().(map[string]any)
}

func cloneLimitedJSONValue(value reflect.Value) reflect.Value {
	if value.Kind() == reflect.Interface {
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		clone := cloneLimitedJSONValue(value.Elem())
		out := reflect.New(value.Type()).Elem()
		out.Set(clone)
		return out
	}
	switch value.Kind() {
	case reflect.Map:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		out := reflect.MakeMapWithSize(value.Type(), value.Len())
		iter := value.MapRange()
		for iter.Next() {
			out.SetMapIndex(iter.Key(), cloneLimitedJSONValue(iter.Value()))
		}
		return out
	case reflect.Slice:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		out := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for i := 0; i < value.Len(); i++ {
			out.Index(i).Set(cloneLimitedJSONValue(value.Index(i)))
		}
		return out
	case reflect.Array:
		out := reflect.New(value.Type()).Elem()
		for i := 0; i < value.Len(); i++ {
			out.Index(i).Set(cloneLimitedJSONValue(value.Index(i)))
		}
		return out
	default:
		return value
	}
}

func cloneExecutionState(state map[string]any, limited bool) map[string]any {
	if limited {
		return cloneLimitedState(state)
	}
	return cloneMap(state)
}
