package notification

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/git"
)

func TestParseWebhookHeaders(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name        string
		raw         string
		expected    http.Header
		expectedErr error
	}{
		{
			name:     "Empty Input",
			raw:      "",
			expected: nil,
		},
		{
			name:     "Single Header",
			raw:      "Authorization: Bearer abc",
			expected: http.Header{"Authorization": []string{"Bearer abc"}},
		},
		{
			name:     "Comma Separated Headers",
			raw:      "X-Api-Key: secret,X-Source: doco-cd",
			expected: http.Header{"X-Api-Key": []string{"secret"}, "X-Source": []string{"doco-cd"}},
		},
		{
			name:     "Newline Separated Headers",
			raw:      "X-Api-Key: secret\nX-Source: doco-cd",
			expected: http.Header{"X-Api-Key": []string{"secret"}, "X-Source": []string{"doco-cd"}},
		},
		{
			name:     "Mixed Separators With Whitespace And Trailing Separator",
			raw:      " X-Api-Key: secret \nX-Source: doco-cd,",
			expected: http.Header{"X-Api-Key": []string{"secret"}, "X-Source": []string{"doco-cd"}},
		},
		{
			name:     "Whitespace-Only Entries Are Skipped",
			raw:      "X-Api-Key: secret, ,X-Source: doco-cd",
			expected: http.Header{"X-Api-Key": []string{"secret"}, "X-Source": []string{"doco-cd"}},
		},
		{
			name:     "Value Containing Colon",
			raw:      "X-A: b:c",
			expected: http.Header{"X-A": []string{"b:c"}},
		},
		{
			name:     "Empty Value",
			raw:      "X-Empty:",
			expected: http.Header{"X-Empty": []string{""}},
		},
		{
			name:     "Duplicate Header Names Keep Last Value",
			raw:      "X-Api-Key: first,X-Api-Key: second",
			expected: http.Header{"X-Api-Key": []string{"second"}},
		},
		{
			name:        "Missing Colon",
			raw:         "NoColon",
			expectedErr: ErrInvalidWebhookHeader,
		},
		{
			name:        "Empty Header Name",
			raw:         ": value",
			expectedErr: ErrInvalidWebhookHeader,
		},
		{
			name:        "Invalid Header Name",
			raw:         "Bad Name: value",
			expectedErr: ErrInvalidWebhookHeader,
		},
		{
			name:        "Invalid Header Value",
			raw:         "X-A: b\x00c",
			expectedErr: ErrInvalidWebhookHeader,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			headers, err := parseWebhookHeaders(tc.raw)
			if tc.expectedErr != nil {
				if !errors.Is(err, tc.expectedErr) {
					t.Fatalf("expected error wrapping %q, got: %v", tc.expectedErr, err)
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if len(headers) != len(tc.expected) {
				t.Fatalf("expected %d headers, got %d: %v", len(tc.expected), len(headers), headers)
			}

			for name := range tc.expected {
				if got := headers.Get(name); got != tc.expected.Get(name) {
					t.Errorf("expected header %q to be %q, got %q", name, tc.expected.Get(name), got)
				}
			}
		})
	}
}

func TestNewValidatesWebhookConfig(t *testing.T) {
	t.Parallel()

	t.Run("valid config is stored", func(t *testing.T) {
		t.Parallel()

		notifier, err := New(Config{WebhookURL: "http://example.com/hook", WebhookHeaders: "X-Api-Key: secret", WebhookLevel: "warning"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if notifier.webhookURL != "http://example.com/hook" {
			t.Errorf("expected url %q, got %q", "http://example.com/hook", notifier.webhookURL)
		}

		if got := notifier.webhookHeaders.Get("X-Api-Key"); got != "secret" {
			t.Errorf("expected header value %q, got %q", "secret", got)
		}

		if notifier.webhookLevel != Warning {
			t.Errorf("expected level %v, got %v", Warning, notifier.webhookLevel)
		}
	})

	t.Run("empty level defaults to info", func(t *testing.T) {
		t.Parallel()

		notifier, err := New(Config{WebhookURL: "http://example.com/hook"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if notifier.webhookLevel != Info {
			t.Errorf("expected level %v, got %v", Info, notifier.webhookLevel)
		}
	})

	t.Run("invalid headers return error", func(t *testing.T) {
		t.Parallel()

		if _, err := New(Config{WebhookURL: "http://example.com/hook", WebhookHeaders: "NoColon"}); !errors.Is(err, ErrInvalidWebhookHeader) {
			t.Fatalf("expected error wrapping %q, got: %v", ErrInvalidWebhookHeader, err)
		}
	})

	t.Run("unknown level is rejected", func(t *testing.T) {
		t.Parallel()

		if _, err := New(Config{WebhookURL: "http://example.com/hook", WebhookLevel: "bogus"}); !errors.Is(err, ErrInvalidWebhookLevel) {
			t.Fatalf("expected error wrapping %q, got: %v", ErrInvalidWebhookLevel, err)
		}
	})

	t.Run("headers and level are validated without a url", func(t *testing.T) {
		t.Parallel()

		if _, err := New(Config{WebhookHeaders: "NoColon"}); !errors.Is(err, ErrInvalidWebhookHeader) {
			t.Fatalf("expected error wrapping %q, got: %v", ErrInvalidWebhookHeader, err)
		}

		if _, err := New(Config{WebhookLevel: "bogus"}); !errors.Is(err, ErrInvalidWebhookLevel) {
			t.Fatalf("expected error wrapping %q, got: %v", ErrInvalidWebhookLevel, err)
		}
	})
}

// TestWebhookEnabled pins that Enabled considers both backends, so a level the
// webhook delivers is not skipped because Apprise would drop it.
func TestWebhookEnabled(t *testing.T) {
	t.Parallel()

	levels := []Level{Info, Success, Warning, Failure}

	testCases := []struct {
		name   string
		config Config
		want   []bool // indexed like levels
	}{
		{
			name:   "webhook only, info level",
			config: Config{WebhookURL: "http://hook.test", WebhookLevel: "info"},
			want:   []bool{true, true, true, true},
		},
		{
			name:   "webhook only, failure level",
			config: Config{WebhookURL: "http://hook.test", WebhookLevel: "failure"},
			want:   []bool{false, false, false, true},
		},
		{
			name:   "apprise failure and webhook success",
			config: Config{APIURL: "http://apprise.test", NotifyURLs: "apprise://x", NotifyLevel: "failure", WebhookURL: "http://hook.test", WebhookLevel: "success"},
			want:   []bool{false, true, true, true},
		},
		{
			name:   "apprise info and webhook failure",
			config: Config{APIURL: "http://apprise.test", NotifyURLs: "apprise://x", NotifyLevel: "info", WebhookURL: "http://hook.test", WebhookLevel: "failure"},
			want:   []bool{true, true, true, true},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			notifier, err := New(tc.config)
			if err != nil {
				t.Fatal(err)
			}

			for i, level := range levels {
				if got := notifier.Enabled(level); got != tc.want[i] {
					t.Errorf("Enabled(%s) = %v, want %v", logLevels[level], got, tc.want[i])
				}
			}
		})
	}
}

func TestNewWebhookPayload(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		level    Level
		title    string
		message  string
		metadata Metadata
		expected string
	}{
		{
			name:    "Full Metadata Without Reconciliation",
			level:   Success,
			title:   "Deployment completed",
			message: "Successfully deployed stack\n",
			metadata: Metadata{
				Repository: "github.com/my/repo",
				Stack:      "my-stack",
				Context:    "remote-vm",
				Target:     "prod",
				Revision:   "main (abc123)",
				JobID:      "job-1",
				Commits: []git.CommitInfo{
					{Hash: "abc123def456", ShortHash: "abc123", Subject: "fix: something", Author: "Jane Doe"},
				},
				Duration:        90500 * time.Millisecond,
				ChangedServices: []string{"web", "worker"},
			},
			expected: `{"level":"success","title":"Deployment completed","message":"Successfully deployed stack","metadata":{"repository":"github.com/my/repo","stack":"my-stack","context":"remote-vm","target":"prod","revision":"main (abc123)","job_id":"job-1","commits":[{"hash":"abc123def456","short_hash":"abc123","subject":"fix: something","author":"Jane Doe"}],"duration_seconds":90.5,"changed_services":["web","worker"]}}`,
		},
		{
			name:    "Reconciliation Metadata",
			level:   Failure,
			title:   "Restart failed",
			message: "Container restart failed",
			metadata: Metadata{
				Repository:          "github.com/my/repo",
				Stack:               "my-stack",
				TraceID:             "trace-123",
				ReconciliationEvent: "unhealthy",
				AffectedActorKind:   " Container ",
				AffectedActorID:     "abc123",
				AffectedActorName:   "web_1",
			},
			expected: `{"level":"failure","title":"Restart failed","message":"Container restart failed","metadata":{"repository":"github.com/my/repo","stack":"my-stack","trace_id":"trace-123","reconciliation":{"event":"unhealthy","actor_kind":"container","actor_id":"abc123","actor_name":"web_1"}}}`,
		},
		{
			name:     "Whitespace Reconciliation Event Is Ignored",
			level:    Warning,
			title:    "Service restarted",
			message:  "Service was restarted",
			metadata: Metadata{ReconciliationEvent: "   "},
			expected: `{"level":"warning","title":"Service restarted","message":"Service was restarted","metadata":{}}`,
		},
		{
			name:     "Empty Metadata Omits Fields",
			level:    Info,
			title:    " Update available ",
			message:  "New version available\n\n",
			metadata: Metadata{},
			expected: `{"level":"info","title":"Update available","message":"New version available","metadata":{}}`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			payload := newWebhookPayload(tc.level, tc.title, tc.message, tc.metadata)

			jsonData, err := json.Marshal(payload)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if string(jsonData) != tc.expected {
				t.Errorf("expected %s, got %s", tc.expected, jsonData)
			}
		})
	}
}

func TestSendWebhookNotification(t *testing.T) {
	t.Parallel()

	metadata := Metadata{
		Repository: "github.com/my/repo",
		Stack:      "my-stack",
		Revision:   "main (abc123)",
		JobID:      "job-1",
	}

	newNotifier := func(t *testing.T, url, headers, level string) *Notifier {
		t.Helper()

		notifier, err := New(Config{WebhookURL: url, WebhookHeaders: headers, WebhookLevel: level})
		if err != nil {
			t.Fatalf("failed to create notifier: %v", err)
		}

		return notifier
	}

	t.Run("successful request contains expected headers and body", func(t *testing.T) {
		t.Parallel()

		var (
			requestCount int
			gotMethod    string
			gotHeaders   http.Header
			gotBody      []byte
		)

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestCount++
			gotMethod = r.Method
			gotHeaders = r.Header.Clone()
			gotBody, _ = io.ReadAll(r.Body)

			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		notifier := newNotifier(t, server.URL, "X-Api-Key: secret", "info")

		if err := notifier.Send(Success, "Deployment completed", "Successfully deployed stack", metadata); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if requestCount != 1 {
			t.Fatalf("expected 1 request, got %d", requestCount)
		}

		if gotMethod != http.MethodPost {
			t.Errorf("expected method %q, got %q", http.MethodPost, gotMethod)
		}

		if got := gotHeaders.Get("Content-Type"); got != "application/json" {
			t.Errorf("expected Content-Type %q, got %q", "application/json", got)
		}

		if got := gotHeaders.Get("User-Agent"); got != webhookUserAgent {
			t.Errorf("expected User-Agent %q, got %q", webhookUserAgent, got)
		}

		if got := gotHeaders.Get("X-Api-Key"); got != "secret" {
			t.Errorf("expected X-Api-Key %q, got %q", "secret", got)
		}

		expectedBody := `{"level":"success","title":"Deployment completed","message":"Successfully deployed stack","metadata":{"repository":"github.com/my/repo","stack":"my-stack","revision":"main (abc123)","job_id":"job-1"}}`
		if string(gotBody) != expectedBody {
			t.Errorf("expected body %s, got %s", expectedBody, gotBody)
		}
	})

	t.Run("no content status is accepted", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))
		defer server.Close()

		notifier := newNotifier(t, server.URL, "", "info")

		if err := notifier.Send(Success, "Deployment completed", "Successfully deployed stack", metadata); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("server error returns ErrWebhookNotifyFailed", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer server.Close()

		notifier := newNotifier(t, server.URL, "", "info")

		err := notifier.Send(Success, "Deployment completed", "Successfully deployed stack", metadata)
		if !errors.Is(err, ErrWebhookNotifyFailed) {
			t.Fatalf("expected error wrapping %q, got: %v", ErrWebhookNotifyFailed, err)
		}
	})

	t.Run("connection error returns ErrWebhookNotifyFailed", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		server.Close()

		notifier := newNotifier(t, server.URL, "", "info")

		err := notifier.Send(Success, "Deployment completed", "Successfully deployed stack", metadata)
		if !errors.Is(err, ErrWebhookNotifyFailed) {
			t.Fatalf("expected error wrapping %q, got: %v", ErrWebhookNotifyFailed, err)
		}
	})

	t.Run("notification below configured level is not sent", func(t *testing.T) {
		t.Parallel()

		var requestCount atomic.Int32

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requestCount.Add(1)

			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		notifier := newNotifier(t, server.URL, "", "failure")

		if err := notifier.Send(Info, "Update available", "New version available", metadata); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if requestCount.Load() != 0 {
			t.Errorf("expected 0 requests, got %d", requestCount.Load())
		}
	})

	t.Run("empty url is a no-op", func(t *testing.T) {
		t.Parallel()

		notifier := newNotifier(t, "", "", "success")

		if err := notifier.Send(Success, "Deployment completed", "Successfully deployed stack", metadata); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

// TestSendWebhookRetriesFailureAfterDeliveryError mirrors the Apprise behaviour:
// a failure the webhook could not deliver is not suppressed on the next attempt.
func TestSendWebhookRetriesFailureAfterDeliveryError(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)

			return
		}

		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	notifier, err := New(Config{WebhookURL: server.URL, WebhookLevel: "info", FailureRepeatInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	metadata := Metadata{Repository: "acme/repo", Stack: "app"}
	if err := notifier.Send(Failure, "Deployment Failed", "pull access denied", metadata); err == nil {
		t.Fatal("first delivery should fail")
	}

	if err := notifier.Send(Failure, "Deployment Failed", "pull access denied", metadata); err != nil {
		t.Fatalf("retry delivery failed: %v", err)
	}

	if err := notifier.Send(Failure, "Deployment Failed", "pull access denied", metadata); err != nil {
		t.Fatalf("repeated failure should be suppressed: %v", err)
	}

	if requests.Load() != 2 {
		t.Fatalf("failure delivery attempts = %d, want 2", requests.Load())
	}
}

func TestPostWebhook(t *testing.T) {
	t.Parallel()

	t.Run("invalid url returns request creation error", func(t *testing.T) {
		t.Parallel()

		err := postWebhook("://invalid", nil, webhookPayload{})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})
}

func TestSendFanOut(t *testing.T) {
	t.Parallel()

	newServer := func(statusCode int, requestCount *atomic.Int64) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requestCount.Add(1)

			w.WriteHeader(statusCode)
		}))
	}

	testCases := []struct {
		name              string
		appriseStatusCode int
		webhookStatusCode int
		expectAppriseErr  bool
		expectWebhookErr  bool
	}{
		{
			name:              "Both Backends Succeed",
			appriseStatusCode: http.StatusOK,
			webhookStatusCode: http.StatusOK,
		},
		{
			name:              "Apprise Fails And Webhook Succeeds",
			appriseStatusCode: http.StatusFailedDependency,
			webhookStatusCode: http.StatusOK,
			expectAppriseErr:  true,
		},
		{
			name:              "Apprise Succeeds And Webhook Fails",
			appriseStatusCode: http.StatusOK,
			webhookStatusCode: http.StatusInternalServerError,
			expectWebhookErr:  true,
		},
		{
			name:              "Both Backends Fail",
			appriseStatusCode: http.StatusFailedDependency,
			webhookStatusCode: http.StatusInternalServerError,
			expectAppriseErr:  true,
			expectWebhookErr:  true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var appriseRequests, webhookRequests atomic.Int64

			appriseServer := newServer(tc.appriseStatusCode, &appriseRequests)
			defer appriseServer.Close()

			webhookServer := newServer(tc.webhookStatusCode, &webhookRequests)
			defer webhookServer.Close()

			notifier, err := New(Config{
				APIURL:       appriseServer.URL + "/notify",
				NotifyURLs:   "apprise://example.test",
				NotifyLevel:  "info",
				WebhookURL:   webhookServer.URL,
				WebhookLevel: "info",
			})
			if err != nil {
				t.Fatalf("failed to create notifier: %v", err)
			}

			err = notifier.Send(Success, "Deployment completed", "Successfully deployed stack", Metadata{Stack: "my-stack"})

			// Both backends must be attempted regardless of the other backend's outcome.
			if appriseRequests.Load() != 1 {
				t.Errorf("expected 1 apprise request, got %d", appriseRequests.Load())
			}

			if webhookRequests.Load() != 1 {
				t.Errorf("expected 1 webhook request, got %d", webhookRequests.Load())
			}

			if !tc.expectAppriseErr && !tc.expectWebhookErr {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}

				return
			}

			if errors.Is(err, ErrNotifyFailed) != tc.expectAppriseErr {
				t.Errorf("expected apprise error: %v, got: %v", tc.expectAppriseErr, err)
			}

			if errors.Is(err, ErrWebhookNotifyFailed) != tc.expectWebhookErr {
				t.Errorf("expected webhook error: %v, got: %v", tc.expectWebhookErr, err)
			}
		})
	}
}

// TestSendFanOutKeepsFailureSuppressedWhenOneBackendDelivers pins that a failure
// delivered by one backend is not repeated just because the other backend failed.
func TestSendFanOutKeepsFailureSuppressedWhenOneBackendDelivers(t *testing.T) {
	t.Parallel()

	var appriseRequests, webhookRequests atomic.Int32

	appriseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		appriseRequests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer appriseServer.Close()

	webhookServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		webhookRequests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer webhookServer.Close()

	notifier, err := New(Config{
		APIURL:                appriseServer.URL + "/notify",
		NotifyURLs:            "apprise://example.test",
		NotifyLevel:           "info",
		WebhookURL:            webhookServer.URL,
		WebhookLevel:          "info",
		FailureRepeatInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}

	metadata := Metadata{Repository: "acme/repo", Stack: "app"}

	err = notifier.Send(Failure, "Deployment Failed", "pull access denied", metadata)
	if !errors.Is(err, ErrWebhookNotifyFailed) {
		t.Fatalf("first delivery error = %v, want ErrWebhookNotifyFailed", err)
	}

	if err := notifier.Send(Failure, "Deployment Failed", "pull access denied", metadata); err != nil {
		t.Fatalf("repeated failure should be suppressed: %v", err)
	}

	if appriseRequests.Load() != 1 || webhookRequests.Load() != 1 {
		t.Fatalf("delivery attempts apprise=%d webhook=%d, want 1 each", appriseRequests.Load(), webhookRequests.Load())
	}
}
