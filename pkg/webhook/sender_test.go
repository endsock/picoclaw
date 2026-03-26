package webhook

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSendSuccessAndHeaders(t *testing.T) {
	var gotHeader http.Header
	var gotPayload Payload
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Clone()
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotPayload)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	err := Send(context.Background(), SendRequest{
		URL:    server.URL,
		Event:  "completed",
		TaskID: "subagent-1",
		Headers: map[string]string{
			"X-Custom": "abc",
		},
		Payload: Payload{
			Event:     "completed",
			Status:    "completed",
			TaskID:    "subagent-1",
			ModelName: "claude-sonnet-4-6",
			Result:    "done",
		},
		Timeout:         time.Second,
		MaxRetries:      1,
		MaxPayloadBytes: 1024,
	})
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}
	if gotHeader.Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type = %q", gotHeader.Get("Content-Type"))
	}
	if gotHeader.Get("User-Agent") != "picoclaw-webhook/1.0" {
		t.Fatalf("User-Agent = %q", gotHeader.Get("User-Agent"))
	}
	if gotHeader.Get("X-PicoClaw-Event") != "completed" {
		t.Fatalf("X-PicoClaw-Event = %q", gotHeader.Get("X-PicoClaw-Event"))
	}
	if gotHeader.Get("X-PicoClaw-Task-ID") != "subagent-1" {
		t.Fatalf("X-PicoClaw-Task-ID = %q", gotHeader.Get("X-PicoClaw-Task-ID"))
	}
	if gotHeader.Get("X-Custom") != "abc" {
		t.Fatalf("X-Custom = %q", gotHeader.Get("X-Custom"))
	}
	if gotPayload.TaskID != "subagent-1" || gotPayload.Result != "done" {
		t.Fatalf("payload = %+v", gotPayload)
	}
}

func TestSendRetriesOn5xxAnd429(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	err := Send(context.Background(), SendRequest{
		URL:        server.URL,
		Event:      "completed",
		TaskID:     "subagent-2",
		Payload:    Payload{Event: "completed", Status: "completed", TaskID: "subagent-2", ModelName: "model"},
		Timeout:    10 * time.Second,
		MaxRetries: 3,
	})
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}

func TestSendDoesNotRetryOn4xx(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()

	err := Send(context.Background(), SendRequest{
		URL:        server.URL,
		Event:      "failed",
		TaskID:     "subagent-3",
		Payload:    Payload{Event: "failed", Status: "failed", TaskID: "subagent-3", ModelName: "model"},
		Timeout:    time.Second,
		MaxRetries: 3,
	})
	if err == nil {
		t.Fatal("expected error for 400 response")
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}

func TestTruncatePayload(t *testing.T) {
	payload := truncatePayload(Payload{
		Event:     "completed",
		Status:    "completed",
		TaskID:    "subagent-4",
		ModelName: "model",
		Result:    "这是一个很长的结果" + string(make([]byte, 200)),
	}, 120)
	if !payload.ResultTruncated {
		t.Fatal("expected payload to be truncated")
	}
	if payload.ResultSize == 0 {
		t.Fatal("expected original result size")
	}
	if len([]byte(payload.Result)) >= payload.ResultSize {
		t.Fatalf("expected truncated result, got len=%d size=%d", len([]byte(payload.Result)), payload.ResultSize)
	}
}
