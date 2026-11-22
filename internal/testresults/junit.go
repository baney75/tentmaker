package testresults

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/baney75/tentmaker/internal/runs"
)

// maxText caps the text kept from one failure while parsing, before
// runs.TestResults.Capped cuts it to runs.MaxDetail.
const maxText = 2 * runs.MaxDetail

// ParseJUnit parses one JUnit XML report (<testsuites> or a lone <testsuite>,
// nested suites too). file is recorded on each case.
func ParseJUnit(r io.Reader, file string) ([]runs.TestCase, error) {
	d := xml.NewDecoder(r)
	d.Strict = true
	d.CharsetReader = charsetReader
	var (
		cases  []runs.TestCase
		suites []string
		seen   bool // the root element was read
	)
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			if !seen {
				return nil, errors.New("no testsuites or testsuite element")
			}
			return cases, nil
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if !seen {
				if t.Name.Local != "testsuites" && t.Name.Local != "testsuite" {
					return nil, fmt.Errorf("the root element is %s, not testsuites or testsuite", t.Name.Local)
				}
				seen = true
			}
			switch t.Name.Local {
			case "testsuite":
				suites = append(suites, attr(t, "name"))
			case "testcase":
				suite := ""
				if len(suites) > 0 {
					suite = suites[len(suites)-1]
				}
				c, err := parseCase(d, t, suite)
				if err != nil {
					return nil, err
				}
				if c.Name != "" {
					c.File = file
					cases = append(cases, c)
				}
			}
		case xml.EndElement:
			if t.Name.Local == "testsuite" && len(suites) > 0 {
				suites = suites[:len(suites)-1]
			}
		}
	}
}

// parseCase reads one <testcase> element whose start tag is start, through
// its end tag.
func parseCase(d *xml.Decoder, start xml.StartElement, suite string) (runs.TestCase, error) {
	c := runs.TestCase{
		Name:    strings.TrimSpace(attr(start, "name")),
		Suite:   strings.TrimSpace(attr(start, "classname")),
		Status:  runs.TestPassed,
		Seconds: seconds(attr(start, "time")),
	}
	if c.Suite == "" {
		c.Suite = suite
	}
	for {
		tok, err := d.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return c, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "failure", "error":
				text, err := elementText(d)
				if err != nil {
					return c, err
				}
				if c.Status != runs.TestFailed {
					c.Status = runs.TestFailed
					c.Message = oneLine(attr(t, "message"))
					if c.Message == "" {
						c.Message = firstLine(text)
					}
					c.Detail = strings.TrimSpace(text)
				}
			case "skipped":
				text, err := elementText(d)
				if err != nil {
					return c, err
				}
				if c.Status == runs.TestPassed {
					c.Status = runs.TestSkipped
					c.Message = oneLine(attr(t, "message"))
					if c.Message == "" {
						c.Message = firstLine(text)
					}
				}
			default:
				// flakyFailure, rerunFailure, system-out, properties and the
				// like say nothing about the final status.
				if err := d.Skip(); err != nil {
					return c, err
				}
			}
		case xml.EndElement:
			return c, nil
		}
	}
}

// elementText returns the text of the element just started, through its
// end tag, cut to maxText bytes.
func elementText(d *xml.Decoder) (string, error) {
	var b strings.Builder
	depth := 1
	for depth > 0 {
		tok, err := d.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return "", err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
		case xml.EndElement:
			depth--
		case xml.CharData:
			if room := maxText - b.Len(); room > 0 {
				if len(t) > room {
					t = t[:room]
				}
				b.Write(t)
			}
		}
	}
	return b.String(), nil
}

func attr(e xml.StartElement, name string) string {
	for _, a := range e.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

// seconds parses a time attribute such as "0.012" or "1,234.5"; anything
// else is zero.
func seconds(s string) float64 {
	f, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(s), ",", ""), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
		return 0
	}
	return f
}

// firstLine is the first line of s with text on it, trimmed.
func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

// oneLine is s with its lines joined by spaces, so a message attribute
// that holds a newline still reads as one line.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// charsetReader accepts the encodings a JUnit report plausibly declares
// that are UTF-8 or map byte for byte onto it, and refuses the rest.
func charsetReader(charset string, in io.Reader) (io.Reader, error) {
	switch strings.ToLower(strings.TrimSpace(charset)) {
	case "utf-8", "utf8", "us-ascii", "ascii":
		return in, nil
	case "iso-8859-1", "latin1", "latin-1", "iso8859-1":
		return &latin1Reader{r: in}, nil
	}
	return nil, fmt.Errorf("unsupported charset %q", charset)
}

// latin1Reader turns ISO-8859-1 bytes into UTF-8.
type latin1Reader struct {
	r   io.Reader
	buf []byte // UTF-8 not yet returned
	in  [512]byte
}

func (l *latin1Reader) Read(p []byte) (int, error) {
	for len(l.buf) == 0 {
		n, err := l.r.Read(l.in[:])
		for _, b := range l.in[:n] {
			l.buf = utf8.AppendRune(l.buf, rune(b))
		}
		if n == 0 && err != nil {
			return 0, err
		}
	}
	n := copy(p, l.buf)
	l.buf = l.buf[n:]
	return n, nil
}
