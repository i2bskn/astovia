package google

import (
	"encoding/base64"
	"io"
	"mime"
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/text/encoding/htmlindex"
)

const (
	// TextLimit is the most characters of a message body kept.
	TextLimit = 4000
	// fromLimit bounds the raw From used as a name when it cannot be
	// parsed.
	fromLimit = 200
)

// charsetReader converts text in charset to UTF-8, for mime.WordDecoder and
// the address parser.
func charsetReader(charset string, input io.Reader) (io.Reader, error) {
	enc, err := htmlindex.Get(charset)
	if err != nil {
		return nil, err
	}
	return enc.NewDecoder().Reader(input), nil
}

var wordDecoder = &mime.WordDecoder{CharsetReader: charsetReader}

// decodeHeader decodes the RFC 2047 encoded words of a header value. A
// value that cannot be decoded is returned as is.
func decodeHeader(v string) string {
	out, err := wordDecoder.DecodeHeader(v)
	if err != nil {
		return v
	}
	return out
}

// cleanLine removes control and format characters and surrounding spaces.
func cleanLine(v string) string {
	v = strings.Map(func(r rune) rune {
		if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp) {
			return -1
		}
		return r
	}, v)
	return strings.TrimFunc(v, unicode.IsSpace)
}

func truncate(v string, n int) string {
	if utf8.RuneCountInString(v) <= n {
		return v
	}
	return string([]rune(v)[:n])
}

// parseFrom parses a From header into the sender's name and lower-case
// address. When it cannot be parsed, the name is the raw value (decoded,
// without control characters, at most fromLimit characters) and the
// address is empty.
func parseFrom(raw string) (string, string) {
	list, ok := parseAddresses(raw)
	if ok && len(list) > 0 {
		return list[0].Name, list[0].Email
	}
	return truncate(cleanLine(decodeHeader(raw)), fromLimit), ""
}

// parseAddresses parses an address list header. An empty header is an
// empty list. The second result is false when the header cannot be
// parsed.
func parseAddresses(raw string) ([]Address, bool) {
	if strings.TrimSpace(raw) == "" {
		return []Address{}, true
	}
	p := mail.AddressParser{WordDecoder: wordDecoder}
	list, err := p.ParseList(raw)
	if err != nil {
		return nil, false
	}
	out := make([]Address, 0, len(list))
	for _, a := range list {
		out = append(out, Address{Name: cleanLine(a.Name), Email: strings.ToLower(a.Address)})
	}
	return out, true
}

// bodyText returns the text of a message for the AI: the text/html part
// turned into text when there is one, so that the AI reads what the user
// sees, otherwise the text/plain part. Nested multiparts are followed and
// attachments (parts with a file name) are not read. Quotes are removed and
// the text is cut at TextLimit characters.
func bodyText(p part) string {
	var htmlPart, plainPart *part
	var walk func(p *part)
	walk = func(p *part) {
		if p.Filename != "" || p.Body.AttachmentID != "" {
			return
		}
		mt, _, _ := mime.ParseMediaType(p.MimeType)
		switch {
		case strings.HasPrefix(mt, "multipart/"):
			for i := range p.Parts {
				walk(&p.Parts[i])
			}
		case mt == "text/html" && htmlPart == nil:
			htmlPart = p
		case mt == "text/plain" && plainPart == nil:
			plainPart = p
		}
	}
	walk(&p)
	var text string
	switch {
	case htmlPart != nil:
		text = htmlText(partText(*htmlPart))
	case plainPart != nil:
		text = strings.ReplaceAll(strings.ReplaceAll(partText(*plainPart), "\r\n", "\n"), "\r", "\n")
	}
	return truncate(strings.TrimSpace(stripQuotes(text)), TextLimit)
}

// partText decodes the base64url body of a part and converts it from the
// charset of its Content-Type to UTF-8. An unknown charset is read as UTF-8,
// and invalid bytes are replaced.
func partText(p part) string {
	data := strings.TrimRight(strings.TrimSpace(p.Body.Data), "=")
	raw, err := base64.RawURLEncoding.DecodeString(data)
	if err != nil {
		// Gmail sends base64url; standard base64 is accepted as well.
		if raw, err = base64.RawStdEncoding.DecodeString(data); err != nil {
			return ""
		}
	}
	charset := ""
	if ct, ok := p.header("Content-Type"); ok {
		if _, params, err := mime.ParseMediaType(ct); err == nil {
			charset = strings.ToLower(strings.TrimSpace(params["charset"]))
		}
	}
	if charset != "" && charset != "utf-8" && charset != "utf8" && charset != "us-ascii" {
		if enc, err := htmlindex.Get(charset); err == nil {
			if out, err := enc.NewDecoder().Bytes(raw); err == nil {
				raw = out
			}
		}
	}
	return strings.ToValidUTF8(string(raw), "�")
}

// skippedElements are removed with their content.
var skippedElements = map[string]bool{"head": true, "title": true, "script": true, "style": true, "template": true, "noscript": true, "svg": true}

// blockElements end a line.
var blockElements = map[string]bool{"p": true, "div": true, "li": true, "tr": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true}

// htmlText turns an HTML body into text: elements that are not shown (see
// hiddenElement) are removed with their content, comments are dropped, br
// and the block elements start a new line, entities are decoded, runs of
// white space are collapsed outside pre and empty lines are dropped.
func htmlText(src string) string {
	doc, err := html.Parse(strings.NewReader(src))
	if err != nil {
		return ""
	}
	var b strings.Builder
	var walk func(n *html.Node, pre bool)
	walk = func(n *html.Node, pre bool) {
		switch n.Type {
		case html.TextNode:
			if pre {
				b.WriteString(n.Data)
			} else {
				b.WriteString(collapseSpace(n.Data))
			}
			return
		case html.ElementNode:
			name := strings.ToLower(n.Data)
			if skippedElements[name] || hiddenElement(n) {
				return
			}
			if name == "br" {
				b.WriteString("\n")
				return
			}
			pre = pre || name == "pre"
			if blockElements[name] {
				b.WriteString("\n")
			}
		case html.CommentNode, html.DoctypeNode:
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, pre)
		}
		if n.Type == html.ElementNode && blockElements[strings.ToLower(n.Data)] {
			b.WriteString("\n")
		}
	}
	walk(doc, false)
	return tidyLines(b.String())
}

// spaceRun is a run of the white space HTML collapses, with no-break
// spaces read as spaces.
var spaceRun = regexp.MustCompile(`[ \t\n\r\f\x{00A0}]+`)

func collapseSpace(v string) string { return spaceRun.ReplaceAllString(v, " ") }

// tidyLines trims the lines and drops the empty ones.
func tidyLines(v string) string {
	var out []string
	for _, line := range strings.Split(v, "\n") {
		line = strings.TrimFunc(line, func(r rune) bool { return r == ' ' || r == '\t' || r == '\r' || r == '\u00a0' })
		if line != "" {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// hiddenElement reports whether an element is not shown: it has the hidden
// attribute, aria-hidden="true", the class gmail_quote (a quote of an
// earlier message), or a style attribute that hides it.
func hiddenElement(n *html.Node) bool {
	for _, a := range n.Attr {
		key := strings.ToLower(a.Key)
		switch {
		case key == "hidden":
			return true
		case key == "aria-hidden" && strings.EqualFold(strings.TrimSpace(a.Val), "true"):
			return true
		case key == "class":
			for _, c := range strings.Fields(a.Val) {
				if c == "gmail_quote" {
					return true
				}
			}
		case key == "style" && hiddenStyle(a.Val):
			return true
		}
	}
	return false
}

// hiddenStyle reports whether a style attribute hides its element:
// display: none, visibility: hidden, opacity, font-size or max-height of
// zero, or a height or width of zero with overflow: hidden. The
// declarations are compared whole, not as substrings.
func hiddenStyle(style string) bool {
	decls := map[string]string{}
	for _, d := range strings.Split(style, ";") {
		prop, value, ok := strings.Cut(d, ":")
		if !ok {
			continue
		}
		prop = strings.ToLower(strings.TrimSpace(prop))
		value = strings.ToLower(strings.TrimSpace(value))
		value = strings.TrimSpace(strings.TrimSuffix(value, "!important"))
		decls[prop] = value
	}
	switch {
	case decls["display"] == "none", decls["visibility"] == "hidden":
		return true
	case zero(decls["opacity"]), zero(decls["font-size"]), zero(decls["max-height"]):
		return true
	case (zero(decls["height"]) || zero(decls["width"])) && decls["overflow"] == "hidden":
		return true
	}
	return false
}

var numberPattern = regexp.MustCompile(`^([+-]?(?:[0-9]+\.?[0-9]*|\.[0-9]+))([a-z%]*)$`)

// zero reports whether a CSS value is a number equal to zero, with or
// without a unit (0, 0px, 0em, 0%, 0.0).
func zero(v string) bool {
	m := numberPattern.FindStringSubmatch(v)
	if m == nil {
		return false
	}
	f, err := strconv.ParseFloat(m[1], 64)
	return err == nil && f == 0
}

var (
	// wrotePattern is the line an English mail client puts before a
	// quote, and wrotePatternJA the Japanese ones.
	wrotePattern    = regexp.MustCompile(`^On .+ wrote:$`)
	wrotePatternJA  = regexp.MustCompile(`さんは書きました[:：]$`)
	wrotePatternJA2 = regexp.MustCompile(`^[0-9]{4}年[0-9]{1,2}月[0-9]{1,2}日\s*[(（].[)）]\s*[0-9]{1,2}:[0-9]{2}.*<[^<>]+>\s*[:：]$`)
)

// stripQuotes removes the quoted lines (starting with >) and everything
// from the line that introduces a quote ("On … wrote:", "…さんは書きました:",
// "2026年10月28日(水) 9:30 name <address>:").
func stripQuotes(v string) string {
	var out []string
	for _, line := range strings.Split(v, "\n") {
		t := strings.TrimSpace(line)
		if wrotePattern.MatchString(t) || wrotePatternJA.MatchString(t) || wrotePatternJA2.MatchString(t) {
			break
		}
		if strings.HasPrefix(t, ">") {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}
