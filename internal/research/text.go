package research

import (
	"bytes"
	"strings"

	"golang.org/x/net/html"
)

// skippedElements carry no readable text: scripts, styles, embedded objects
// and the furniture around a page's content.
var skippedElements = map[string]bool{
	"head": true, "script": true, "style": true, "noscript": true, "template": true, "svg": true,
	"iframe": true, "object": true, "canvas": true, "nav": true, "footer": true, "aside": true,
	"form": true, "button": true, "select": true, "textarea": true,
}

// blockElements start and end a paragraph; lineElements a line.
var (
	blockElements = map[string]bool{
		"p": true, "div": true, "section": true, "article": true, "main": true, "header": true,
		"ul": true, "ol": true, "dl": true, "table": true, "blockquote": true, "pre": true,
		"figure": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
		"hr": true, "details": true,
	}
	lineElements = map[string]bool{
		"li": true, "tr": true, "dt": true, "dd": true, "br": true, "figcaption": true, "summary": true,
	}
)

// readable is an HTML page's title and readable text: the main or article
// element when there is one, else the body, without page furniture or
// hidden elements, with headings marked by # and list items by -, and
// preformatted text kept as it is.
func readable(body []byte) (string, string) {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return tokenized(body)
	}

	title := ""
	if t := first(doc, "title"); t != nil {
		title = strings.Join(strings.Fields(textOf(t)), " ")
	}

	root := first(doc, "main")
	if root == nil {
		root = first(doc, "article")
	}

	if root == nil {
		root = first(doc, "body")
	}

	if root == nil {
		root = doc
	}

	var w textWriter

	w.walk(root)

	return title, strings.ToValidUTF8(strings.TrimSpace(w.b.String()), "�")
}

// tokenized is the readable text of markup the parser refuses, such as
// nesting deeper than it accepts: no title, and the text outside the skipped
// elements, read token by token through the same writer.
func tokenized(body []byte) (string, string) {
	z := html.NewTokenizer(bytes.NewReader(body))

	var w textWriter

	skip := 0

	for {
		switch z.Next() {
		case html.ErrorToken:
			return "", strings.ToValidUTF8(strings.TrimSpace(w.b.String()), "�")
		case html.TextToken:
			if skip == 0 {
				w.text(z.Token().Data)
			}
		case html.StartTagToken:
			name := z.Token().Data

			switch {
			case skippedElements[name]:
				skip++
			case skip == 0:
				w.open(name)
			}
		case html.SelfClosingTagToken:
			// Never a skipped element's start: it has no end tag to match.
			if name := z.Token().Data; skip == 0 && !skippedElements[name] {
				w.open(name)
			}
		case html.EndTagToken:
			name := z.Token().Data

			switch {
			case skippedElements[name]:
				if skip > 0 {
					skip--
				}
			case skip == 0:
				w.close(name)
			}
		}
	}
}

func first(n *html.Node, tag string) *html.Node {
	if n.Type == html.ElementNode && n.Data == tag {
		return n
	}

	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if f := first(c, tag); f != nil {
			return f
		}
	}

	return nil
}

func textOf(n *html.Node) string {
	if n.Type == html.TextNode {
		return n.Data
	}

	var b strings.Builder

	for c := n.FirstChild; c != nil; c = c.NextSibling {
		b.WriteString(textOf(c))
	}

	return b.String()
}

func hidden(n *html.Node) bool {
	for _, a := range n.Attr {
		if a.Key == "hidden" || (a.Key == "aria-hidden" && a.Val == "true") {
			return true
		}
	}

	return false
}

// textWriter collapses whitespace outside pre and puts at most one blank
// line between blocks.
type textWriter struct {
	b strings.Builder
	// breaks are the newlines owed before the next text, space a space.
	breaks int
	space  bool
	pre    int
}

func (w *textWriter) walk(n *html.Node) {
	name := ""

	switch n.Type {
	case html.TextNode:
		w.text(n.Data)

		return
	case html.ElementNode:
		if skippedElements[n.Data] || hidden(n) {
			return
		}

		name = n.Data
	}

	w.open(name)

	for c := n.FirstChild; c != nil; c = c.NextSibling {
		w.walk(c)
	}

	w.close(name)
}

func (w *textWriter) open(name string) {
	switch {
	case blockElements[name]:
		w.owe(2)
	case lineElements[name]:
		w.owe(1)
	case name == "td" || name == "th":
		w.space = true
	}

	switch {
	case len(name) == 2 && name[0] == 'h' && name[1] >= '1' && name[1] <= '6':
		w.write(strings.Repeat("#", int(name[1]-'0')) + " ")
	case name == "li":
		w.write("- ")
	case name == "pre":
		w.pre++
	}
}

func (w *textWriter) close(name string) {
	if name == "pre" {
		w.pre--
	}

	switch {
	case blockElements[name]:
		w.owe(2)
	case lineElements[name]:
		w.owe(1)
	}
}

func (w *textWriter) owe(n int) {
	if w.b.Len() > 0 && n > w.breaks {
		w.breaks = n
	}
}

func (w *textWriter) write(s string) {
	switch {
	case w.breaks > 0:
		w.b.WriteString(strings.Repeat("\n", w.breaks))
	case w.space && w.b.Len() > 0:
		w.b.WriteByte(' ')
	}

	w.breaks, w.space = 0, false
	w.b.WriteString(s)
}

func (w *textWriter) text(s string) {
	if w.pre > 0 {
		w.write(s)

		return
	}

	fields := strings.Fields(s)
	if len(fields) == 0 {
		w.space = w.space || s != ""

		return
	}

	if isSpace(s[0]) {
		w.space = true
	}

	w.write(strings.Join(fields, " "))
	w.space = isSpace(s[len(s)-1])
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f'
}
