package access

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// SiteverifyURL is Cloudflare's Turnstile verification endpoint.
const SiteverifyURL = "https://challenges.cloudflare.com/turnstile/v0/siteverify"

// Turnstile verifies Cloudflare Turnstile tokens server-side.
type Turnstile struct {
	secret string
	client *http.Client
	// Endpoint can be overridden (tests).
	Endpoint string
}

func NewTurnstile(secret string, client *http.Client) *Turnstile {
	if client == nil {
		client = http.DefaultClient
	}
	return &Turnstile{secret: secret, client: client, Endpoint: SiteverifyURL}
}

// Verify returns nil when Cloudflare accepts the token.
func (t *Turnstile) Verify(ctx context.Context, token, remoteIP string) error {
	if token == "" {
		return errors.New("missing token")
	}
	form := url.Values{"secret": {t.secret}, "response": {token}}
	if remoteIP != "" {
		form.Set("remoteip", remoteIP)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.Endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("siteverify request: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		Success    bool     `json:"success"`
		ErrorCodes []string `json:"error-codes"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil {
		return fmt.Errorf("siteverify response: %w", err)
	}
	if !out.Success {
		return fmt.Errorf("turnstile rejected: %s", strings.Join(out.ErrorCodes, ","))
	}
	return nil
}
