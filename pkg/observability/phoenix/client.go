// Copyright 2026 Teradata
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package phoenix is a narrow REST client for Arize Phoenix.
//
// The client exposes only the endpoints Loom's Go code needs: project lookup,
// span and trace listing, and the agent-session routes that drive Phoenix's
// assistant headlessly. There is no generic request escape hatch, so a holder
// of the client cannot reach Phoenix's DELETE, user or API-key endpoints.
//
// The client was written against the Phoenix 20.22.0 OpenAPI document
// ("Arize-Phoenix REST API" 1.0). Request and response types mirror that
// document; fields Loom does not use are not modelled.
package phoenix

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/teradata-labs/loom/pkg/observability"
)

const (
	// DefaultRequestTimeout bounds one non-streaming request.
	DefaultRequestTimeout = 15 * time.Second
	// DefaultMaxPages bounds how many pages a List* call follows.
	DefaultMaxPages = 20
	// DefaultMaxResponseBytes bounds one response body.
	DefaultMaxResponseBytes int64 = 8 << 20
	// DefaultRetryBackoff is the base backoff between retried GET requests.
	DefaultRetryBackoff = 200 * time.Millisecond
	// defaultRetryAttempts is the total number of attempts for a retryable GET.
	defaultRetryAttempts = 3

	// AttrMetaAgent marks spans emitted by Sentinel's own Phoenix traffic so an
	// exporter can keep them out of the project they describe.
	AttrMetaAgent = "loom.meta_agent"
	// MetaAgentSentinel is the AttrMetaAgent value for Sentinel.
	MetaAgentSentinel = "sentinel"
)

// Config configures a Client.
type Config struct {
	// BaseURL is the Phoenix origin, e.g. "http://phoenix.example:6006".
	BaseURL string
	// APIKey is sent as "Authorization: Bearer <key>". It is never logged or
	// included in errors.
	APIKey string
	// RequestTimeout bounds each non-streaming request. Zero means
	// DefaultRequestTimeout. Streaming chat requests are bounded only by the
	// caller's context.
	RequestTimeout time.Duration
	// MaxPages bounds how many pages a List* call follows. Zero means
	// DefaultMaxPages.
	MaxPages int
	// MaxResponseBytes bounds each response body. Zero means
	// DefaultMaxResponseBytes. A body over the cap fails the call with
	// ErrResponseTooLarge.
	MaxResponseBytes int64
	// RetryBackoff is the base backoff for retried GET requests. Zero means
	// DefaultRetryBackoff.
	RetryBackoff time.Duration
	// HTTPClient is optional. Its own Timeout must be zero or longer than any
	// streaming chat turn; the client applies RequestTimeout per request.
	HTTPClient *http.Client
	// Tracer is optional. When set, each call opens a "phoenix.rest.<op>" span
	// tagged AttrMetaAgent=MetaAgentSentinel.
	Tracer observability.Tracer
}

// Client talks to one Phoenix deployment.
type Client struct {
	baseURL      string
	apiKey       string
	timeout      time.Duration
	maxPages     int
	maxBytes     int64
	retryBackoff time.Duration
	http         *http.Client
	tracer       observability.Tracer
}

// New creates a Client. BaseURL and APIKey are required.
func New(cfg Config) (*Client, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		return nil, errors.New("phoenix: base URL is required")
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("phoenix: invalid base URL %q", base)
	}
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("phoenix: API key is required")
	}
	c := &Client{
		baseURL:      base,
		apiKey:       cfg.APIKey,
		timeout:      cfg.RequestTimeout,
		maxPages:     cfg.MaxPages,
		maxBytes:     cfg.MaxResponseBytes,
		retryBackoff: cfg.RetryBackoff,
		http:         cfg.HTTPClient,
		tracer:       cfg.Tracer,
	}
	if c.timeout <= 0 {
		c.timeout = DefaultRequestTimeout
	}
	if c.maxPages <= 0 {
		c.maxPages = DefaultMaxPages
	}
	if c.maxBytes <= 0 {
		c.maxBytes = DefaultMaxResponseBytes
	}
	if c.retryBackoff <= 0 {
		c.retryBackoff = DefaultRetryBackoff
	}
	if c.http == nil {
		c.http = &http.Client{}
	}
	return c, nil
}

// BaseURL returns the configured Phoenix origin.
func (c *Client) BaseURL() string { return c.baseURL }

// ErrResponseTooLarge is returned when a response body exceeds MaxResponseBytes.
var ErrResponseTooLarge = errors.New("phoenix: response body exceeds configured cap")

// APIError is a non-2xx response that is not an agent-session conflict.
// Body is truncated and never contains the request's credentials.
type APIError struct {
	StatusCode int
	Method     string
	Path       string
	Body       string
}

func (e *APIError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("phoenix: %s %s returned HTTP %d", e.Method, e.Path, e.StatusCode)
	}
	return fmt.Sprintf("phoenix: %s %s returned HTTP %d: %s", e.Method, e.Path, e.StatusCode, e.Body)
}

// ConflictError is the body of every HTTP 409 from the agent-session routes.
type ConflictError struct {
	// Code is one of the agent_session_* codes, e.g. "agent_session_busy".
	Code string `json:"code"`
	// Message is Phoenix's optional elaboration.
	Message string `json:"message,omitempty"`
}

func (e *ConflictError) Error() string {
	if e.Message == "" {
		return "phoenix: conflict " + e.Code
	}
	return "phoenix: conflict " + e.Code + ": " + e.Message
}

// Conflict codes returned by the agent-session routes (Phoenix 20.22.0).
const (
	ConflictSessionBusy           = "agent_session_busy"
	ConflictModelStale            = "agent_session_model_stale"
	ConflictMessagesStale         = "agent_session_messages_stale"
	ConflictToolOutputsConflict   = "agent_session_tool_outputs_conflict"
	ConflictToolApprovalsConflict = "agent_session_tool_approvals_conflict"
)

// IsConflict reports whether err is a ConflictError with the given code.
func IsConflict(err error, code string) bool {
	var ce *ConflictError
	return errors.As(err, &ce) && ce.Code == code
}

const maxErrorBodyBytes = 2048

// startSpan opens a client span when a tracer is configured.
func (c *Client) startSpan(ctx context.Context, op string) (context.Context, func(error)) {
	if c.tracer == nil {
		return ctx, func(error) {}
	}
	ctx, span := c.tracer.StartSpan(ctx, "phoenix.rest."+op,
		observability.WithAttribute(AttrMetaAgent, MetaAgentSentinel),
		observability.WithAttribute("phoenix.operation", op),
	)
	return ctx, func(err error) {
		if err != nil {
			span.RecordError(err)
		}
		c.tracer.EndSpan(span)
	}
}

// newRequest builds an authenticated request. query may be nil.
func (c *Client) newRequest(ctx context.Context, method, path string, query url.Values, body []byte) (*http.Request, error) {
	full := c.baseURL + path
	if len(query) > 0 {
		full += "?" + query.Encode()
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, full, rdr)
	if err != nil {
		return nil, fmt.Errorf("phoenix: building %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// doJSON performs a non-streaming request with RequestTimeout, decodes a 2xx
// body into out (when out is non-nil) and maps other statuses to errors.
// GET requests are retried on 429, 502, 503, 504 and transport errors.
func (c *Client) doJSON(ctx context.Context, method, path string, query url.Values, in, out any) error {
	var body []byte
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("phoenix: encoding %s %s body: %w", method, path, err)
		}
		body = b
	}

	attempts := 1
	if method == http.MethodGet {
		attempts = defaultRetryAttempts
	}

	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			wait := c.retryBackoff << uint(attempt-1)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
		retry, err := c.doJSONOnce(ctx, method, path, query, body, out)
		if err == nil {
			return nil
		}
		lastErr = err
		if !retry || ctx.Err() != nil {
			return err
		}
	}
	return lastErr
}

// doJSONOnce performs one attempt. The bool reports whether the failure is
// retryable.
func (c *Client) doJSONOnce(ctx context.Context, method, path string, query url.Values, body []byte, out any) (bool, error) {
	rctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := c.newRequest(rctx, method, path, query, body)
	if err != nil {
		return false, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return true, fmt.Errorf("phoenix: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := c.readBody(resp.Body)
	if err != nil {
		return false, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return retryableStatus(resp.StatusCode), c.statusError(method, path, resp.StatusCode, data)
	}
	if out == nil || len(data) == 0 {
		return false, nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return false, fmt.Errorf("phoenix: decoding %s %s response: %w", method, path, err)
	}
	return false, nil
}

func retryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// readBody reads at most maxBytes from r and fails when the body is larger.
func (c *Client) readBody(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, c.maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("phoenix: reading response: %w", err)
	}
	if int64(len(data)) > c.maxBytes {
		return nil, ErrResponseTooLarge
	}
	return data, nil
}

// statusError maps a non-2xx response to ConflictError or APIError.
func (c *Client) statusError(method, path string, status int, data []byte) error {
	if status == http.StatusConflict {
		var ce ConflictError
		if json.Unmarshal(data, &ce) == nil && ce.Code != "" {
			return &ce
		}
	}
	text := strings.TrimSpace(string(data))
	if len(text) > maxErrorBodyBytes {
		text = text[:maxErrorBodyBytes] + "..."
	}
	return &APIError{StatusCode: status, Method: method, Path: path, Body: text}
}

// Project is one Phoenix project.
type Project struct {
	// ID is the project's GlobalID, the value UI contexts call projectNodeId.
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// GetProject looks a project up by name or GlobalID.
func (c *Client) GetProject(ctx context.Context, identifier string) (Project, error) {
	ctx, end := c.startSpan(ctx, "get_project")
	var err error
	defer func() { end(err) }()

	if strings.TrimSpace(identifier) == "" {
		err = errors.New("phoenix: project identifier is required")
		return Project{}, err
	}
	var resp struct {
		Data Project `json:"data"`
	}
	err = c.doJSON(ctx, http.MethodGet, "/v1/projects/"+url.PathEscape(identifier), nil, nil, &resp)
	if err != nil {
		return Project{}, err
	}
	return resp.Data, nil
}

// Version returns the Phoenix server version string.
func (c *Client) Version(ctx context.Context) (string, error) {
	ctx, end := c.startSpan(ctx, "version")
	var err error
	defer func() { end(err) }()

	rctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := c.newRequest(rctx, http.MethodGet, "/arize_phoenix_version", nil, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		err = fmt.Errorf("phoenix: GET /arize_phoenix_version: %w", err)
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := c.readBody(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		err = c.statusError(http.MethodGet, "/arize_phoenix_version", resp.StatusCode, data)
		return "", err
	}
	return strings.Trim(strings.TrimSpace(string(data)), `"`), nil
}
