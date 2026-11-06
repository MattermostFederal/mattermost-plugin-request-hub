package main

import (
	"bytes"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// pdfDoc writes a plain text PDF: US Letter pages, the standard Helvetica
// fonts in WinAnsi encoding, wrapped lines and page numbers. It covers what
// the bot inventory needs and nothing more, so the plugin needs no PDF
// library. Characters WinAnsi lacks print as "?".
type pdfDoc struct {
	title string
	now   time.Time
	pages []*bytes.Buffer
	cur   *bytes.Buffer
	y     float64
}

const (
	pdfPageWidth  = 612.0
	pdfPageHeight = 792.0
	pdfMargin     = 54.0
	pdfTextWidth  = pdfPageWidth - 2*pdfMargin
	// pdfBottom leaves room for the page number under the text.
	pdfBottom = pdfMargin + 18
	// pdfLeading is the line height as a multiple of the font size.
	pdfLeading = 1.3
	// pdfBoldWidth widens Helvetica's widths to measure Helvetica-Bold,
	// which runs about this much wider, so bold lines never overflow.
	pdfBoldWidth = 1.1
)

func newPDF(title string, now time.Time) *pdfDoc {
	d := &pdfDoc{title: title, now: now}
	d.newPage()
	return d
}

func (d *pdfDoc) newPage() {
	d.cur = &bytes.Buffer{}
	d.pages = append(d.pages, d.cur)
	d.y = pdfPageHeight - pdfMargin
}

// ensure starts a new page unless height points still fit on this one.
func (d *pdfDoc) ensure(height float64) {
	if d.y-height < pdfBottom {
		d.newPage()
	}
}

// space moves down by height points.
func (d *pdfDoc) space(height float64) {
	d.y -= height
}

// text writes s wrapped to the text width, each paragraph of s on its own
// line.
func (d *pdfDoc) text(s string, size float64, bold bool) {
	for _, line := range wrapPDF(pdfEncode(s), size, bold, pdfTextWidth, pdfTextWidth) {
		d.line(pdfMargin, line, size, bold)
	}
}

// field writes "label: value", the label in bold, with the value wrapped
// under the label's indent.
func (d *pdfDoc) field(label, value string, size float64) {
	lbl := pdfEncode(label + ": ")
	indent := pdfWidth(lbl, size, true)
	lines := wrapPDF(pdfEncode(value), size, false, pdfTextWidth-indent, pdfTextWidth-indent)
	if len(lines) == 0 {
		lines = [][]byte{nil}
	}
	for i, line := range lines {
		d.ensure(size * pdfLeading)
		d.y -= size * pdfLeading
		if i == 0 {
			d.draw(pdfMargin, d.y, lbl, size, true)
		}
		d.draw(pdfMargin+indent, d.y, line, size, false)
	}
}

// rule draws a thin gray line across the text width.
func (d *pdfDoc) rule() {
	d.ensure(8)
	d.y -= 6
	fmt.Fprintf(d.cur, "0.75 G 0.5 w %.2f %.2f m %.2f %.2f l S 0 G\n", pdfMargin, d.y, pdfPageWidth-pdfMargin, d.y)
	d.y -= 2
}

func (d *pdfDoc) line(x float64, encoded []byte, size float64, bold bool) {
	d.ensure(size * pdfLeading)
	d.y -= size * pdfLeading
	d.draw(x, d.y, encoded, size, bold)
}

func (d *pdfDoc) draw(x, y float64, encoded []byte, size float64, bold bool) {
	if len(encoded) == 0 {
		return
	}
	font := "F1"
	if bold {
		font = "F2"
	}
	fmt.Fprintf(d.cur, "BT /%s %.1f Tf %.2f %.2f Td (%s) Tj ET\n", font, size, x, y, pdfEscape(encoded))
}

// bytes renders the document, numbering each page at its foot.
func (d *pdfDoc) bytes() []byte {
	var out bytes.Buffer
	var offsets []int
	obj := func(body string) {
		offsets = append(offsets, out.Len())
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", len(offsets), body)
	}
	out.WriteString("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")

	// Objects: 1 catalog, 2 page tree, 3 and 4 fonts, 5 info, then a page
	// and its content stream for each page.
	n := len(d.pages)
	kids := make([]string, n)
	for i := range d.pages {
		kids[i] = fmt.Sprintf("%d 0 R", 6+2*i)
	}
	obj("<< /Type /Catalog /Pages 2 0 R >>")
	obj(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), n))
	obj("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>")
	obj("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica-Bold /Encoding /WinAnsiEncoding >>")
	obj(fmt.Sprintf("<< /Title (%s) /Producer (Mattermost Request Hub) /CreationDate (D:%s) >>",
		pdfEscape(pdfEncode(d.title)), d.now.UTC().Format("20060102150405")+"Z"))
	for i, page := range d.pages {
		var content bytes.Buffer
		content.Write(page.Bytes())
		footer := pdfEncode(fmt.Sprintf("Page %d of %d", i+1, n))
		fmt.Fprintf(&content, "0.4 g BT /F1 8.0 Tf %.2f %.2f Td (%s) Tj ET 0 g\n",
			pdfPageWidth-pdfMargin-pdfWidth(footer, 8, false), pdfMargin, pdfEscape(footer))
		obj(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %.0f %.0f] /Resources << /Font << /F1 3 0 R /F2 4 0 R >> >> /Contents %d 0 R >>",
			pdfPageWidth, pdfPageHeight, 7+2*i))
		obj(fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", content.Len(), content.String()))
	}

	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1)
	for _, off := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R /Info 5 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xref)
	return out.Bytes()
}

// winAnsiExtra maps the characters WinAnsi places in 0x80-0x9F. Latin-1
// characters (0xA0-0xFF) keep their code.
var winAnsiExtra = map[rune]byte{
	'€': 0x80, '‚': 0x82, 'ƒ': 0x83, '„': 0x84, '…': 0x85, '†': 0x86, '‡': 0x87, 'ˆ': 0x88,
	'‰': 0x89, 'Š': 0x8A, '‹': 0x8B, 'Œ': 0x8C, 'Ž': 0x8E, '‘': 0x91, '’': 0x92, '“': 0x93,
	'”': 0x94, '•': 0x95, '–': 0x96, '—': 0x97, '˜': 0x98, '™': 0x99, 'š': 0x9A, '›': 0x9B,
	'œ': 0x9C, 'ž': 0x9E, 'Ÿ': 0x9F,
}

// pdfEncode turns s into WinAnsi bytes: whitespace and control characters
// become spaces, and characters WinAnsi lacks become "?".
func pdfEncode(s string) []byte {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		switch {
		case r == '\n':
			out = append(out, '\n')
		case unicode.IsSpace(r) || unicode.IsControl(r):
			out = append(out, ' ')
		case r < 0x80 || (r >= 0xA0 && r <= 0xFF):
			out = append(out, byte(r)) //nolint:gosec // r is below 0x100
		default:
			if b, ok := winAnsiExtra[r]; ok {
				out = append(out, b)
			} else {
				out = append(out, '?')
			}
		}
	}
	return out
}

// pdfEscape makes WinAnsi bytes safe inside a PDF string: backslash and
// parentheses are escaped, and bytes outside printable ASCII are written
// in octal.
func pdfEscape(b []byte) string {
	var s strings.Builder
	for _, c := range b {
		switch {
		case c == '\\' || c == '(' || c == ')':
			s.WriteByte('\\')
			s.WriteByte(c)
		case c < 0x20 || c > 0x7E:
			fmt.Fprintf(&s, "\\%03o", c)
		default:
			s.WriteByte(c)
		}
	}
	return s.String()
}

// helveticaWidths are Helvetica's glyph widths, in thousandths of the font
// size, for WinAnsi codes 32 to 126. Other characters measure as 556.
var helveticaWidths = [95]int{
	278, 278, 355, 556, 556, 889, 667, 191, 333, 333, 389, 584, 278, 333, 278, 278, // space to /
	556, 556, 556, 556, 556, 556, 556, 556, 556, 556, // 0 to 9
	278, 278, 584, 584, 584, 556, 1015, // : to @
	667, 667, 722, 722, 667, 611, 778, 722, 278, 500, 667, 556, 833, // A to M
	722, 778, 667, 778, 722, 667, 611, 722, 667, 944, 667, 667, 611, // N to Z
	278, 278, 278, 469, 556, 333, // [ to `
	556, 556, 500, 556, 556, 278, 556, 556, 222, 222, 500, 222, 833, // a to m
	556, 556, 556, 556, 333, 500, 278, 556, 500, 722, 500, 500, 500, // n to z
	334, 260, 334, 584, // { to ~
}

func pdfCharWidth(c byte) int {
	if c >= 32 && c <= 126 {
		return helveticaWidths[c-32]
	}
	return 556
}

// pdfWidth is the width of encoded text, in points.
func pdfWidth(b []byte, size float64, bold bool) float64 {
	total := 0
	for _, c := range b {
		total += pdfCharWidth(c)
	}
	w := float64(total) * size / 1000
	if bold {
		w *= pdfBoldWidth
	}
	return w
}

// wrapPDF breaks encoded text into lines: at newlines, then at spaces so
// each line fits its width (first for the first line, rest after), and
// mid-word for a word longer than a whole line.
func wrapPDF(b []byte, size float64, bold bool, first, rest float64) [][]byte {
	var lines [][]byte
	limit := first
	emit := func(line []byte) {
		lines = append(lines, line)
		limit = rest
	}
	for para := range bytes.SplitSeq(b, []byte("\n")) {
		var line []byte
		for word := range bytes.FieldsSeq(para) {
			candidate := word
			if len(line) > 0 {
				candidate = append(append(append([]byte{}, line...), ' '), word...)
			}
			if pdfWidth(candidate, size, bold) <= limit {
				line = candidate
				continue
			}
			if len(line) > 0 {
				emit(line)
			}
			for len(word) > 1 && pdfWidth(word, size, bold) > limit {
				cut := 1
				for cut < len(word) && pdfWidth(word[:cut+1], size, bold) <= limit {
					cut++
				}
				emit(word[:cut])
				word = word[cut:]
			}
			line = word
		}
		emit(line)
	}
	return lines
}
