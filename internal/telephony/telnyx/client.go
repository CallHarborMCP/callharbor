package telnyx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultBaseURL is the production Telnyx REST root from the pinned OpenAPI
// servers entry (https://api.telnyx.com/v2).
const DefaultBaseURL = "https://api.telnyx.com/v2"

const maxResponseBody = 1 << 20

var (
	ErrInvalidClient = errors.New("telnyx: invalid client configuration")
	ErrAPI           = errors.New("telnyx: API request rejected")
	ErrBadResponse   = errors.New("telnyx: unexpected API response")
	// ErrNotFound reports a lookup that Telnyx answered with no matching record.
	ErrNotFound = errors.New("telnyx: not found")
)

// APIError is a non-2xx Telnyx response. It carries the method, the path
// without its query string, and the HTTP status. It never carries the response
// body or the API key, and it matches ErrAPI.
type APIError struct {
	Method string
	Path   string
	Status int
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%v: %s %s returned HTTP %d", ErrAPI, e.Method, e.Path, e.Status)
}

// Unwrap makes errors.Is(err, ErrAPI) hold for every APIError.
func (e *APIError) Unwrap() error { return ErrAPI }

// Client is a minimal Telnyx Call Control REST client. It performs at most one
// HTTP attempt per call: an automatic retry could create a second dial, so
// idempotency is provided by command_id instead.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// NewClient returns a Client. Plaintext HTTP is accepted only for loopback
// emulators, so the API key is never sent in clear text to a remote host.
func NewClient(baseURL, apiKey string, hc *http.Client) (*Client, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("%w: api key required", ErrInvalidClient)
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("%w: base url", ErrInvalidClient)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopback(u.Hostname()) {
			return nil, fmt.Errorf("%w: plaintext http only for loopback", ErrInvalidClient)
		}
	default:
		return nil, fmt.Errorf("%w: base url scheme", ErrInvalidClient)
	}
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, http: hc}, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// DialResult holds the identifiers Telnyx returns for an accepted dial.
type DialResult struct {
	CallControlID string
	CallLegID     string
	CallSessionID string
}

type dialResponse struct {
	Data *struct {
		CallControlID string `json:"call_control_id"`
		CallLegID     string `json:"call_leg_id"`
		CallSessionID string `json:"call_session_id"`
	} `json:"data"`
}

// Dial starts one outbound call (POST /v2/calls). The request is built and
// validated by BuildDialRequest, so the body always satisfies the pinned
// CallRequest schema.
func (c *Client) Dial(ctx context.Context, p DialParams) (DialResult, error) {
	req, err := BuildDialRequest(p)
	if err != nil {
		return DialResult{}, err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return DialResult{}, err
	}
	raw, err := c.do(ctx, http.MethodPost, "/calls", body)
	if err != nil {
		return DialResult{}, err
	}
	var resp dialResponse
	if err := json.Unmarshal(raw, &resp); err != nil || resp.Data == nil {
		return DialResult{}, fmt.Errorf("%w: dial response", ErrBadResponse)
	}
	if resp.Data.CallControlID == "" || resp.Data.CallLegID == "" {
		return DialResult{}, fmt.Errorf("%w: dial response missing call identifiers", ErrBadResponse)
	}
	return DialResult{
		CallControlID: resp.Data.CallControlID,
		CallLegID:     resp.Data.CallLegID,
		CallSessionID: resp.Data.CallSessionID,
	}, nil
}

// Hangup ends a call leg (POST /v2/calls/{call_control_id}/actions/hangup).
// commandID makes repeated hangups for the same call idempotent at Telnyx.
func (c *Client) Hangup(ctx context.Context, callControlID, commandID string) error {
	if callControlID == "" || commandID == "" {
		return fmt.Errorf("%w: call_control_id and command_id required", ErrInvalidDial)
	}
	body, err := json.Marshal(map[string]string{"command_id": commandID})
	if err != nil {
		return err
	}
	_, err = c.do(ctx, http.MethodPost, "/calls/"+url.PathEscape(callControlID)+"/actions/hangup", body)
	return err
}

func (c *Client) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("telnyx: %s %s: transport error", method, path)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read body", ErrBadResponse)
	}
	if len(raw) > maxResponseBody {
		return nil, fmt.Errorf("%w: body too large", ErrBadResponse)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		p, _, _ := strings.Cut(path, "?")
		return nil, &APIError{Method: method, Path: p, Status: resp.StatusCode}
	}
	return raw, nil
}

// CallControlApplication is the subset of a Telnyx Call Control application
// that validation reads.
type CallControlApplication struct {
	ID     string
	Active bool
}

// PhoneNumber is the subset of a Telnyx phone number record that validation
// reads.
type PhoneNumber struct {
	ID           string
	PhoneNumber  string
	Status       string
	ConnectionID string
}

type applicationResponse struct {
	Data *struct {
		ID     string `json:"id"`
		Active *bool  `json:"active"`
	} `json:"data"`
}

// GetCallControlApplication reads one application (GET
// /v2/call_control_applications/{id}). It is read-only and never dials. A 404
// is reported as ErrNotFound.
func (c *Client) GetCallControlApplication(ctx context.Context, id string) (CallControlApplication, error) {
	if id == "" {
		return CallControlApplication{}, fmt.Errorf("%w: application id required", ErrInvalidClient)
	}
	raw, err := c.do(ctx, http.MethodGet, "/call_control_applications/"+url.PathEscape(id), nil)
	if err != nil {
		return CallControlApplication{}, mapNotFound(err)
	}
	var resp applicationResponse
	if err := json.Unmarshal(raw, &resp); err != nil || resp.Data == nil || resp.Data.ID == "" {
		return CallControlApplication{}, fmt.Errorf("%w: application response", ErrBadResponse)
	}
	active := true
	if resp.Data.Active != nil {
		active = *resp.Data.Active
	}
	return CallControlApplication{ID: resp.Data.ID, Active: active}, nil
}

type phoneNumbersResponse struct {
	Data []struct {
		ID           string `json:"id"`
		PhoneNumber  string `json:"phone_number"`
		Status       string `json:"status"`
		ConnectionID string `json:"connection_id"`
	} `json:"data"`
}

// FindPhoneNumber looks up one owned number by its E.164 value (GET
// /v2/phone_numbers?filter[phone_number]=...). It is read-only. Only an exact
// match is returned; no match is ErrNotFound.
func (c *Client) FindPhoneNumber(ctx context.Context, e164 string) (PhoneNumber, error) {
	if e164 == "" {
		return PhoneNumber{}, fmt.Errorf("%w: phone number required", ErrInvalidClient)
	}
	q := url.Values{"filter[phone_number]": {e164}}
	raw, err := c.do(ctx, http.MethodGet, "/phone_numbers?"+q.Encode(), nil)
	if err != nil {
		return PhoneNumber{}, mapNotFound(err)
	}
	var resp phoneNumbersResponse
	if err := json.Unmarshal(raw, &resp); err != nil || resp.Data == nil {
		return PhoneNumber{}, fmt.Errorf("%w: phone number response", ErrBadResponse)
	}
	for _, n := range resp.Data {
		if n.PhoneNumber == e164 {
			return PhoneNumber{ID: n.ID, PhoneNumber: n.PhoneNumber, Status: n.Status, ConnectionID: n.ConnectionID}, nil
		}
	}
	return PhoneNumber{}, ErrNotFound
}

// mapNotFound turns an HTTP 404 into ErrNotFound. Every other error is
// returned unchanged.
func mapNotFound(err error) error {
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		return ErrNotFound
	}
	return err
}
