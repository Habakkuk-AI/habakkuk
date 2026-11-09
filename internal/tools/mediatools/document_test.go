package mediatools

import (
	"archive/zip"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// minimalPDF builds a valid PDF with one page per text, with a correct
// cross-reference table.
func minimalPDF(pages ...string) []byte {
	var objs []string
	kids := ""
	for i := range pages {
		kids += fmt.Sprintf("%d 0 R ", 4+2*i)
	}
	objs = append(objs, "<< /Type /Catalog /Pages 2 0 R >>",
		fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", kids, len(pages)),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>")
	for i, text := range pages {
		stream := fmt.Sprintf("BT /F1 24 Tf 72 700 Td (%s) Tj ET", text)
		objs = append(objs,
			fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents %d 0 R /Resources << /Font << /F1 3 0 R >> >> >>", 5+2*i),
			fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream))
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs))
	for i, o := range objs {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return b.Bytes()
}

func zipOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, body := range files {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		f.Write([]byte(body))
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

const wns = `xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"`

func TestDocumentText(t *testing.T) {
	docx := zipOf(t, map[string]string{
		"[Content_Types].xml": "<Types/>",
		"word/document.xml": `<?xml version="1.0"?><w:document ` + wns + `><w:body>
			<w:p><w:r><w:t>Quarterly</w:t></w:r><w:r><w:t xml:space="preserve"> report</w:t></w:r></w:p>
			<w:p><w:r><w:t>Revenue</w:t><w:tab/><w:t>up 4%</w:t></w:r></w:p>
			<w:tbl><w:tr><w:tc><w:p><w:r><w:t>cell</w:t></w:r></w:p></w:tc></w:tr></w:tbl>
		</w:body></w:document>`,
		"word/footnotes.xml": `<w:footnotes ` + wns + `><w:footnote><w:p><w:r><w:t>Source: ledger</w:t></w:r></w:p></w:footnote></w:footnotes>`,
	})
	xlsx := zipOf(t, map[string]string{
		"xl/workbook.xml": `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">
			<sheets><sheet name="Sales" sheetId="1" r:id="rId1"/><sheet name="Notes" sheetId="2" r:id="rId2"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
			<Relationship Id="rId1" Target="worksheets/sheet1.xml"/><Relationship Id="rId2" Target="/xl/worksheets/other.xml"/></Relationships>`,
		"xl/sharedStrings.xml": `<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><si><t>Region</t></si><si><r><t>To</t></r><r><t>tal</t></r></si><si><t>North</t></si></sst>`,
		"xl/worksheets/sheet1.xml": `<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>
			<row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" t="s"><v>1</v></c></row>
			<row r="2"><c r="A2" t="s"><v>2</v></c><c r="C2"><v>42.5</v></c></row>
			<row r="3"><c r="B3" t="inlineStr"><is><t>inline</t></is></c><c r="C3" t="b"><v>1</v></c></row>
		</sheetData></worksheet>`,
		"xl/worksheets/other.xml": `<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData><row><c t="inlineStr"><is><t>remember</t></is></c></row></sheetData></worksheet>`,
	})
	pptx := zipOf(t, map[string]string{
		"ppt/presentation.xml":               "<p:presentation/>",
		"ppt/slides/slide2.xml":              `<p:sld xmlns:a="a" xmlns:p="p"><a:p><a:r><a:t>Second slide</a:t></a:r></a:p></p:sld>`,
		"ppt/slides/slide10.xml":             `<p:sld xmlns:a="a" xmlns:p="p"><a:p><a:r><a:t>Tenth slide</a:t></a:r></a:p></p:sld>`,
		"ppt/slides/slide1.xml":              `<p:sld xmlns:a="a" xmlns:p="p"><a:p><a:r><a:t>Title</a:t></a:r></a:p><a:p><a:r><a:t>Subtitle</a:t></a:r></a:p></p:sld>`,
		"ppt/notesSlides/notesSlide1.xml":    `<p:notes xmlns:a="a" xmlns:p="p"><a:p><a:r><a:t>Speak slowly</a:t></a:r></a:p></p:notes>`,
		"ppt/slides/_rels/slide1.xml.rels":   "<Relationships/>",
		"ppt/slideLayouts/slideLayout1.xml":  `<a:t xmlns:a="a">layout text</a:t>`,
		"ppt/slideMasters/slideMaster1.xml":  "<x/>",
		"ppt/notesSlides/notesSlide2.xml":    `<p:notes xmlns:a="a" xmlns:p="p"></p:notes>`,
		"ppt/slides/slide3-backup/slide.xml": "<x/>",
	})
	html := []byte(`<!doctype html><html><head><title>Menu</title><style>body{color:red}</style><script>alert("x")</script></head>
		<body><h1>Today&#39;s menu</h1><p>Jollof &amp; plantain</p><table><tr><td>Small</td><td>$9</td></tr></table>
		<img src="a.png" alt="the dish"><noscript>enable js</noscript></body></html>`)
	cases := []struct {
		name, file string
		data       []byte
		kind       string
		want, not  []string
	}{
		{"pdf", "scan.pdf", minimalPDF("Hello PDF world", "Page two here"), "pdf",
			[]string{"Hello PDF world", "--- page 2 ---", "Page two here"}, nil},
		{"docx", "report.docx", docx, "docx",
			[]string{"Quarterly report\n", "Revenue\tup 4%", "cell", "--- footnotes ---", "Source: ledger"}, nil},
		{"xlsx", "book.xlsx", xlsx, "xlsx",
			[]string{"--- sheet Sales ---\nRegion\tTotal\nNorth\t\t42.5\n\tinline\tTRUE", "--- sheet Notes ---\nremember"}, nil},
		{"pptx", "deck.pptx", pptx, "pptx",
			[]string{"--- slide 1 ---\nTitle\nSubtitle", "[notes] Speak slowly", "--- slide 2 ---\nSecond slide", "--- slide 3 ---\nTenth slide"},
			[]string{"layout text"}},
		{"html", "page.html", html, "html",
			[]string{"Today's menu", "Jollof & plantain", "Small\t$9", "[image: the dish]", "Menu"},
			[]string{"color:red", "alert", "enable js"}},
	}
	for _, c := range cases {
		kind, text, err := DocumentText(c.file, c.data)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if kind != c.kind {
			t.Errorf("%s: kind %s", c.name, kind)
		}
		for _, w := range c.want {
			if !strings.Contains(text, w) {
				t.Errorf("%s: %q missing from:\n%s", c.name, w, text)
			}
		}
		for _, n := range c.not {
			if strings.Contains(text, n) {
				t.Errorf("%s: %q should not be in:\n%s", c.name, n, text)
			}
		}
	}
}

func TestDocumentErrors(t *testing.T) {
	for name, data := range map[string][]byte{
		"notes.txt":  []byte("plain"),
		"old.doc":    {0xd0, 0xcf, 0x11, 0xe0},
		"bundle.zip": zipOf(t, map[string]string{"a.txt": "x"}),
		"bad.pdf":    []byte("%PDF-1.4 garbage"),
	} {
		if _, _, err := DocumentText(name, data); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestReadDocumentTool(t *testing.T) {
	env := testEnv(t)
	os.WriteFile(filepath.Join(env.Workspace, "a.pdf"), minimalPDF("Invoice 1042"), 0o644)
	os.WriteFile(filepath.Join(env.Workspace, "empty.pdf"), minimalPDF(""), 0o644)
	res, _ := run(t, env, readDocumentTool{}, `{"path":"a.pdf"}`)
	if res.IsError || !strings.Contains(res.Text, "a.pdf (pdf):") || !strings.Contains(res.Text, "Invoice 1042") {
		t.Fatalf("result: %s", res.Text)
	}
	res, _ = run(t, env, readDocumentTool{}, `{"path":"empty.pdf"}`)
	if !res.IsError || !strings.Contains(res.Text, "no text layer") {
		t.Fatalf("empty: %s", res.Text)
	}
	res, _ = run(t, env, readDocumentTool{}, `{"path":"missing.docx"}`)
	if !res.IsError {
		t.Fatalf("missing: %s", res.Text)
	}
}
