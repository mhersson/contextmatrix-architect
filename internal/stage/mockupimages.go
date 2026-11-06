package stage

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	pathpkg "path"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
)

// MaxImageBytes caps a repository image a mockup source references.
const MaxImageBytes = 32 << 10

// assetPrefix starts a mockup source's reference to a repository image.
const assetPrefix = "asset:"

// imageExtensions names the extensions imageType takes, for a refusal.
const imageExtensions = ".svg, .png, .jpg, .jpeg, .gif, .webp or .avif"

// Images resolves a mockup's asset: references.
type Images interface {
	// DataURI returns the data: URI of the image at the tool-root-relative
	// path, or an error naming the rule the path breaks.
	DataURI(ctx context.Context, path string) (string, error)
}

// ImageFiles is the image files of the blueprint's checked-out trees and of
// the packages its design gather kept, under Root.
type ImageFiles struct {
	Root     string
	Trees    []string // each tree's top-level directory under Root
	Packages []string // each kept package's top-level directory under Root
}

// imageType is the media type of a lower-cased extension; "" for one no
// mockup takes.
func imageType(ext string) string {
	switch ext {
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".avif":
		return "image/avif"
	}

	return ""
}

// DataURI reads the image at rel when every rule holds and returns it as a
// base64 data: URI.
func (f ImageFiles) DataURI(ctx context.Context, rel string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	segs, mime, err := f.checkPath(rel)
	if err != nil {
		return "", err
	}

	data, err := f.read(rel, segs)
	if err != nil {
		return "", err
	}

	if err := checkImage(rel, mime, data); err != nil {
		return "", err
	}

	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

// checkPath checks rel's shape and returns its segments and media type.
func (f ImageFiles) checkPath(rel string) ([]string, string, error) {
	switch {
	case rel == "":
		return nil, "", errors.New("the path is empty")
	case strings.Contains(rel, `\`):
		return nil, "", fmt.Errorf("%s holds a backslash: separate its segments with /", rel)
	case strings.HasPrefix(rel, "/"):
		return nil, "", fmt.Errorf("%s is absolute: give the path relative to the tool root, starting with a tree's or a kept package's directory", rel)
	}

	segs := strings.Split(rel, "/")

	for _, s := range segs {
		switch s {
		case "":
			return nil, "", fmt.Errorf("%s has an empty segment", rel)
		case ".":
			return nil, "", fmt.Errorf("%s has a . segment", rel)
		case "..":
			return nil, "", fmt.Errorf("%s has a .. segment, which leaves its directory", rel)
		}
	}

	if !slices.Contains(f.Trees, segs[0]) && !slices.Contains(f.Packages, segs[0]) {
		return nil, "", fmt.Errorf("%s does not start with a checked-out tree's directory or a kept package's: %s",
			rel, strings.Join(slices.Concat(f.Trees, f.Packages), ", "))
	}

	// Folded, as a case-insensitive file system opens .GIT as .git.
	for _, s := range segs {
		if strings.EqualFold(s, ".git") || strings.EqualFold(s, "node_modules") {
			return nil, "", fmt.Errorf("%s is inside %s, which is no part of the tree a mockup takes images from", rel, s)
		}
	}

	mime := imageType(strings.ToLower(pathpkg.Ext(rel)))
	if mime == "" {
		return nil, "", fmt.Errorf("%s is no image file a mockup takes: the extension must be %s", rel, imageExtensions)
	}

	return segs, mime, nil
}

// read returns the bytes of the regular file at rel, refusing a symlink at
// any component under Root and a file over MaxImageBytes.
func (f ImageFiles) read(rel string, segs []string) ([]byte, error) {
	at := f.Root

	var info fs.FileInfo

	for i, s := range segs {
		at = filepath.Join(at, s)

		var err error

		info, err = os.Lstat(at)

		switch {
		case errors.Is(err, fs.ErrNotExist):
			return nil, fmt.Errorf("there is no file at %s", rel)
		case err != nil:
			return nil, fmt.Errorf("read %s: %w", rel, err)
		case info.Mode()&fs.ModeSymlink != 0 && i == len(segs)-1:
			return nil, fmt.Errorf("%s is a symlink, which a mockup never follows", rel)
		case info.Mode()&fs.ModeSymlink != 0:
			return nil, fmt.Errorf("%s passes through the symlink %s, which a mockup never follows", rel, strings.Join(segs[:i+1], "/"))
		case i < len(segs)-1 && !info.IsDir():
			return nil, fmt.Errorf("there is no file at %s: %s is no directory", rel, strings.Join(segs[:i+1], "/"))
		}
	}

	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", rel)
	}

	if info.Size() > MaxImageBytes {
		return nil, fmt.Errorf("%s is %d bytes and an image a mockup references takes at most %d", rel, info.Size(), MaxImageBytes)
	}

	// Opened through the root as well, so a tree changed since the checks
	// above still cannot lead the read outside it.
	root, err := os.OpenRoot(f.Root)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", rel, err)
	}
	defer root.Close()

	file, err := root.Open(filepath.FromSlash(rel))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", rel, err)
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, MaxImageBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", rel, err)
	}

	if len(data) > MaxImageBytes {
		return nil, fmt.Errorf("%s is over %d bytes, the most an image a mockup references takes", rel, MaxImageBytes)
	}

	return data, nil
}

// checkImage refuses data that is not the image its extension says.
func checkImage(rel, mime string, data []byte) error {
	switch mime {
	case "image/svg+xml":
		if !utf8.Valid(data) || !bytes.Contains(data, []byte("<svg")) {
			return fmt.Errorf("%s is no SVG drawing: an SVG file is UTF-8 text holding <svg", rel)
		}
	case "image/avif":
		// Go's sniffer does not know AVIF: its ftyp box names the brand.
		if len(data) < 12 || string(data[4:8]) != "ftyp" || string(data[8:12]) != "avif" && string(data[8:12]) != "avis" {
			return fmt.Errorf("%s is no AVIF image: it opens with no ftyp box of the avif or avis brand", rel)
		}
	default:
		if got := http.DetectContentType(data); got != mime {
			return fmt.Errorf("%s is no %s image: its bytes read as %s", rel, mime, got)
		}
	}

	return nil
}

// inlineImages replaces every asset: reference in src's attribute values and
// its style and script elements' text with the image's data: URI; markup
// text, comments and the rest stay byte for byte. img nil refuses every
// reference.
func inlineImages(ctx context.Context, src string, img Images) (string, error) {
	if !strings.Contains(src, assetPrefix) {
		return src, nil
	}

	uris := map[string]string{}

	resolve := func(path string) (string, error) {
		if uri, ok := uris[path]; ok {
			return uri, nil
		}

		if img == nil {
			return "", fmt.Errorf("the source references %s%s, but this run has no checked-out repository to take images from", assetPrefix, path)
		}

		uri, err := img.DataURI(ctx, path)
		if err != nil {
			return "", fmt.Errorf("the source's %s%s cannot be inserted: %w; reference an image file of a checked-out tree, or a kept package image the brief lists, as the brief or the code tools show its path", assetPrefix, path, err)
		}

		uris[path] = uri

		return uri, nil
	}

	var (
		b strings.Builder
		// in is the element whose text the tokenizer reads, as checkMarkup
		// tracks it.
		in string
	)

	b.Grow(len(src))

	z := html.NewTokenizer(strings.NewReader(src))

	for {
		tt := z.Next()
		// Copied before TagName, which lowers the tag's bytes in place.
		raw := string(z.Raw())

		var err error

		switch tt {
		case html.ErrorToken:
			if err := z.Err(); !errors.Is(err, io.EOF) {
				return "", fmt.Errorf("the source does not read as HTML: %w", err)
			}

			b.WriteString(raw)

			return b.String(), nil
		case html.StartTagToken, html.SelfClosingTagToken:
			name, _ := z.TagName()
			in = string(name)
			raw, err = replaceInValues(raw, resolve)
		case html.TextToken:
			if in == "style" || in == "script" {
				raw, err = replaceReferences(raw, resolve)
			}
		case html.EndTagToken:
			in = ""
		}

		if err != nil {
			return "", err
		}

		b.WriteString(raw)
	}
}

// replaceInValues replaces the references in the attribute values of tag, a
// start or self-closing tag's raw text.
func replaceInValues(tag string, resolve func(string) (string, error)) (string, error) {
	var (
		b    strings.Builder
		done int
	)

	for _, span := range attrValues(tag) {
		value := tag[span[0]:span[1]]

		out, err := replaceReferences(value, resolve)
		if err != nil {
			return "", err
		}

		if out == value {
			continue
		}

		b.WriteString(tag[done:span[0]])
		b.WriteString(out)

		done = span[1]
	}

	if done == 0 {
		return tag, nil
	}

	b.WriteString(tag[done:])

	return b.String(), nil
}

// attrValues returns the start and end of each attribute value in tag, a
// start or self-closing tag's raw text, read the way html's tokenizer reads
// them.
func attrValues(tag string) [][2]int {
	var out [][2]int

	i := 1
	for i < len(tag) && !htmlSpace(tag[i]) && tag[i] != '/' && tag[i] != '>' {
		i++
	}

	for i = skipHTMLSpace(tag, i); i < len(tag) && tag[i] != '>'; i = skipHTMLSpace(tag, i) {
		// The name; an = that opens it is part of it.
		for start := i; i < len(tag); i++ {
			if c := tag[i]; htmlSpace(c) || c == '/' || c == '>' || c == '=' && i > start {
				break
			}
		}

		j := skipHTMLSpace(tag, i)

		switch {
		case j < len(tag) && tag[j] == '/':
			i = j + 1

			continue
		case j >= len(tag) || tag[j] != '=':
			i = j

			continue
		}

		j = skipHTMLSpace(tag, j+1)

		switch {
		case j >= len(tag) || tag[j] == '>':
			i = j
		case tag[j] == '"' || tag[j] == '\'':
			end := strings.IndexByte(tag[j+1:], tag[j])
			if end < 0 {
				return append(out, [2]int{j + 1, len(tag)})
			}

			out = append(out, [2]int{j + 1, j + 1 + end})
			i = j + 2 + end
		default:
			end := j
			for end < len(tag) && !htmlSpace(tag[end]) && tag[end] != '>' {
				end++
			}

			out = append(out, [2]int{j, end})
			i = end
		}
	}

	return out
}

func htmlSpace(c byte) bool {
	return c == ' ' || c == '\n' || c == '\r' || c == '\t' || c == '\f'
}

func skipHTMLSpace(s string, i int) int {
	for i < len(s) && htmlSpace(s[i]) {
		i++
	}

	return i
}

// replaceReferences replaces each reference in s with its data: URI: asset:
// not preceded by a letter, digit, _, $ or -, and the longest run of path
// characters after it, holding a /.
func replaceReferences(s string, resolve func(string) (string, error)) (string, error) {
	var (
		b    strings.Builder
		done int
	)

	for from := 0; ; {
		i := strings.Index(s[from:], assetPrefix)
		if i < 0 {
			break
		}

		at := from + i
		start := at + len(assetPrefix)
		end := start + pathRun(s[start:])
		from = start

		// A tree's or kept package's directory leads every path, so a run
		// without a slash, such as CSS's .asset:hover or a script's
		// {asset:src}, is no reference.
		if !strings.Contains(s[start:end], "/") || !referenceStart(s[:at]) {
			continue
		}

		uri, err := resolve(s[start:end])
		if err != nil {
			return "", err
		}

		b.WriteString(s[done:at])
		b.WriteString(uri)

		done, from = end, end
	}

	if done == 0 {
		return s, nil
	}

	b.WriteString(s[done:])

	return b.String(), nil
}

// referenceStart reports whether a reference may follow before: a name
// such as myasset: or data-asset: is no reference.
func referenceStart(before string) bool {
	r, size := utf8.DecodeLastRuneInString(before)
	if size == 0 {
		return true
	}

	return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '$' && r != '-'
}

// pathRun is the length of the run of path characters s starts with.
func pathRun(s string) int {
	for i, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune("._-~@+/", r) {
			return i
		}
	}

	return len(s)
}
