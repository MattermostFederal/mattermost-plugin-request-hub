package main

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// checkPDF checks a PDF's frame: the header, an xref whose offsets each
// point at their object, and a trailer whose startxref points at the xref.
// It returns the number of pages.
func checkPDF(t *testing.T, data []byte) int {
	t.Helper()
	require.True(t, bytes.HasPrefix(data, []byte("%PDF-1.4\n")))
	require.True(t, bytes.HasSuffix(data, []byte("%%EOF\n")))

	m := regexp.MustCompile(`startxref\n(\d+)\n%%EOF\n$`).FindSubmatch(data)
	require.NotNil(t, m)
	xref, err := strconv.Atoi(string(m[1]))
	require.NoError(t, err)
	require.True(t, bytes.HasPrefix(data[xref:], []byte("xref\n0 ")))

	lines := strings.Split(string(data[xref:]), "\n")
	count, err := strconv.Atoi(strings.Fields(lines[1])[1])
	require.NoError(t, err)
	require.Equal(t, "0000000000 65535 f ", lines[2])
	for i := 1; i < count; i++ {
		off, err := strconv.Atoi(lines[2+i][:10])
		require.NoError(t, err)
		require.True(t, bytes.HasPrefix(data[off:], []byte(strconv.Itoa(i)+" 0 obj\n")), "object %d is at its xref offset", i)
	}
	require.Contains(t, string(data), "/Size "+strconv.Itoa(count))

	// Each stream's /Length is its byte count.
	for _, sm := range regexp.MustCompile(`(?s)/Length (\d+) >>\nstream\n(.*?)endstream`).FindAllSubmatch(data, -1) {
		n, _ := strconv.Atoi(string(sm[1]))
		require.Len(t, sm[2], n)
	}
	pages := regexp.MustCompile(`/Count (\d+)`).FindSubmatch(data)
	require.NotNil(t, pages)
	n, _ := strconv.Atoi(string(pages[1]))
	return n
}

func TestPDF_Frame(t *testing.T) {
	doc := newPDF("Title (with) parens", time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC))
	doc.text("Hello", 12, true)
	doc.field("Label", "value", 9)
	doc.rule()

	data := doc.bytes()

	require.Equal(t, 1, checkPDF(t, data))
	s := string(data)
	require.Contains(t, s, `/Title (Title \(with\) parens)`)
	require.Contains(t, s, "/CreationDate (D:20261105120000Z)")
	require.Contains(t, s, "/BaseFont /Helvetica-Bold /Encoding /WinAnsiEncoding")
	require.Contains(t, s, "/F2 12.0 Tf")
	require.Contains(t, s, "(Hello) Tj")
	require.Contains(t, s, "(Label: ) Tj")
	require.Contains(t, s, "(value) Tj")
	require.Contains(t, s, "(Page 1 of 1) Tj")
}

func TestPDFEncodeAndEscape(t *testing.T) {
	require.Equal(t, []byte("a b c"), pdfEncode("a\tb\rc"))
	require.Equal(t, []byte("x\ny"), pdfEncode("x\ny"))
	require.Equal(t, []byte{'c', 'a', 'f', 0xE9}, pdfEncode("café"))
	require.Equal(t, []byte{0x93, 'q', 0x94, ' ', 0x96, ' ', 0x80}, pdfEncode("“q” – €"))
	require.Equal(t, []byte("??"), pdfEncode("漢😀"))

	require.Equal(t, `a\\b \(c\) \351`, pdfEscape([]byte{'a', '\\', 'b', ' ', '(', 'c', ')', ' ', 0xE9}))
	require.Equal(t, `\012`, pdfEscape([]byte{'\n'}))
}

func TestPDFWidth(t *testing.T) {
	require.InDelta(t, 2*0.278+0.944, pdfWidth([]byte("  W"), 1, false), 1e-9)
	require.InDelta(t, 10*0.556, pdfWidth([]byte{0xE9}, 10, false), 1e-9)
	require.Greater(t, pdfWidth([]byte("abc"), 10, true), pdfWidth([]byte("abc"), 10, false))
}

func TestWrapPDF(t *testing.T) {
	width := pdfWidth([]byte("aaaa aaaa"), 10, false)

	t.Run("breaks at spaces to fit", func(t *testing.T) {
		lines := wrapPDF([]byte("aaaa aaaa aaaa"), 10, false, width, width)
		require.Equal(t, [][]byte{[]byte("aaaa aaaa"), []byte("aaaa")}, lines)
	})
	t.Run("the first line can be narrower", func(t *testing.T) {
		lines := wrapPDF([]byte("aaaa aaaa aaaa"), 10, false, pdfWidth([]byte("aaaa"), 10, false), width)
		require.Equal(t, [][]byte{[]byte("aaaa"), []byte("aaaa aaaa")}, lines)
	})
	t.Run("keeps paragraphs and blank lines", func(t *testing.T) {
		lines := wrapPDF([]byte("a\n\nb"), 10, false, width, width)
		require.Equal(t, [][]byte{[]byte("a"), nil, []byte("b")}, lines)
	})
	t.Run("cuts a word longer than a line", func(t *testing.T) {
		lines := wrapPDF([]byte(strings.Repeat("a", 20)), 10, false, width, width)
		require.Greater(t, len(lines), 1)
		var joined []byte
		for _, line := range lines {
			require.LessOrEqual(t, pdfWidth(line, 10, false), width)
			joined = append(joined, line...)
		}
		require.Equal(t, strings.Repeat("a", 20), string(joined))
	})
	t.Run("a line narrower than one character still moves on", func(t *testing.T) {
		lines := wrapPDF([]byte("ab"), 10, false, 1, 1)
		require.Equal(t, [][]byte{[]byte("a"), []byte("b")}, lines)
	})
}

func TestPDF_Paginates(t *testing.T) {
	doc := newPDF("Long", time.Now())
	for range 200 {
		doc.field("Line", strings.Repeat("word ", 30), 9)
	}

	data := doc.bytes()

	pages := checkPDF(t, data)
	require.Greater(t, pages, 3)
	require.Contains(t, string(data), "(Page "+strconv.Itoa(pages)+" of "+strconv.Itoa(pages)+") Tj")
	for _, m := range regexp.MustCompile(`([\d.]+) ([\d.]+) Td`).FindAllStringSubmatch(string(data), -1) {
		y, _ := strconv.ParseFloat(m[2], 64)
		require.GreaterOrEqual(t, y, pdfMargin, "nothing is drawn below the bottom margin")
		require.LessOrEqual(t, y, pdfPageHeight-pdfMargin)
	}
}
