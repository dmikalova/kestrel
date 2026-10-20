// Package discord posts kestrel's alarms to a Discord webhook.
//
// This mirrors den's internal/notify so the two agree on how an alert looks.
package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Colors for the embed strip. Discord takes a decimal RGB integer.
const (
	colorRed   = 0xCC_33_33
	colorGreen = 0x33_99_55
)

const (
	requestTimeout = 10 * time.Second
	maxAttempts    = 3
)

// Client posts to one webhook URL.
type Client struct {
	webhook string
	http    *http.Client
}

// New returns a client for the given webhook URL.
func New(webhook string) (*Client, error) {
	if !strings.HasPrefix(webhook, "https://") {
		return nil, errors.New("the webhook must be an https URL")
	}

	return &Client{
		webhook: webhook,
		http:    &http.Client{Timeout: requestTimeout},
	}, nil
}

type field struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline"`
}

type embed struct {
	Title       string  `json:"title"`
	Description string  `json:"description"`
	Color       int     `json:"color"`
	Fields      []field `json:"fields,omitempty"`
	Timestamp   string  `json:"timestamp"`
}

type payload struct {
	Embeds []embed `json:"embeds"`
	// An alarm names a host and a job. Neither should ever ring a phone.
	AllowedMentions struct {
		Parse []string `json:"parse"`
	} `json:"allowed_mentions"`
}

// Down announces that a check missed its deadline.
func (c *Client) Down(ctx context.Context, name string, last *time.Time, deadline time.Time) error {
	seen := "never"
	if last != nil {
		seen = last.UTC().Format(time.RFC3339)
	}

	return c.send(ctx, embed{
		Title:       fmt.Sprintf("%s is down", name),
		Description: "The check missed its deadline. The job did not report success.",
		Color:       colorRed,
		Fields: []field{
			{Name: "Last ping", Value: seen, Inline: true},
			{Name: "Deadline", Value: deadline.UTC().Format(time.RFC3339), Inline: true},
		},
	})
}

// Up announces that a check that was down has pinged again.
func (c *Client) Up(ctx context.Context, name string) error {
	return c.send(ctx, embed{
		Title:       fmt.Sprintf("%s recovered", name),
		Description: "The check pinged again.",
		Color:       colorGreen,
	})
}

func (c *Client) send(ctx context.Context, e embed) error {
	e.Timestamp = time.Now().UTC().Format(time.RFC3339)

	var p payload
	p.Embeds = []embed{e}
	p.AllowedMentions.Parse = []string{}

	body, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("encode the Discord payload: %w", err)
	}

	var lastErr error

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			// Linear backoff. Discord's own 429 retry_after is finer grained,
			// but an alarm that is a few seconds late is still an alarm.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt-1) * time.Second):
			}
		}

		retryable, err := c.post(ctx, body)
		if err == nil {
			return nil
		}

		lastErr = err

		if !retryable {
			return err
		}
	}

	return fmt.Errorf("post to Discord after %d attempts: %w", maxAttempts, lastErr)
}

// post reports whether the failure is worth retrying.
func (c *Client) post(ctx context.Context, body []byte) (retryable bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.webhook, bytes.NewReader(body))
	if err != nil {
		return false, fmt.Errorf("build the Discord request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return true, fmt.Errorf("send the Discord request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	_, _ = io.Copy(io.Discard, resp.Body)

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return false, nil
	case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode >= 500:
		return true, fmt.Errorf("discord returned %s", resp.Status)
	default:
		return false, fmt.Errorf("discord returned %s", resp.Status)
	}
}
