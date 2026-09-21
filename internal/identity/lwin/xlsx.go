package lwin

import (
	"archive/zip"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// maxSheetBytes bounds the uncompressed first worksheet. The real export is
// ~264 MB uncompressed from a ~26 MB download; the cap stops a hostile or
// corrupt archive from decompressing without limit (a zip bomb) while leaving
// the genuine file several times its current size of headroom.
const maxSheetBytes = 2 << 30

// LoadXLSX reads the Liv-ex export in its published form, an .xlsx workbook,
// without converting it first: the first worksheet is streamed row by row, so
// memory is bounded by one row plus the records kept, not by the 264 MB sheet.
// Columns are recognized exactly as LoadCSV recognizes them, and loadRows
// applies the same normalization and filtering.
//
// Only what the Liv-ex export uses is supported: inline strings, shared
// strings, and numbers. Formulas, dates-as-dates and multiple sheets are not.
func LoadXLSX(r io.ReaderAt, size int64) (*DB, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, fmt.Errorf("lwin: not an xlsx archive: %w", err)
	}
	shared, err := readSharedStrings(zr)
	if err != nil {
		return nil, err
	}
	sheet := findFile(zr, "xl/worksheets/sheet1.xml")
	if sheet == nil {
		return nil, errors.New("lwin: xlsx has no xl/worksheets/sheet1.xml")
	}
	rc, err := sheet.Open()
	if err != nil {
		return nil, fmt.Errorf("lwin: opening worksheet: %w", err)
	}
	defer func() { _ = rc.Close() }()

	rows := &sheetRows{dec: xml.NewDecoder(io.LimitReader(rc, maxSheetBytes)), shared: shared}
	header, err := rows.next()
	if err != nil {
		if err == io.EOF {
			return nil, errors.New("lwin: worksheet is empty")
		}
		return nil, err
	}
	return loadRows(header, rows.next)
}

func findFile(zr *zip.Reader, name string) *zip.File {
	for _, f := range zr.File {
		if f.Name == name {
			return f
		}
	}
	return nil
}

// readSharedStrings loads the shared string table, if the workbook has one.
// The Liv-ex export stores its text inline and ships an almost empty table.
func readSharedStrings(zr *zip.Reader) ([]string, error) {
	f := findFile(zr, "xl/sharedStrings.xml")
	if f == nil {
		return nil, nil
	}
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("lwin: opening shared strings: %w", err)
	}
	defer func() { _ = rc.Close() }()
	dec := xml.NewDecoder(io.LimitReader(rc, maxSheetBytes))
	var out []string
	var cur strings.Builder
	inSI, inT := false, false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("lwin: shared strings: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "si":
				inSI = true
				cur.Reset()
			case "t":
				inT = inSI
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "si":
				out = append(out, cur.String())
				inSI = false
			case "t":
				inT = false
			}
		case xml.CharData:
			if inT {
				cur.Write(t)
			}
		}
	}
}

// sheetRows streams <row> elements from a worksheet as dense string slices.
type sheetRows struct {
	dec    *xml.Decoder
	shared []string
}

// next returns the next row, with each cell at its column index (gaps are
// ""), or io.EOF after the last row.
func (s *sheetRows) next() ([]string, error) {
	var row []string
	inRow := false
	var (
		col       = -1
		cellType  string
		inValue   bool
		value     strings.Builder
		nextIndex int
	)
	for {
		tok, err := s.dec.Token()
		if err == io.EOF {
			if inRow {
				return nil, errors.New("lwin: worksheet ends inside a row")
			}
			return nil, io.EOF
		}
		if err != nil {
			return nil, fmt.Errorf("lwin: worksheet: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "row":
				inRow, row, nextIndex = true, row[:0], 0
			case "c":
				col, cellType = nextIndex, ""
				for _, a := range t.Attr {
					switch a.Name.Local {
					case "r":
						if i, ok := columnIndex(a.Value); ok {
							col = i
						}
					case "t":
						cellType = a.Value
					}
				}
				value.Reset()
			case "v", "t":
				inValue = inRow && col >= 0
			}
		case xml.CharData:
			if inValue {
				value.Write(t)
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "v", "t":
				inValue = false
			case "c":
				v := value.String()
				if cellType == "s" {
					i, err := strconv.Atoi(strings.TrimSpace(v))
					if err != nil || i < 0 || i >= len(s.shared) {
						return nil, fmt.Errorf("lwin: bad shared string index %q", v)
					}
					v = s.shared[i]
				}
				for len(row) <= col {
					row = append(row, "")
				}
				row[col] = v
				nextIndex = col + 1
				col = -1
			case "row":
				return row, nil
			}
		}
	}
}

// columnIndex turns a cell reference ("C12") into a zero-based column (2).
func columnIndex(ref string) (int, bool) {
	n := 0
	i := 0
	for ; i < len(ref) && ref[i] >= 'A' && ref[i] <= 'Z'; i++ {
		n = n*26 + int(ref[i]-'A'+1)
		if n > 16384 { // Excel's column limit
			return 0, false
		}
	}
	if i == 0 {
		return 0, false
	}
	return n - 1, true
}
