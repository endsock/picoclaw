package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sipeed/picoclaw/pkg/utils"
)

type Payload struct {
	Event           string            `json:"event"`
	Status          string            `json:"status"`
	TaskID          string            `json:"task_id"`
	Label           string            `json:"label,omitempty"`
	AgentID         string            `json:"agent_id,omitempty"`
	ModelName       string            `json:"model_name"`
	Source          string            `json:"source,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`
	Result          string            `json:"result,omitempty"`
	Error           string            `json:"error,omitempty"`
	CreatedAtMS     int64             `json:"created_at_ms"`
	FinishedAtMS    int64             `json:"finished_at_ms"`
	DurationMS      int64             `json:"duration_ms"`
	ResultTruncated bool              `json:"result_truncated,omitempty"`
	ResultSize      int               `json:"result_size,omitempty"`
}

type SendRequest struct {
	URL             string
	Headers         map[string]string
	Event           string
	TaskID          string
	Payload         Payload
	Timeout         time.Duration
	MaxRetries      int
	MaxPayloadBytes int
}

func Send(ctx context.Context, req SendRequest) error {
	payload := truncatePayload(req.Payload, req.MaxPayloadBytes)
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal webhook payload: %w", err)
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(requestCtx, http.MethodPost, req.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build webhook request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", "picoclaw-webhook/1.0")
	if req.Event != "" {
		httpReq.Header.Set("X-PicoClaw-Event", req.Event)
	}
	if req.TaskID != "" {
		httpReq.Header.Set("X-PicoClaw-Task-ID", req.TaskID)
	}
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}
	if httpReq.GetBody == nil {
		payloadBytes := append([]byte(nil), body...)
		httpReq.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(payloadBytes)), nil
		}
	}

	client := &http.Client{Timeout: timeout}
	resp, err := utils.DoRequestWithRetryMaxRetries(client, httpReq, req.MaxRetries)
	if err != nil {
		return fmt.Errorf("send webhook: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned status %d", resp.StatusCode)
	}
	return nil
}

func truncatePayload(payload Payload, maxPayloadBytes int) Payload {
	payload.ResultSize = len([]byte(payload.Result))
	if maxPayloadBytes <= 0 {
		return payload
	}
	if estimatedPayloadSize(payload) <= maxPayloadBytes {
		return payload
	}
	if payload.Result == "" {
		return payload
	}

	truncated := payload
	truncated.ResultTruncated = true
	available := maxPayloadBytes - estimatedPayloadSizeWithoutResult(truncated)
	if available <= 0 {
		truncated.Result = ""
		return truncated
	}
	truncated.Result = truncateUTF8ByBytes(payload.Result, available)
	return truncated
}

func estimatedPayloadSize(payload Payload) int {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0
	}
	return len(body)
}

func estimatedPayloadSizeWithoutResult(payload Payload) int {
	payload.Result = ""
	body, err := json.Marshal(payload)
	if err != nil {
		return 0
	}
	return len(body)
}

func truncateUTF8ByBytes(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) == 0 {
		return ""
	}
	if len([]byte(s)) <= maxBytes {
		return s
	}
	const suffix = "..."
	suffixBytes := len([]byte(suffix))
	if maxBytes <= suffixBytes {
		return strings.Repeat(".", maxBytes)
	}
	limit := maxBytes - suffixBytes
	var b strings.Builder
	written := 0
	for _, r := range s {
		runeBytes := utf8.RuneLen(r)
		if runeBytes < 0 {
			continue
		}
		if written+runeBytes > limit {
			break
		}
		b.WriteRune(r)
		written += runeBytes
	}
	return b.String() + suffix
}
