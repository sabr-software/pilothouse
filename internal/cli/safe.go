package cli

import (
	"io"
	"os"
	"reflect"

	"golang.org/x/term"

	"github.com/sabr-software/pilothouse/internal/termsafe"
)

// What the daemon holds was written, somewhere along the way, by an agent,
// a tool, a paired device or another machine: commands, paths, titles,
// messages, device names, even ids and states. None of it may reach the
// terminal as a control character, or the command on screen is not the
// command that runs (see termsafe). The human-readable printers call
// forPrint on what they were given, or oneLineText on a single field;
// --json keeps the values and goes through jsonOut; Main escapes the
// error a command returns.

// forPrint rewrites every string v points at as one printable line, map
// keys included. It is for values that are only printed from here on: an
// id that goes back to the daemon must go back as it came. Raw JSON is
// left for whoever decodes it.
func forPrint(v any) { scrub(reflect.ValueOf(v)) }

func scrub(v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			scrub(v.Elem())
		}
	case reflect.Interface:
		if v.IsNil() || !v.CanSet() {
			return
		}
		c := reflect.New(v.Elem().Type()).Elem()
		c.Set(v.Elem())
		scrub(c)
		v.Set(c)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if f := v.Field(i); f.CanSet() { // not time.Time's or other private fields
				scrub(f)
			}
		}
	case reflect.Slice, reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return
		}
		for i := 0; i < v.Len(); i++ {
			scrub(v.Index(i))
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			nk := reflect.New(k.Type()).Elem()
			nk.Set(k)
			scrub(nk)
			nv := reflect.New(v.Type().Elem()).Elem()
			nv.Set(v.MapIndex(k))
			scrub(nv)
			v.SetMapIndex(k, reflect.Value{})
			v.SetMapIndex(nk, nv)
		}
	case reflect.String:
		if v.CanSet() {
			v.SetString(termsafe.Line(v.String()))
		}
	}
}

// errLine is err as one printable line, for commands that report a
// failure and carry on.
func errLine(err error) string { return termsafe.Line(err.Error()) }

// jsonOut is where --json output goes. The values stay as they are, for
// scripts; the characters a terminal acts on that encoding/json leaves
// alone (DEL, C1 controls, bidi overrides) are written as \u escapes,
// which parse back to the same strings.
func jsonOut(w io.Writer) io.Writer { return jsonWriter{w} }

type jsonWriter struct{ w io.Writer }

func (j jsonWriter) Write(p []byte) (int, error) {
	if _, err := j.w.Write(termsafe.JSON(p)); err != nil {
		return 0, err
	}
	return len(p), nil
}

// outIsTerminal reports whether w is a terminal; tests replace it.
var outIsTerminal = func(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// rawOut is for output that is data when piped and text when read: a
// patch must stay byte for byte the patch, but on a terminal the control
// characters in it are shown. Call done when everything is written.
func rawOut(w io.Writer) (out io.Writer, done func() error) {
	if !outIsTerminal(w) {
		return w, func() error { return nil }
	}
	sw := termsafe.NewWriter(w)
	return sw, sw.Flush
}
