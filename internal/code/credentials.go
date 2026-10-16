// Package code keeps the read-only code the stages ground cards in: one bare
// mirror per repository, one plain tree per blueprint at a recorded commit,
// and the facts repo_info reports. Nothing here writes to a repository.
package code

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// ErrSessionNotLive is returned when CM refuses credentials because the
// blueprint's session is not attached.
var ErrSessionNotLive = errors.New("code: credentials refused, the session is not live")

// Credentials mints a git credential for one repository. The architect
// implements it over CM; tests use nil for public fixtures.
type Credentials interface {
	Fetch(ctx context.Context, host, path string) (username, token string, err error)
}

// CMCredentials fetches per-repo credentials from CM's worker endpoint with
// the session's git-credentials token as the bearer.
type CMCredentials struct {
	client *http.Client
	url    string
	bearer string
}

// NewCMCredentials builds a client for <baseURL>/api/worker/git-credentials.
func NewCMCredentials(baseURL, bearer string) *CMCredentials {
	return &CMCredentials{
		client: &http.Client{Timeout: 15 * time.Second},
		url:    baseURL + "/api/worker/git-credentials",
		bearer: bearer,
	}
}

// Fetch mints a credential for (host, path). A 409 is ErrSessionNotLive; any
// other failure is an error that never carries the bearer.
func (c *CMCredentials) Fetch(ctx context.Context, host, path string) (string, string, error) {
	u, err := url.Parse(c.url)
	if err != nil {
		return "", "", fmt.Errorf("code: credentials url: %w", err)
	}

	q := u.Query()
	q.Set("host", host)
	q.Set("path", path)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", "", fmt.Errorf("code: credentials request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.bearer)

	resp, err := c.client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("code: fetch credentials: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch {
	case resp.StatusCode == http.StatusConflict:
		return "", "", ErrSessionNotLive
	case resp.StatusCode >= 400:
		return "", "", fmt.Errorf("code: credentials endpoint returned %d", resp.StatusCode)
	}

	var cred struct {
		Username string `json:"username"`
		Token    string `json:"token"`
	}

	if err := json.Unmarshal(body, &cred); err != nil {
		return "", "", fmt.Errorf("code: decode credentials: %w", err)
	}

	if cred.Token == "" {
		return "", "", errors.New("code: credentials response carried no token")
	}

	return cred.Username, cred.Token, nil
}
