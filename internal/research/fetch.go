package research

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// RuleAddress is the rule a connection to a non-public address breaks.
const RuleAddress = "address"

const userAgent = "contextmatrix-architect web research"

// Page is a fetched page as the model reads it.
type Page struct {
	// URL is where the page was read, after redirects.
	URL   string
	Title string
	Text  string
	// Truncated says Text stops at a size limit.
	Truncated bool
}

// AddressError refuses a connection to an address that is not public.
type AddressError struct {
	Addr netip.Addr
	// Kind is loopback, private, link-local, unspecified, multicast or
	// reserved.
	Kind string
}

func (e *AddressError) Error() string {
	return fmt.Sprintf("%s is a %s address; web_fetch reads public pages only", e.Addr, e.Kind)
}

// TypeError refuses a page that is neither HTML nor text.
type TypeError struct{ Type string }

func (e *TypeError) Error() string {
	return fmt.Sprintf("the page is %s; web_fetch reads HTML and text pages only", e.Type)
}

// limits bound one fetch: the bytes read and the text kept, the redirects
// followed, and the time for the whole request, a dial, a TLS handshake and
// the response headers.
type limits struct {
	body      int64
	text      int
	title     int
	redirects int
	total     time.Duration
	dial      time.Duration
	tls       time.Duration
	header    time.Duration
}

func defaultLimits() limits {
	return limits{
		body: 5 << 20, text: 48 << 10, title: 512, redirects: 5,
		total: 20 * time.Second, dial: 5 * time.Second, tls: 10 * time.Second, header: 10 * time.Second,
	}
}

// Fetcher reads public web pages: http and https only, every connection's
// address checked after its name is resolved, redirects included, and every
// fetch bounded in size and time.
type Fetcher struct {
	client *http.Client
	limits limits
}

// NewFetcher returns a Fetcher that dials public addresses only.
func NewFetcher() *Fetcher {
	return newFetcher(publicOnly, defaultLimits())
}

func newFetcher(allow func(netip.AddrPort) error, l limits) *Fetcher {
	dialer := &net.Dialer{
		Timeout: l.dial,
		// Control sees the address being dialled, after resolution, so a
		// redirect's host and a name that resolves differently between
		// lookups are checked where the connection is made.
		Control: func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return fmt.Errorf("research: dial %s: %w", address, err)
			}

			return allow(netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()))
		},
	}

	transport := &http.Transport{
		// No proxy: the dial check would see the proxy's address, never the
		// page's.
		Proxy:                  nil,
		DialContext:            dialer.DialContext,
		TLSHandshakeTimeout:    l.tls,
		ResponseHeaderTimeout:  l.header,
		MaxResponseHeaderBytes: 64 << 10,
		ForceAttemptHTTP2:      true,
		IdleConnTimeout:        90 * time.Second,
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   l.total,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			switch {
			case len(via) > l.redirects:
				return fmt.Errorf("stopped after %d redirects", l.redirects)
			case req.URL.Scheme != "http" && req.URL.Scheme != "https":
				return &Refusal{Rule: RuleScheme, Token: req.URL.Scheme, Reason: "is not http or https (a redirect)"}
			case req.URL.User != nil:
				return &Refusal{Rule: RuleCredentials, Reason: "a redirect carries credentials"}
			}

			return nil
		},
	}

	return &Fetcher{client: client, limits: l}
}

// Fetch reads rawURL: an HTML page as its title and readable text, a text
// page as it is. Any other type, a status outside 2xx and a refused address
// are errors; a page over the limits is cut and marked truncated.
func (f *Fetcher) Fetch(ctx context.Context, rawURL string) (Page, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return Page{}, fmt.Errorf("research: fetch %s: %w", rawURL, err)
	}

	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html, text/plain;q=0.9, */*;q=0.1")

	resp, err := f.client.Do(req)
	if err != nil {
		return Page{}, fmt.Errorf("research: fetch %s: %w", rawURL, err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Page{}, fmt.Errorf("research: fetch %s: HTTP %d", rawURL, resp.StatusCode)
	}

	br := bufio.NewReader(resp.Body)

	// The type is judged before the body is read: a declared type decides
	// it outright, and an undeclared one from the first bytes only.
	ct := declaredType(resp.Header.Get("Content-Type"))
	if ct == "" {
		var head []byte

		head, err = br.Peek(sniffBytes)
		if err != nil && !errors.Is(err, io.EOF) {
			return Page{}, fmt.Errorf("research: read %s: %w", rawURL, err)
		}

		ct = sniffedType(head)
	}

	if !isPage(ct) {
		return Page{}, &TypeError{Type: ct}
	}

	body, err := io.ReadAll(io.LimitReader(br, f.limits.body+1))
	if err != nil {
		return Page{}, fmt.Errorf("research: read %s: %w", rawURL, err)
	}

	page := Page{URL: resp.Request.URL.String()}

	if int64(len(body)) > f.limits.body {
		body, page.Truncated = body[:f.limits.body], true
	}

	if ct == "text/html" || ct == "application/xhtml+xml" {
		page.Title, page.Text = readable(body)
		page.Title = cutUTF8(page.Title, f.limits.title)
	} else {
		page.Text = strings.TrimSpace(strings.ToValidUTF8(string(body), "�"))
	}

	if len(page.Text) > f.limits.text {
		page.Text, page.Truncated = cutUTF8(page.Text, f.limits.text), true
	}

	return page, nil
}

// sniffBytes is how much of an undeclared body decides its type.
const sniffBytes = 512

// declaredType is the media type a response declares, lower-cased, or ""
// when the header declares none it can parse.
func declaredType(header string) string {
	t, _, err := mime.ParseMediaType(header)
	if err != nil {
		return ""
	}

	return strings.ToLower(t)
}

// sniffedType is the type a body's first bytes suggest.
func sniffedType(head []byte) string {
	t, _, _ := mime.ParseMediaType(http.DetectContentType(head))

	return t
}

// isPage says the fetcher reads a type: HTML or text.
func isPage(t string) bool {
	return t == "text/html" || t == "application/xhtml+xml" || isText(t)
}

func isText(t string) bool {
	switch t {
	case "application/json", "application/xml", "application/yaml", "application/x-yaml", "application/toml", "application/javascript":
		return true
	}

	return strings.HasPrefix(t, "text/")
}

// cutUTF8 cuts s to at most n bytes without splitting a character.
func cutUTF8(s string, n int) string {
	if n >= len(s) {
		return s
	}

	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}

	return s[:n]
}

// reservedRanges are the non-public ranges netip's predicates leave out.
var reservedRanges = []struct {
	prefix netip.Prefix
	kind   string
}{
	{netip.MustParsePrefix("0.0.0.0/8"), "unspecified"},
	// Shared address space: carrier NAT and overlay networks.
	{netip.MustParsePrefix("100.64.0.0/10"), "private"},
	{netip.MustParsePrefix("192.0.0.0/24"), "reserved"},
	{netip.MustParsePrefix("192.0.2.0/24"), "reserved"},
	{netip.MustParsePrefix("198.18.0.0/15"), "reserved"},
	{netip.MustParsePrefix("198.51.100.0/24"), "reserved"},
	{netip.MustParsePrefix("203.0.113.0/24"), "reserved"},
	// Broadcast included.
	{netip.MustParsePrefix("240.0.0.0/4"), "reserved"},
	{netip.MustParsePrefix("64:ff9b:1::/48"), "private"},
	{netip.MustParsePrefix("100::/64"), "reserved"},
	// IETF protocol assignments, Teredo included.
	{netip.MustParsePrefix("2001::/23"), "reserved"},
	{netip.MustParsePrefix("2001:db8::/32"), "reserved"},
	// 6to4 can embed any IPv4 address.
	{netip.MustParsePrefix("2002::/16"), "reserved"},
}

// nat64 embeds an IPv4 address in its last four bytes; that address is
// judged instead.
var nat64 = netip.MustParsePrefix("64:ff9b::/96")

// ipv6Global is the IPv6 space allocated for global unicast.
var ipv6Global = netip.MustParsePrefix("2000::/3")

func publicOnly(ap netip.AddrPort) error {
	return publicAddr(ap.Addr())
}

// publicAddr refuses every address that is not public.
func publicAddr(a netip.Addr) error {
	a = a.Unmap().WithZone("")

	if nat64.Contains(a) {
		b := a.As16()

		return publicAddr(netip.AddrFrom4([4]byte(b[12:])))
	}

	kind := ""

	switch {
	case !a.IsValid() || a.IsUnspecified():
		kind = "unspecified"
	case a.IsLoopback():
		kind = "loopback"
	case a.IsPrivate():
		kind = "private"
	case a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast():
		kind = "link-local"
	case a.IsMulticast() || a.IsInterfaceLocalMulticast():
		kind = "multicast"
	default:
		for _, r := range reservedRanges {
			if r.prefix.Contains(a) {
				kind = r.kind

				break
			}
		}

		// Only 2000::/3 is allocated for global unicast; the rest of the
		// IPv6 space is refused until it is listed as public.
		if kind == "" && a.Is6() && !ipv6Global.Contains(a) {
			kind = "reserved"
		}
	}

	if kind == "" {
		return nil
	}

	return &AddressError{Addr: a, Kind: kind}
}
