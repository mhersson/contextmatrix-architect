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
	"strings"
	"time"
)

// ErrSessionNotLive is returned when CM refuses credentials because the
// blueprint's session is not running or not known to it.
var ErrSessionNotLive = errors.New("code: credentials refused, the session is not live")

// CM's error codes for a session that cannot mint credentials.
const (
	codeWorkerNotRunning = "WORKER_NOT_RUNNING"
	codeChatNotFound     = "CHAT_NOT_FOUND"
)

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
		url:    strings.TrimRight(baseURL, "/") + "/api/worker/git-credentials",
		bearer: bearer,
	}
}

// Fetch mints a credential for (host, path). A session CM reports as not
// running (409 WORKER_NOT_RUNNING) or unknown (404 CHAT_NOT_FOUND) is
// ErrSessionNotLive; any other refusal is an error carrying CM's error text.
// No error ever carries the bearer.
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

	if resp.StatusCode >= http.StatusBadRequest {
		return "", "", c.refusal(resp.StatusCode, body)
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

// refusal maps CM's APIError body to an error: the session codes to
// ErrSessionNotLive, anything else to CM's error text and code.
func (c *CMCredentials) refusal(status int, body []byte) error {
	var apiErr struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}

	if json.Unmarshal(body, &apiErr) != nil || apiErr.Error == "" {
		return fmt.Errorf("code: credentials endpoint returned %d", status)
	}

	if status == http.StatusConflict && apiErr.Code == codeWorkerNotRunning ||
		status == http.StatusNotFound && apiErr.Code == codeChatNotFound {
		return ErrSessionNotLive
	}

	// CM's error text holds no secret; the bearer is masked in case a
	// server ever echoes it.
	text := apiErr.Error
	if c.bearer != "" {
		text = strings.ReplaceAll(text, c.bearer, "[redacted]")
	}

	return fmt.Errorf("code: credentials refused (%d %s): %s", status, apiErr.Code, text)
}
