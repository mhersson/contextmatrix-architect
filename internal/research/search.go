package research

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// Result is one search hit as the model reads it.
type Result struct {
	Title   string
	URL     string
	Snippet string
}

// Searcher runs one web search.
type Searcher interface {
	Search(ctx context.Context, query string) ([]Result, error)
}

// ProviderBrave names the Brave Search API in a session's research settings.
const ProviderBrave = "brave"

// NewSearcher returns the provider a session's research settings name.
func NewSearcher(provider, apiKey string) (Searcher, error) {
	switch {
	case strings.TrimSpace(apiKey) == "":
		return nil, errors.New("the search settings carry no API key")
	case provider == ProviderBrave:
		return NewBrave(apiKey), nil
	default:
		return nil, fmt.Errorf("search provider %q is not supported", provider)
	}
}

const (
	braveEndpoint = "https://api.search.brave.com/res/v1/web/search"
	braveCount    = 10
	braveTimeout  = 15 * time.Second
	maxSearchBody = 2 << 20
)

// Brave is the Brave Search API's web search. The key travels in the
// X-Subscription-Token header only, so no URL, error or log line holds it.
type Brave struct {
	key      string
	endpoint string
	client   *http.Client
}

// NewBrave returns a Brave client for apiKey.
func NewBrave(apiKey string) *Brave {
	// The key header would follow a redirect to another host, so redirects are not followed.
	client := &http.Client{
		Timeout:       braveTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	return &Brave{key: apiKey, endpoint: braveEndpoint, client: client}
}

// Search returns up to ten web results for query.
func (b *Brave) Search(ctx context.Context, query string) ([]Result, error) {
	u, err := url.Parse(b.endpoint)
	if err != nil {
		return nil, fmt.Errorf("research: brave endpoint: %w", err)
	}

	q := u.Query()
	q.Set("q", query)
	q.Set("count", strconv.Itoa(braveCount))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("research: brave search: %w", err)
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", b.key)

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("research: brave search: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSearchBody))
	if err != nil {
		return nil, fmt.Errorf("research: brave search: read the response: %w", err)
	}

	switch code := resp.StatusCode; {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return nil, fmt.Errorf("research: brave search: the API key was refused (HTTP %d)", code)
	case code == http.StatusTooManyRequests:
		return nil, errors.New("research: brave search: rate limited (HTTP 429); wait before searching again")
	case code < 200 || code > 299:
		return nil, fmt.Errorf("research: brave search: HTTP %d", code)
	}

	var out struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}

	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("research: brave search: decode the response: %w", err)
	}

	results := make([]Result, 0, len(out.Web.Results))

	for _, r := range out.Web.Results {
		if strings.TrimSpace(r.URL) == "" {
			continue
		}

		results = append(results, Result{Title: plain(r.Title), URL: r.URL, Snippet: plain(r.Description)})
	}

	return results, nil
}

var markup = regexp.MustCompile(`<[^>]*>`)

// plain is a result field without the markup Brave highlights matches with.
func plain(s string) string {
	return strings.Join(strings.Fields(html.UnescapeString(markup.ReplaceAllString(s, ""))), " ")
}
