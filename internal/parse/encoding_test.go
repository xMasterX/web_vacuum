package parse

import (
	"bytes"
	"math/rand"
	"strings"
	"testing"
)

// win1251 is a fragment of Cyrillic text in windows-1251, the encoding most of
// the old Russian-language web is written in. These bytes are not valid UTF-8.
var win1251 = []byte("\xd0\xf3\xf1\xf1\xea\xe8\xe9 \xf2\xe5\xea\xf1\xf2 \xe4\xeb\xff \xef\xf0\xee\xe2\xe5\xf0\xea\xe8")

// TestAsciiLowerPreservesLength is the property the parser depends on.
//
// bytes.ToLower decodes UTF-8 and turns every invalid byte into a three-byte
// replacement character. Any offset taken from such a copy is wrong against the
// original, and on a large page it lands past the end and panics.
func TestAsciiLowerPreservesLength(t *testing.T) {
	inputs := [][]byte{
		[]byte("<STYLE>x</STYLE>"),
		win1251,
		append([]byte("<SCRIPT>"), win1251...),
		{0x00, 0xff, 0xfe, 0x80, 0x41, 0x5a},
	}
	for _, in := range inputs {
		out := AsciiLower(in)
		if len(out) != len(in) {
			t.Errorf("AsciiLower changed length: %d -> %d for %q", len(in), len(out), in)
		}
		for i := range in {
			want := in[i]
			if want >= 'A' && want <= 'Z' {
				want += 'a' - 'A'
			}
			if out[i] != want {
				t.Errorf("byte %d: got %#x, want %#x", i, out[i], want)
			}
		}
	}

	// The standard library's version does not have this property, which is
	// exactly why this function exists.
	if len(bytes.ToLower(win1251)) == len(win1251) {
		t.Log("note: bytes.ToLower preserved length here; the guarantee still cannot be relied on")
	}
}

// TestScanningNonUTF8PageDoesNotPanic reproduces the crash: a long page in a
// legacy encoding, with enough non-ASCII bytes that offsets taken from a
// re-encoded copy run past the end of the original.
func TestScanningNonUTF8PageDoesNotPanic(t *testing.T) {
	var b bytes.Buffer
	b.WriteString(`<html><head><style>body{background:url(bg.png)}</style></head><body>`)
	// Enough Cyrillic that a three-times inflation would overshoot badly.
	for i := 0; i < 4000; i++ {
		b.Write(win1251)
		b.WriteString(`<a href="showthread.php?t=`)
		b.WriteString(strings.Repeat("1", 3))
		b.WriteString(`">`)
		b.Write(win1251)
		b.WriteString(`</a>`)
		if i%50 == 0 {
			b.WriteString(`<style>.x{background:url(x.gif)}</style>`)
			b.WriteString(`<script>var a=1;</script>`)
		}
	}
	b.WriteString(`</body></html>`)
	src := b.Bytes()

	opt := Options{ScanSrcset: true, ScanInlineCSS: true, FollowMeta: true, ScanComments: true}
	doc := HTML(src, opt)
	if len(doc.Links) == 0 {
		t.Fatal("no links found in a page that clearly has some")
	}
	// Every reported range has to be valid against the original bytes.
	for _, l := range doc.Links {
		if l.Start < 0 || l.End > len(src) || l.Start > l.End {
			t.Fatalf("link %q has an impossible range %d..%d in a %d byte document",
				l.Value, l.Start, l.End, len(src))
		}
	}
	// And the element scanners must agree with the document too.
	for _, r := range ElementRanges(src, "script") {
		if r.Start < 0 || r.End > len(src) || r.Start > r.End {
			t.Fatalf("element range %d..%d is outside a %d byte document", r.Start, r.End, len(src))
		}
	}
}

// TestParserSurvivesHostileBytes throws malformed and truncated documents at
// the scanner. None of them may panic, and every range reported must be usable.
func TestParserSurvivesHostileBytes(t *testing.T) {
	seeds := []string{
		`<style>`, `</style>`, `<style><style>`, `<script`, `<script>`,
		`<a href=`, `<a href="`, `<a href='`, `<img srcset="`,
		`<style>a{background:url(`, `<meta http-equiv=refresh content="0;url=`,
		`<!--`, `<!-- <a href="x"> `, `<base href=`,
		`<STYLE>` + string(win1251) + `</STYLE>`,
		string(win1251) + `<a href="` + string(win1251) + `">`,
	}
	opt := Options{ScanSrcset: true, ScanInlineCSS: true, FollowMeta: true, ScanComments: true, ScanScripts: true}

	for _, seed := range seeds {
		src := []byte(seed)
		doc := HTML(src, opt)
		for _, l := range doc.Links {
			if l.Start < 0 || l.End > len(src) || l.Start > l.End {
				t.Errorf("seed %q produced range %d..%d for a %d byte input", seed, l.Start, l.End, len(src))
			}
		}
		ElementRanges(src, "script")
		ElementRanges(src, "style")
		CSS(src, 0)
		// Applying whatever came back must also be safe.
		var reps []Replacement
		for _, l := range doc.Links {
			reps = append(reps, Replacement{Start: l.Start, End: l.End, Text: "X"})
		}
		Apply(src, reps)
	}

	// Random bytes, including plenty that are not valid UTF-8.
	rng := rand.New(rand.NewSource(1))
	fragments := []string{"<a href=", "<style>", "</style>", "<script>", "</script>",
		`"`, `'`, ">", "<", "url(", "&amp;", "\xf0\xf3", "\x00", "<!--", "-->"}
	for i := 0; i < 400; i++ {
		var b bytes.Buffer
		for j := 0; j < 60; j++ {
			b.WriteString(fragments[rng.Intn(len(fragments))])
		}
		src := b.Bytes()
		doc := HTML(src, opt)
		for _, l := range doc.Links {
			if l.Start < 0 || l.End > len(src) || l.Start > l.End {
				t.Fatalf("random input produced range %d..%d for a %d byte document\n%q",
					l.Start, l.End, len(src), src)
			}
		}
		ElementRanges(src, "script")
		CSS(src, 0)
	}
}

// TestAttributeValuesInLegacyEncodings covers the same defect in the two places
// it hid in attribute handling rather than document scanning: a meta refresh
// and a charset declaration whose value contains bytes that are not UTF-8.
func TestAttributeValuesInLegacyEncodings(t *testing.T) {
	cyr := string(win1251)

	// A refresh whose content carries non-ASCII before the URL.
	src := []byte(`<meta http-equiv="refresh" content="5; ` + cyr + ` URL=/next.html">`)
	doc := HTML(src, Options{FollowMeta: true})
	var found bool
	for _, l := range doc.Links {
		if l.Start < 0 || l.End > len(src) || l.Start > l.End {
			t.Fatalf("meta refresh produced range %d..%d in a %d byte document", l.Start, l.End, len(src))
		}
		if l.Value == "/next.html" {
			found = true
			if got := string(src[l.Start:l.End]); got != "/next.html" {
				t.Errorf("range covers %q, not the URL", got)
			}
		}
	}
	if !found {
		t.Errorf("the refresh target was not found: %+v", doc.Links)
	}

	// A charset declaration behind non-ASCII text.
	src2 := []byte(`<meta http-equiv="Content-Type" content="text/html; ` + cyr + ` charset=windows-1251">`)
	doc2 := HTML(src2, Options{})
	if doc2.Charset == "" {
		t.Error("charset was not read")
	}

	// A truncated directive must not slice past the end either.
	for _, bad := range []string{
		`<meta http-equiv="refresh" content="0;url=">`,
		`<meta http-equiv="refresh" content="url=">`,
		`<meta http-equiv="refresh" content="` + cyr + `url=">`,
		`<meta http-equiv="Content-Type" content="` + cyr + `charset=">`,
	} {
		HTML([]byte(bad), Options{FollowMeta: true})
	}
}
