// Package twilio is the small slice of Twilio that dialout needs: the Calls REST resource,
// request-signature validation, inline TwiML and the Media Streams WebSocket messages.
// It uses only the standard library.
package twilio

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is the US1 REST endpoint.
const DefaultBaseURL = "https://api.twilio.com"

// Client calls the Twilio REST API with a Standard API key.
type Client struct {
	AccountSID string // AC…
	KeySID     string // SK…
	KeySecret  string
	BaseURL    string // DefaultBaseURL when empty
	HTTP       *http.Client
}

// Call is the subset of the Call resource dialout uses.
type Call struct {
	SID       string `json:"sid"`
	Status    string `json:"status"` // queued, ringing, in-progress, completed, busy, failed, no-answer, canceled
	Duration  string `json:"duration"`
	Price     string `json:"price"`
	PriceUnit string `json:"price_unit"`
	StartTime string `json:"start_time"`
	EndTime   string `json:"end_time"`
	To        string `json:"to"`
	From      string `json:"from"`
}

// Final reports whether the call is in a terminal state.
func (c Call) Final() bool {
	switch c.Status {
	case "completed", "busy", "failed", "no-answer", "canceled":
		return true
	}
	return false
}

// Error is a Twilio REST error body.
type Error struct {
	HTTPStatus int    `json:"-"`
	Code       int    `json:"code"`
	Message    string `json:"message"`
	MoreInfo   string `json:"more_info"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("twilio: HTTP %d, code %d: %s", e.HTTPStatus, e.Code, e.Message)
}

// CreateCallParams are the parameters dialout sets when placing a call.
type CreateCallParams struct {
	To, From  string
	TwiML     string        // inline TwiML, so Twilio needs no webhook to fetch it
	Timeout   time.Duration // ring time before no-answer
	TimeLimit time.Duration // hard cap on the call, a backstop for dialout's own limit
}

func (c *Client) base() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return DefaultBaseURL
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (c *Client) callsURL(sid string) string {
	u := c.base() + "/2010-04-01/Accounts/" + url.PathEscape(c.AccountSID) + "/Calls"
	if sid != "" {
		u += "/" + url.PathEscape(sid)
	}
	return u + ".json"
}

func (c *Client) do(ctx context.Context, method, u string, form url.Values) (*Call, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.KeySID, c.KeySecret)
	req.Header.Set("Accept", "application/json")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		e := &Error{HTTPStatus: resp.StatusCode}
		if json.Unmarshal(data, e) != nil || e.Message == "" {
			e.Message = strings.TrimSpace(string(data))
		}
		return nil, e
	}
	var call Call
	if err := json.Unmarshal(data, &call); err != nil {
		return nil, fmt.Errorf("twilio: decoding call: %w", err)
	}
	return &call, nil
}

// CreateCall places an outbound call.
func (c *Client) CreateCall(ctx context.Context, p CreateCallParams) (*Call, error) {
	form := url.Values{}
	form.Set("To", p.To)
	form.Set("From", p.From)
	form.Set("Twiml", p.TwiML)
	if p.Timeout > 0 {
		form.Set("Timeout", strconv.Itoa(int(p.Timeout/time.Second)))
	}
	if p.TimeLimit > 0 {
		form.Set("TimeLimit", strconv.Itoa(int(p.TimeLimit/time.Second)))
	}
	return c.do(ctx, http.MethodPost, c.callsURL(""), form)
}

// FetchCall reads the current state of a call.
func (c *Client) FetchCall(ctx context.Context, sid string) (*Call, error) {
	return c.do(ctx, http.MethodGet, c.callsURL(sid), nil)
}

// EndCall hangs up a call. A call that hasn't been answered yet is canceled; an answered one is
// completed. Ending a call that is already over is not an error.
func (c *Client) EndCall(ctx context.Context, sid string) (*Call, error) {
	cur, err := c.FetchCall(ctx, sid)
	if err != nil {
		return nil, err
	}
	if cur.Final() {
		return cur, nil
	}
	status := "completed"
	if cur.Status == "queued" || cur.Status == "ringing" {
		status = "canceled"
	}
	form := url.Values{}
	form.Set("Status", status)
	return c.do(ctx, http.MethodPost, c.callsURL(sid), form)
}
