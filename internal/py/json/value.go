// Package json ports the parts of CPython's json module that agent-decisions depends on byte for byte:
// json.loads, json.dumps, and the Python value semantics (dict order, truthiness, ==) of decoded values.
//
// A decoded value is nil (None), bool, Int, float64, string, []any (list) or *Object (dict). Dumps, Truthy
// and Equal also accept Go int and int64 as a Python int and []string as a list of str.
//
// Strings are WTF-8: a Python str may hold a lone surrogate, which a Go string keeps as its 3-byte
// generalized UTF-8 encoding (0xED 0xA0-0xBF 0x80-0xBF).
package json

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"

	"github.com/rexbrahh/make-better-decisions/internal/py/pyerr"
	"github.com/rexbrahh/make-better-decisions/internal/py/str"
)

// Object is a Python dict with string keys. It keeps insertion order, and assigning an existing key keeps
// the key's position. The zero value is an empty dict.
//
// py: Objects/dictobject.c dict (Python 3.15)
type Object struct {
	keys   []string
	values map[string]any
}

// NewObject returns a dict built from alternating string keys and values, like a dict literal. It panics on
// an odd count or a non-string key.
//
// py: Objects/dictobject.c dict (Python 3.15)
func NewObject(pairs ...any) *Object {
	if len(pairs)%2 != 0 {
		panic("json.NewObject needs alternating keys and values")
	}
	object := &Object{}
	for i := 0; i < len(pairs); i += 2 {
		key, ok := pairs[i].(string)
		if !ok {
			panic(fmt.Sprintf("json.NewObject key %d is %T, not string", i/2, pairs[i]))
		}
		object.Set(key, pairs[i+1])
	}
	return object
}

// Get returns the value for key and whether the key is present, like `key in d` and d[key].
//
// py: Objects/dictobject.c dict.__getitem__ (Python 3.15)
func (o *Object) Get(key string) (any, bool) {
	value, ok := o.values[key]
	return value, ok
}

// Value returns the value for key, or nil when the key is missing.
//
// py: Objects/dictobject.c dict.get (Python 3.15)
func (o *Object) Value(key string) any {
	return o.values[key]
}

// ValueOr returns o.get(key, fallback).
//
// py: Objects/dictobject.c dict.get (Python 3.15)
func (o *Object) ValueOr(key string, fallback any) any {
	if value, present := o.Get(key); present {
		return value
	}
	return fallback
}

// Item returns o[key], with the KeyError Python raises for a missing key.
//
// py: Objects/dictobject.c dict_subscript (Python 3.15)
func (o *Object) Item(key string) (any, error) {
	if value, present := o.Get(key); present {
		return value, nil
	}
	return nil, &pyerr.Error{Class: "KeyError", Msg: str.Repr(key)}
}

// Set assigns value to key. A new key goes last; an existing key keeps its position.
//
// py: Objects/dictobject.c dict.__setitem__ (Python 3.15)
func (o *Object) Set(key string, value any) {
	if o.values == nil {
		o.values = map[string]any{}
	}
	if _, ok := o.values[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.values[key] = value
}

// Delete removes key and reports whether it was present.
//
// py: Objects/dictobject.c dict.pop (Python 3.15)
func (o *Object) Delete(key string) bool {
	if _, ok := o.values[key]; !ok {
		return false
	}
	delete(o.values, key)
	for i, k := range o.keys {
		if k == key {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
	return true
}

// Keys returns a copy of the keys in insertion order.
//
// py: Objects/dictobject.c dict.keys (Python 3.15)
func (o *Object) Keys() []string {
	return append([]string(nil), o.keys...)
}

// Len returns the number of keys.
//
// py: Objects/dictobject.c dict.__len__ (Python 3.15)
func (o *Object) Len() int {
	return len(o.keys)
}

// Copy returns a shallow copy, like dict(d).
//
// py: Objects/dictobject.c dict.copy (Python 3.15)
func (o *Object) Copy() *Object {
	copied := &Object{keys: o.Keys(), values: make(map[string]any, len(o.values))}
	for key, value := range o.values {
		copied.values[key] = value
	}
	return copied
}

// Int is a Python int of any size, held as its canonical decimal text ("0", "-12"): no sign on zero and no
// leading zeros.
//
// py: Objects/longobject.c int (Python 3.15)
type Int string

// IntOf returns n as an Int.
//
// py: Objects/longobject.c int.__repr__ (Python 3.15)
func IntOf(n int64) Int {
	return Int(strconv.FormatInt(n, 10))
}

// Int64 returns i as an int64, and false when it does not fit.
//
// py: Objects/longobject.c PyLong_AsLongLongAndOverflow (Python 3.15)
func (i Int) Int64() (int64, bool) {
	n, err := strconv.ParseInt(string(i), 10, 64)
	return n, err == nil
}

// Truthy returns bool(v).
//
// py: Objects/object.c PyObject_IsTrue (Python 3.15)
func Truthy(v any) bool {
	switch v := v.(type) {
	case nil:
		return false
	case bool:
		return v
	case Int:
		return v != "0"
	case int:
		return v != 0
	case int64:
		return v != 0
	case float64:
		return v != 0 // NaN is true, as in Python
	case string:
		return v != ""
	case []any:
		return len(v) > 0
	case []string:
		return len(v) > 0
	case *Object:
		return v.Len() > 0
	}
	panic(fmt.Sprintf("json.Truthy: unsupported type %T", v))
}

// kind groups the supported Go types by the Python type family that == compares within.
type kind int

const (
	kindNone kind = iota
	kindNumber
	kindString
	kindList
	kindDict
)

func kindOf(v any) kind {
	switch v.(type) {
	case nil:
		return kindNone
	case bool, Int, int, int64, float64:
		return kindNumber
	case string:
		return kindString
	case []any, []string:
		return kindList
	case *Object:
		return kindDict
	}
	panic(fmt.Sprintf("json: unsupported type %T", v))
}

// PyObject_RichCompare checks its stack before each comparison, and fails when the enclosing container
// comparisons already use more than compareStack bytes. Measured with CPython 3.15 on darwin (16 MB main
// stack, `python3 script.py`): two equal 47527-deep nested lists or 45461-deep nested dicts compare and one
// more level raises, so a list level takes 352 bytes and a dict level 368. Alternating list/dict nesting
// (46471 levels) and one dict per three levels (46818) fit the same sums; together the four measurements
// put compareStack in [16729280, 16729504). As with maxDepth, argv and the environment share that stack
// and move the limit by a few levels per kilobyte.
//
// py: Objects/object.c PyObject_RichCompare _Py_EnterRecursiveCall (Python 3.15)
const (
	compareStack     = 16_729_400
	compareListFrame = 352
	compareDictFrame = 368
)

// Equal returns Python a == b: True == 1 == 1.0, NaN != NaN, lists compare item by item, and dicts compare
// a's keys in insertion order with b's values. It returns the *RecursionError that CPython raises when the
// comparison nests too deep. Python's identity shortcut inside containers is not modelled, so a list
// holding NaN never equals itself.
//
// To compare a value with a str, v == s on the interface is exact and cannot fail.
//
// py: Objects/object.c PyObject_RichCompare (Python 3.15)
func Equal(a, b any) (bool, error) {
	return equal(a, b, 0)
}

// equal is Equal with stack bytes in use by the enclosing comparisons.
//
// py: Objects/listobject.c list_richcompare, Objects/dictobject.c dict_equal (Python 3.15)
func equal(a, b any, stack int) (bool, error) {
	kindA, kindB := kindOf(a), kindOf(b)
	// ponytail: only pairs with a container take the stack check. CPython checks every pair that is not
	// one object (small ints and one-letter strings are), so a scalar leaf can fail one level sooner.
	if (kindA >= kindList || kindB >= kindList) && stack > compareStack {
		return false, stackOverflow(" in comparison")
	}
	if kindA != kindB {
		return false, nil
	}
	switch kindA {
	case kindNone:
		return true, nil
	case kindNumber:
		return numbersEqual(a, b), nil
	case kindString:
		return a.(string) == b.(string), nil
	case kindList:
		listA, listB := asList(a), asList(b)
		if len(listA) != len(listB) {
			return false, nil
		}
		for i := range listA {
			if same, err := equal(listA[i], listB[i], stack+compareListFrame); err != nil || !same {
				return false, err
			}
		}
		return true, nil
	}
	dictA, dictB := a.(*Object), b.(*Object)
	if dictA.Len() != dictB.Len() {
		return false, nil
	}
	for _, key := range dictA.keys {
		valueB, ok := dictB.values[key]
		if !ok {
			return false, nil
		}
		if same, err := equal(dictA.values[key], valueB, stack+compareDictFrame); err != nil || !same {
			return false, err
		}
	}
	return true, nil
}

// numbersEqual compares two Python numbers exactly, so an int and a float are equal only when the float
// holds exactly that integer.
func numbersEqual(a, b any) bool {
	if isNaN(a) || isNaN(b) {
		return false
	}
	return exactNumber(a).Cmp(exactNumber(b)) == 0
}

func isNaN(v any) bool {
	f, ok := v.(float64)
	return ok && math.IsNaN(f)
}

// exactNumber returns a non-NaN number as a big.Float that holds it without rounding.
func exactNumber(v any) *big.Float {
	switch v := v.(type) {
	case bool:
		if v {
			return big.NewFloat(1)
		}
		return big.NewFloat(0)
	case int:
		return new(big.Float).SetInt64(int64(v))
	case int64:
		return new(big.Float).SetInt64(v)
	case float64:
		return big.NewFloat(v)
	}
	n, ok := new(big.Int).SetString(string(v.(Int)), 10)
	if !ok {
		panic(fmt.Sprintf("json: Int %q is not decimal text", v))
	}
	return new(big.Float).SetInt(n) // precision 0 becomes the bit length, so this is exact
}

func asList(v any) []any {
	if items, ok := v.([]string); ok {
		list := make([]any, len(items))
		for i, item := range items {
			list[i] = item
		}
		return list
	}
	return v.([]any)
}

// TypeName returns type(v).__name__. It panics on a Go type outside the value model.
func TypeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case Int, int, int64:
		return "int"
	case float64:
		return "float"
	case string:
		return "str"
	case []any, []string:
		return "list"
	case *Object:
		return "dict"
	}
	panic(fmt.Sprintf("json.TypeName: unsupported type %T", v))
}

// Repr returns repr(v) for a value of the value model. It panics on a Go type outside it.
//
// py: Objects/object.c PyObject_Repr (Python 3.15)
func Repr(v any) string {
	switch v := v.(type) {
	case nil:
		return "None"
	case bool:
		if v {
			return "True"
		}
		return "False"
	case float64:
		switch text, _ := Dumps(v, Options{}); text { // json.dumps writes float.__repr__
		case "NaN":
			return "nan"
		case "Infinity":
			return "inf"
		case "-Infinity":
			return "-inf"
		default:
			return text
		}
	case string:
		return str.Repr(v)
	case []any:
		items := make([]string, len(v))
		for i, item := range v {
			items[i] = Repr(item)
		}
		return "[" + strings.Join(items, ", ") + "]"
	case *Object:
		items := make([]string, 0, v.Len())
		for _, key := range v.Keys() {
			items = append(items, str.Repr(key)+": "+Repr(v.Value(key)))
		}
		return "{" + strings.Join(items, ", ") + "}"
	}
	return string(v.(Int))
}
