package ingest

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestWebhookHandler_PROpened(t *testing.T) {
	secret := []byte("test-secret")
	var received PREvent

	handler := &WebhookHandler{
		OnPREvent: func(_ context.Context, event PREvent) error {
			received = event
			return nil
		},
		Secret: secret,
		Logger: zap.NewNop(),
	}

	payload := makePRPayload("opened")
	req := newSignedRequest(t, payload, secret)
	req.Header.Set("X-GitHub-Event", "pull_request")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusAccepted, rr.Code)
	assert.Equal(t, "ashita-ai/mimir", received.RepoFullName)
	assert.Equal(t, 42, received.PRNumber)
	assert.Equal(t, "abc123", received.HeadSHA)
	assert.Equal(t, "opened", received.Action)
}

func TestWebhookHandler_PRSynchronize(t *testing.T) {
	secret := []byte("test-secret")
	var called bool

	handler := &WebhookHandler{
		OnPREvent: func(_ context.Context, event PREvent) error {
			called = true
			return nil
		},
		Secret: secret,
		Logger: zap.NewNop(),
	}

	payload := makePRPayload("synchronize")
	req := newSignedRequest(t, payload, secret)
	req.Header.Set("X-GitHub-Event", "pull_request")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusAccepted, rr.Code)
	assert.True(t, called)
}

func TestWebhookHandler_IgnoredAction(t *testing.T) {
	secret := []byte("test-secret")
	var called bool

	handler := &WebhookHandler{
		OnPREvent: func(_ context.Context, event PREvent) error {
			called = true
			return nil
		},
		Secret: secret,
		Logger: zap.NewNop(),
	}

	payload := makePRPayload("closed")
	req := newSignedRequest(t, payload, secret)
	req.Header.Set("X-GitHub-Event", "pull_request")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
	assert.False(t, called, "closed action should be ignored")
}

func TestWebhookHandler_IgnoredEventType(t *testing.T) {
	secret := []byte("test-secret")
	var called bool

	handler := &WebhookHandler{
		OnPREvent: func(_ context.Context, event PREvent) error {
			called = true
			return nil
		},
		Secret: secret,
		Logger: zap.NewNop(),
	}

	payload := []byte(`{"action":"completed"}`)
	req := newSignedRequest(t, payload, secret)
	req.Header.Set("X-GitHub-Event", "check_suite")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
	assert.False(t, called, "non-PR events should be ignored")
}

func TestWebhookHandler_BadSignature(t *testing.T) {
	handler := &WebhookHandler{
		OnPREvent: func(_ context.Context, event PREvent) error { return nil },
		Secret:    []byte("correct-secret"),
		Logger:    zap.NewNop(),
	}

	payload := makePRPayload("opened")
	req := newSignedRequest(t, payload, []byte("wrong-secret"))
	req.Header.Set("X-GitHub-Event", "pull_request")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusUnauthorized, rr.Code)
}

func TestWebhookHandler_MethodNotAllowed(t *testing.T) {
	handler := &WebhookHandler{
		OnPREvent: func(_ context.Context, event PREvent) error { return nil },
		Logger:    zap.NewNop(),
	}

	req := httptest.NewRequest(http.MethodGet, "/webhooks/github", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusMethodNotAllowed, rr.Code)
}

// --- helpers ---

func makePRPayload(action string) []byte {
	payload := map[string]any{
		"action": action,
		"pull_request": map[string]any{
			"id":     int64(99999),
			"number": 42,
			"state":  "open",
			"head":   map[string]any{"sha": "abc123"},
			"base":   map[string]any{"sha": "def456"},
			"user":   map[string]any{"login": "testuser"},
		},
		"repository": map[string]any{
			"full_name": "ashita-ai/mimir",
		},
	}
	b, _ := json.Marshal(payload)
	return b
}

func newSignedRequest(t *testing.T, payload, secret []byte) *http.Request {
	t.Helper()
	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", sig)
	return req
}

// Verify PREvent is constructed correctly from payload.
func TestWebhookHandler_PREventFields(t *testing.T) {
	secret := []byte("s")
	var got PREvent

	handler := &WebhookHandler{
		OnPREvent: func(_ context.Context, event PREvent) error { got = event; return nil },
		Secret:    secret,
		Logger:    zap.NewNop(),
	}

	payload := makePRPayload("opened")
	req := newSignedRequest(t, payload, secret)
	req.Header.Set("X-GitHub-Event", "pull_request")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	require.Equal(t, http.StatusAccepted, rr.Code)
	assert.Equal(t, "ashita-ai/mimir", got.RepoFullName)
	assert.Equal(t, 42, got.PRNumber)
	assert.Equal(t, int64(99999), got.GitHubPRID)
	assert.Equal(t, "abc123", got.HeadSHA)
	assert.Equal(t, "def456", got.BaseSHA)
	assert.Equal(t, "testuser", got.Author)
	assert.Equal(t, "opened", got.Action)
}
