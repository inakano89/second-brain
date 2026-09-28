package extract

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"strings"
)

// DOCXBytes extracts the paragraphs of a Word (.docx) document as plain text.
func DOCXBytes(data []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", err
	}
	for _, f := range zr.File {
		if f.Name != "word/document.xml" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return "", err
		}
		defer rc.Close()
		return docxText(io.LimitReader(rc, 64<<20))
	}
	return "", errors.New("docx: word/document.xml ausente")
}

func docxText(r io.Reader) (string, error) {
	dec := xml.NewDecoder(r)
	var b, para strings.Builder
	inText := false
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "t":
				inText = true
			case "tab":
				para.WriteByte('\t')
			case "br", "cr":
				para.WriteByte('\n')
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inText = false
			case "p":
				if s := strings.TrimSpace(para.String()); s != "" {
					b.WriteString(s + "\n\n")
				}
				para.Reset()
			}
		case xml.CharData:
			if inText {
				para.Write(t)
			}
		}
	}
	return strings.TrimSpace(b.String()), nil
}
