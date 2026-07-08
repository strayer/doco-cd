package notification

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/net/http/httpguts"
)

const (
	// webhookUserAgent is the User-Agent header sent with webhook notification requests.
	webhookUserAgent = "doco-cd"

	// webhookRequestTimeout bounds one request to the webhook endpoint. Notifications
	// are sent from the deployment path, so an unresponsive endpoint must not stall it.
	webhookRequestTimeout = 10 * time.Second
)

// webhookClient sends the requests to the webhook endpoint.
var webhookClient = &http.Client{Timeout: webhookRequestTimeout}

var (
	// ErrWebhookNotifyFailed is returned when the request to the notification webhook endpoint fails.
	ErrWebhookNotifyFailed = errors.New("request to notification webhook failed")
	// ErrInvalidWebhookHeader is returned when a webhook header entry is malformed.
	ErrInvalidWebhookHeader = errors.New("invalid webhook header")
	// ErrInvalidWebhookLevel is returned when the webhook notification level is not recognized.
	ErrInvalidWebhookLevel = errors.New("invalid webhook notification level, must be one of info, success, warning, failure")
)

// webhookReconciliation holds reconciliation-specific metadata of a webhook notification payload.
type webhookReconciliation struct {
	Event     string `json:"event"`
	ActorKind string `json:"actor_kind,omitempty"`
	ActorID   string `json:"actor_id,omitempty"`
	ActorName string `json:"actor_name,omitempty"`
}

// webhookCommit holds one deployed commit of a webhook notification payload.
type webhookCommit struct {
	Hash      string `json:"hash"`
	ShortHash string `json:"short_hash"`
	Subject   string `json:"subject"`
	Author    string `json:"author"`
}

// webhookMetadata holds the metadata object of a webhook notification payload.
type webhookMetadata struct {
	Repository      string                 `json:"repository,omitempty"`
	Stack           string                 `json:"stack,omitempty"`
	Context         string                 `json:"context,omitempty"`
	Target          string                 `json:"target,omitempty"`
	Revision        string                 `json:"revision,omitempty"`
	JobID           string                 `json:"job_id,omitempty"`
	TraceID         string                 `json:"trace_id,omitempty"`
	Commits         []webhookCommit        `json:"commits,omitempty"`
	DurationSeconds float64                `json:"duration_seconds,omitempty"`
	ChangedServices []string               `json:"changed_services,omitempty"`
	Reconciliation  *webhookReconciliation `json:"reconciliation,omitempty"`
}

// webhookPayload represents the JSON body sent to the notification webhook endpoint.
type webhookPayload struct {
	Level    string          `json:"level"`
	Title    string          `json:"title"`
	Message  string          `json:"message"`
	Metadata webhookMetadata `json:"metadata"`
}

// parseWebhookHeaders parses comma- or newline-separated "Name: value" pairs into an http.Header.
func parseWebhookHeaders(raw string) (http.Header, error) {
	entries := strings.FieldsFunc(raw, func(r rune) bool {
		return r == '\n' || r == ','
	})

	var headers http.Header

	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		// Header values may contain secrets, so error messages must not echo them.
		name, value, found := strings.Cut(entry, ":")
		if !found {
			return nil, fmt.Errorf("%w: expected 'Name: value' format", ErrInvalidWebhookHeader)
		}

		name = strings.TrimSpace(name)
		if !httpguts.ValidHeaderFieldName(name) {
			return nil, fmt.Errorf("%w: invalid header name: %q", ErrInvalidWebhookHeader, name)
		}

		value = strings.TrimSpace(value)
		if !httpguts.ValidHeaderFieldValue(value) {
			return nil, fmt.Errorf("%w: invalid value for header %q", ErrInvalidWebhookHeader, name)
		}

		if headers == nil {
			headers = http.Header{}
		}

		headers.Set(name, value)
	}

	return headers, nil
}

// parseWebhookLevel converts the configured webhook notify level to the level
// type. An empty level means Info, any other unknown level is an error so a
// typo cannot silently send everything.
func parseWebhookLevel(name string) (Level, error) {
	if name == "" {
		return Info, nil
	}

	parsed, ok := parseLevelStrict(name)
	if !ok {
		return Info, fmt.Errorf("%w: %q", ErrInvalidWebhookLevel, name)
	}

	return parsed, nil
}

// webhookConfigured reports whether the webhook backend is set up.
func (n *Notifier) webhookConfigured() bool {
	return n != nil && n.webhookURL != ""
}

// webhookEnabled reports whether the webhook backend delivers notifications of level.
func (n *Notifier) webhookEnabled(level Level) bool {
	return n.webhookConfigured() && level >= n.webhookLevel
}

// newWebhookPayload builds the webhook notification payload from the given notification parameters.
func newWebhookPayload(lvl Level, title, message string, m Metadata) webhookPayload {
	payload := webhookPayload{
		Level:   logLevels[lvl],
		Title:   strings.TrimSpace(title),
		Message: strings.TrimRight(message, "\n"),
		Metadata: webhookMetadata{
			Repository:      m.Repository,
			Stack:           m.Stack,
			Context:         m.Context,
			Target:          m.Target,
			Revision:        m.Revision,
			JobID:           m.JobID,
			TraceID:         m.TraceID,
			DurationSeconds: m.Duration.Seconds(),
			ChangedServices: m.ChangedServices,
		},
	}

	for _, commit := range m.Commits {
		payload.Metadata.Commits = append(payload.Metadata.Commits, webhookCommit{
			Hash:      commit.Hash,
			ShortHash: commit.ShortHash,
			Subject:   commit.Subject,
			Author:    commit.Author,
		})
	}

	if strings.TrimSpace(m.ReconciliationEvent) != "" {
		payload.Metadata.Reconciliation = &webhookReconciliation{
			Event:     m.ReconciliationEvent,
			ActorKind: strings.TrimSpace(strings.ToLower(m.AffectedActorKind)),
			ActorID:   m.AffectedActorID,
			ActorName: m.AffectedActorName,
		}
	}

	return payload
}

// sendWebhook sends a notification as structured JSON to this notifier's webhook endpoint.
func (n *Notifier) sendWebhook(lvl Level, title, message string, metadata Metadata) error {
	return postWebhook(n.webhookURL, n.webhookHeaders, newWebhookPayload(lvl, title, message, metadata))
}

// postWebhook marshals the payload and sends it to the given webhook URL.
func postWebhook(url string, headers http.Header, payload webhookPayload) error {
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal webhookPayload: %w", err)
	}

	// Send has no context, like the Apprise request; the client timeout bounds the request.
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(jsonData)) // #nosec G107
	if err != nil {
		return fmt.Errorf("failed to create webhook request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", webhookUserAgent)

	for name := range headers {
		req.Header.Set(name, headers.Get(name))
	}

	resp, err := webhookClient.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrWebhookNotifyFailed, err)
	}

	defer func() {
		// Drain the body so the underlying transport can safely reuse the connection.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("%w: unexpected status: %s", ErrWebhookNotifyFailed, resp.Status)
	}

	return nil
}
