package hfs

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// nilReceiverExempt lists methods allowed to return a nil error from a nil
// receiver, with the reason.
//
// Both are cases where nothing is the correct answer rather than a hidden
// failure: closing nothing succeeds, and a nil error wraps nothing.
var nilReceiverExempt = map[string]string{
	"Close":  "closing nothing succeeds",
	"Unwrap": "a nil error wraps nothing",
}

// Every exported method must survive a nil receiver, and every one that can
// report an error must report one.
//
// Open returns a nil *Volume alongside its error, and the Open*By* methods
// return a nil *File alongside theirs. A caller who checks the error never sees
// either. The failure this guards against is the caller who does not — an
// acquisition tool logging the volume kind before testing err should put a
// useless line in its report, not take the process down with an image half
// read. Roughly half this package already worked that way and half did not,
// which is the worst of the two: code that panics consistently at least teaches
// the caller to check.
//
// The second assertion matters as much as the first. A guard that returns zero
// values and a nil error turns a crash into a silent wrong answer — a walk that
// visits nothing, a report naming no files, an empty range list — and on a
// forensic tool that is worse than the panic it replaced.
//
// Reflection rather than a hand-written list, because the list is what rots: a
// method added next year is covered the day it is added, without anyone
// remembering this file exists.
func TestNilReceiverNeverPanics(t *testing.T) {
	targets := []struct {
		name string
		val  reflect.Value
	}{
		{"*Volume", reflect.ValueOf((*Volume)(nil))},
		{"*File", reflect.ValueOf((*File)(nil))},
		{"*ParseError", reflect.ValueOf((*ParseError)(nil))},
	}

	errType := reflect.TypeFor[error]()

	for _, target := range targets {
		t.Run(strings.TrimPrefix(target.name, "*"), func(t *testing.T) {
			rt := target.val.Type()
			var checked, silent []string

			for i := range rt.NumMethod() {
				m := rt.Method(i)
				args := make([]reflect.Value, 0, m.Type.NumIn()-1)
				for j := 1; j < m.Type.NumIn(); j++ {
					args = append(args, nilReceiverArg(m.Type.In(j)))
				}

				out, panicked := callRecovering(target.val.Method(i), args)
				if panicked != nil {
					t.Errorf("%s.%s panicked on a nil receiver: %v", target.name, m.Name, panicked)
					continue
				}
				checked = append(checked, m.Name)

				if _, exempt := nilReceiverExempt[m.Name]; exempt {
					continue
				}
				for k, res := range out {
					if m.Type.Out(k) == errType && res.IsNil() {
						silent = append(silent, m.Name)
					}
				}
			}

			if len(checked) == 0 {
				t.Fatalf("no methods found on %s; the test proved nothing", target.name)
			}
			sort.Strings(silent)
			for _, name := range silent {
				t.Errorf("%s.%s returned a nil error on a nil receiver, so the caller cannot tell it failed", target.name, name)
			}
			t.Logf("%s: %d exported methods survive a nil receiver", target.name, len(checked))
		})
	}
}

// nilReceiverArg builds a value for one parameter.
//
// Data parameters are zero, because the receiver is what is under test.
// Contexts and callbacks are not: a nil context or a nil callback is its own
// error condition, and a method that returns early because of one has not been
// asked the question this test means to ask. Passing a live callback is what
// separates "the walk refused because the volume is nil" from "the walk had
// nothing to call".
func nilReceiverArg(t reflect.Type) reflect.Value {
	if t == reflect.TypeFor[context.Context]() {
		return reflect.ValueOf(context.Background())
	}
	if t.Kind() == reflect.Slice {
		// A zero-length buffer is the same trap as a nil callback: Read and
		// ReadAt both return (0, nil) for an empty one before they look at
		// the receiver, so an empty slice would test nothing.
		return reflect.MakeSlice(t, 1, 1)
	}
	if t.Kind() == reflect.Func {
		return reflect.MakeFunc(t, func([]reflect.Value) []reflect.Value {
			out := make([]reflect.Value, t.NumOut())
			for i := range out {
				out[i] = reflect.Zero(t.Out(i))
			}
			return out
		})
	}
	return reflect.Zero(t)
}

func callRecovering(fn reflect.Value, args []reflect.Value) (out []reflect.Value, panicked error) {
	defer func() {
		if r := recover(); r != nil {
			panicked = fmt.Errorf("%v", r)
		}
	}()
	return fn.Call(args), nil
}
