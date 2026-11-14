// Package termsafe makes text that an agent, a tool or another machine
// chose safe to write to a terminal.
//
// A terminal acts on control characters instead of showing them: an
// escape sequence can erase the line a command was printed on and draw a
// different one, hide text, or write the clipboard (OSC 52); bidi
// overrides reorder what is displayed. A command shown for approval must
// be the command that runs, so these characters are replaced by visible
// stand-ins, never passed through and never silently dropped: ESC becomes
// ␛, a C1 control becomes <U+009B>.
package termsafe

import (
	"bytes"
	"io"
	"strings"
	"unicode/utf8"
)

// Line returns s as one line that is safe to print: runs of white space
// (newlines and tabs included) become one space, the rest goes through
// Escape.
func Line(s string) string {
	return Escape(strings.Join(strings.Fields(s), " "))
}

// Text is Escape for text that keeps its lines: \n and \t stay and \r\n
// becomes \n. A lone \r is escaped, since it lets later text overwrite
// the start of the line.
func Text(s string) string {
	if clean(s, true) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 16)
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		i += n
		switch {
		case r == '\n' || r == '\t':
			b.WriteByte(byte(r))
		case r == '\r' && i < len(s) && s[i] == '\n':
			// dropped: the \n that follows ends the line
		default:
			writeRune(&b, r)
		}
	}
	return b.String()
}

// Escape replaces every character a terminal acts on rather than shows
// with a visible stand-in: C0 controls (newline and tab included) and
// DEL by their control pictures (␛ ␊ ␡), C1 controls, bidi embeddings,
// overrides and isolates, and the line and paragraph separators by
// <U+XXXX>. Bytes that aren't UTF-8 become U+FFFD. Escaping twice
// changes nothing.
func Escape(s string) string {
	if clean(s, false) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 16)
	for _, r := range s {
		writeRune(&b, r)
	}
	return b.String()
}

// Unsafe reports whether a terminal would act on r instead of showing it.
func Unsafe(r rune) bool {
	switch {
	case r < 0x20, r == 0x7f: // C0, DEL
		return true
	case r >= 0x80 && r <= 0x9f: // C1: U+009B is CSI on terminals that honour it
		return true
	case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069: // bidi embeddings, overrides, isolates
		return true
	case r == 0x2028, r == 0x2029: // line and paragraph separator
		return true
	}
	return false
}

// clean reports whether s needs no change (lines: \n and \t are fine,
// and a \r still needs a look).
func clean(s string, lines bool) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= utf8.RuneSelf {
			// Slow path: decode from here.
			for _, r := range s[i:] {
				if Unsafe(r) && !(lines && (r == '\n' || r == '\t')) || r == utf8.RuneError {
					return false
				}
			}
			return true
		}
		if (c < 0x20 || c == 0x7f) && !(lines && (c == '\n' || c == '\t')) {
			return false
		}
	}
	return true
}

const hexDigits = "0123456789ABCDEF"

func writeRune(b *strings.Builder, r rune) {
	switch {
	case r < 0x20:
		b.WriteRune(0x2400 + r) // control pictures: ␀ … ␟
	case r == 0x7f:
		b.WriteRune(0x2421) // ␡
	case Unsafe(r):
		b.WriteString("<U+")
		for shift := 12; shift >= 0; shift -= 4 {
			b.WriteByte(hexDigits[(r>>shift)&0xf])
		}
		b.WriteByte('>')
	default:
		b.WriteRune(r) // an invalid byte arrives as U+FFFD
	}
}

// JSON returns encoded JSON with DEL, C1 controls and bidi controls
// written as \uXXXX escapes. encoding/json already escapes C0 controls
// but passes these through; in JSON they can only occur inside strings,
// so the result decodes to exactly the same value.
func JSON(b []byte) []byte {
	need := false
	for i := 0; i < len(b) && !need; i++ {
		need = b[i] >= 0x7f
	}
	if !need {
		return b
	}
	var out bytes.Buffer
	out.Grow(len(b) + 16)
	for i := 0; i < len(b); {
		r, n := utf8.DecodeRune(b[i:])
		if Unsafe(r) && r >= 0x7f {
			out.WriteString(`\u`)
			for shift := 12; shift >= 0; shift -= 4 {
				out.WriteByte(hexDigits[(r>>shift)&0xf] | 0x20) // lower case, like encoding/json
			}
		} else {
			out.Write(b[i : i+n])
		}
		i += n
	}
	return out.Bytes()
}

// Writer passes what is written through Text, for output that is
// streamed (a diff of files an agent wrote). It holds back a \r or a
// character split across two writes until the next one; call Flush at
// the end.
type Writer struct {
	w    io.Writer
	held []byte
}

// NewWriter returns a Writer on w.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

func (t *Writer) Write(p []byte) (int, error) {
	buf := p
	if len(t.held) > 0 {
		buf = append(t.held, p...)
		t.held = nil
	}
	// Hold back what the next write may complete: a trailing \r (is a \n
	// next?) or the first bytes of a multi-byte character.
	end := len(buf)
	if end > 0 && buf[end-1] == '\r' {
		end--
	} else {
		for i := end - 1; i >= 0 && i >= end-utf8.UTFMax; i-- {
			if utf8.RuneStart(buf[i]) {
				if !utf8.FullRune(buf[i:end]) {
					end = i
				}
				break
			}
		}
	}
	t.held = append(t.held, buf[end:]...)
	if end > 0 {
		if _, err := io.WriteString(t.w, Text(string(buf[:end]))); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// Flush writes what was held back.
func (t *Writer) Flush() error {
	if len(t.held) == 0 {
		return nil
	}
	s := Text(string(t.held))
	t.held = nil
	_, err := io.WriteString(t.w, s)
	return err
}
