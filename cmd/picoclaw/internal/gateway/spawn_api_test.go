package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/agent"
	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/tools"
	"github.com/sipeed/picoclaw/pkg/workqueue"
)

type gatewayTestProvider struct{ fail bool }

func (p *gatewayTestProvider) Chat(ctx context.Context, messages []providers.Message, toolDefs []providers.ToolDefinition, model string, options map[string]any) (*providers.LLMResponse, error) {
	if p.fail {
		return nil, errors.New("boom")
	}
	content := "ok"
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			content = "Task completed: " + messages[i].Content
			break
		}
	}
	return &providers.LLMResponse{Content: content}, nil
}
func (p *gatewayTestProvider) GetDefaultModel() string { return "test-model" }
func (p *gatewayTestProvider) SupportsTools() bool     { return false }
func (p *gatewayTestProvider) GetContextWindow() int   { return 4096 }

func newTestAgentLoop(t *testing.T, provider providers.LLMProvider, queue *workqueue.Queue) *agent.AgentLoop {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	cfg.Gateway.SpawnAPI.Enabled = true
	cfg.Gateway.OutboundWebhook.MaxPayloadBytes = 64
	cfg.Agents.Defaults.ModelName = "claude-sonnet-4-6"
	return agent.NewAgentLoop(cfg, bus.NewMessageBus(), provider, queue)
}

func decodeJSONBody(t *testing.T, rr *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rr.Body.Bytes(), v); err != nil {
		t.Fatalf("decode response: %v body=%s", err, rr.Body.String())
	}
}

func TestSpawnAPI_PostAccepted(t *testing.T) {
	al := newTestAgentLoop(t, &gatewayTestProvider{}, nil)
	h := newSpawnCreateHandler(al)

	req := httptest.NewRequest(http.MethodPost, "/api/spawn", bytes.NewBufferString(`{"task":"analyze","label":"job","model_name":"claude-sonnet-4-6"}`))
	rr := httptest.NewRecorder()
	h(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var resp spawnAcceptedResponse
	decodeJSONBody(t, rr, &resp)
	if resp.TaskID == "" || resp.Status != "accepted" || resp.ResultURL == "" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestSpawnAPI_PostAcceptedPublishesFinalResultWhenChannelAndChatIDPresent(t *testing.T) {
	al := newTestAgentLoop(t, &gatewayTestProvider{}, nil)
	h := newSpawnCreateHandler(al)

	req := httptest.NewRequest(http.MethodPost, "/api/spawn", bytes.NewBufferString(`{"task":"analyze","label":"job","model_name":"claude-sonnet-4-6","channel":"telegram","chat_id":"chat-1"}`))
	rr := httptest.NewRecorder()
	h(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}

	var resp spawnAcceptedResponse
	decodeJSONBody(t, rr, &resp)
	if resp.TaskID == "" {
		t.Fatalf("unexpected response: %+v", resp)
	}

	task := waitGatewayTask(t, al, resp.TaskID, "completed")
	if task.OriginChannel != "telegram" || task.OriginChatID != "chat-1" {
		t.Fatalf("unexpected task origin: %+v", task)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	msg, ok := al.SubscribeOutbound(ctx)
	if !ok {
		t.Fatal("expected outbound message")
	}
	if msg.Channel != "telegram" || msg.ChatID != "chat-1" {
		t.Fatalf("unexpected outbound target: %+v", msg)
	}
	if msg.Content != task.Result {
		t.Fatalf("content = %q, want %q", msg.Content, task.Result)
	}
	if msg.Content == resp.Message {
		t.Fatalf("content should be final result, got accepted message %q", msg.Content)
	}
}

func TestSpawnAPI_PostDoesNotPublishFinalResultOnFailure(t *testing.T) {
	al := newTestAgentLoop(t, &gatewayTestProvider{fail: true}, nil)
	h := newSpawnCreateHandler(al)

	req := httptest.NewRequest(http.MethodPost, "/api/spawn", bytes.NewBufferString(`{"task":"analyze","model_name":"claude-sonnet-4-6","channel":"telegram","chat_id":"chat-1"}`))
	rr := httptest.NewRecorder()
	h(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}

	var resp spawnAcceptedResponse
	decodeJSONBody(t, rr, &resp)
	if resp.TaskID == "" {
		t.Fatalf("unexpected response: %+v", resp)
	}

	waitGatewayTask(t, al, resp.TaskID, "failed")

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if msg, ok := al.SubscribeOutbound(ctx); ok {
		t.Fatalf("unexpected outbound message: %+v", msg)
	}
}
func TestSpawnAPI_PostBadRequest(t *testing.T) {
	al := newTestAgentLoop(t, &gatewayTestProvider{}, nil)
	h := newSpawnCreateHandler(al)

	cases := []struct {
		name string
		body string
	}{
		{name: "invalid json", body: `{`},
		{name: "missing task", body: `{"model_name":"claude-sonnet-4-6"}`},
		{name: "invalid webhook", body: `{"task":"x","model_name":"claude-sonnet-4-6","webhook":{"url":"ftp://bad"}}`},
		{name: "channel without chat_id", body: `{"task":"x","model_name":"claude-sonnet-4-6","channel":"telegram"}`},
		{name: "chat_id without channel", body: `{"task":"x","model_name":"claude-sonnet-4-6","chat_id":"chat-1"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/spawn", bytes.NewBufferString(tc.body))
			rr := httptest.NewRecorder()
			h(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestSpawnAPI_PostQueueFullReturns429(t *testing.T) {
	queue := workqueue.New(1, 1)
	if err := queue.Submit(context.Background(), workqueue.Job{Name: "busy", Run: func(context.Context) {}}); err != nil {
		t.Fatalf("prefill queue: %v", err)
	}
	al := newTestAgentLoop(t, &gatewayTestProvider{}, queue)
	h := newSpawnCreateHandler(al)

	req := httptest.NewRequest(http.MethodPost, "/api/spawn", bytes.NewBufferString(`{"task":"analyze","model_name":"claude-sonnet-4-6"}`))
	rr := httptest.NewRecorder()
	h(rr, req)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestSpawnAPI_GetTaskStates(t *testing.T) {
	t.Run("running", func(t *testing.T) {
		queue := workqueue.New(1, 1)
		al := newTestAgentLoop(t, &gatewayTestProvider{}, queue)
		manager, _, ok := al.GetDefaultSubagentManager()
		if !ok {
			t.Fatal("default manager not found")
		}
		taskID, _, err := manager.SpawnWithRequest(context.Background(), tools.SpawnRequest{Task: "running", ModelName: "claude-sonnet-4-6", Source: "api"}, nil)
		if err != nil {
			t.Fatalf("spawn: %v", err)
		}
		h := newSpawnGetHandler(al, 64)
		req := httptest.NewRequest(http.MethodGet, "/api/spawn/"+taskID, nil)
		rr := httptest.NewRecorder()
		h(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
		}
		var resp spawnTaskResponse
		decodeJSONBody(t, rr, &resp)
		if resp.Status != "running" {
			t.Fatalf("status = %q", resp.Status)
		}
	})

	t.Run("completed", func(t *testing.T) {
		al := newTestAgentLoop(t, &gatewayTestProvider{}, nil)
		manager, _, ok := al.GetDefaultSubagentManager()
		if !ok {
			t.Fatal("default manager not found")
		}
		taskID, _, err := manager.SpawnWithRequest(context.Background(), tools.SpawnRequest{Task: "done", ModelName: "claude-sonnet-4-6", Source: "api"}, nil)
		if err != nil {
			t.Fatalf("spawn: %v", err)
		}
		waitGatewayTask(t, al, taskID, "completed")
		h := newSpawnGetHandler(al, 10)
		req := httptest.NewRequest(http.MethodGet, "/api/spawn/"+taskID, nil)
		rr := httptest.NewRecorder()
		h(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
		}
		var resp spawnTaskResponse
		decodeJSONBody(t, rr, &resp)
		if resp.Status != "completed" || resp.Result == "" {
			t.Fatalf("unexpected response: %+v", resp)
		}
		if !resp.ResultTruncated {
			t.Fatal("expected truncated result with small payload limit")
		}
	})

	t.Run("failed", func(t *testing.T) {
		al := newTestAgentLoop(t, &gatewayTestProvider{fail: true}, nil)
		manager, _, ok := al.GetDefaultSubagentManager()
		if !ok {
			t.Fatal("default manager not found")
		}
		taskID, _, err := manager.SpawnWithRequest(context.Background(), tools.SpawnRequest{Task: "fail", ModelName: "claude-sonnet-4-6", Source: "api"}, nil)
		if err != nil {
			t.Fatalf("spawn: %v", err)
		}
		waitGatewayTask(t, al, taskID, "failed")
		h := newSpawnGetHandler(al, 64)
		req := httptest.NewRequest(http.MethodGet, "/api/spawn/"+taskID, nil)
		rr := httptest.NewRecorder()
		h(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
		}
		var resp spawnTaskResponse
		decodeJSONBody(t, rr, &resp)
		if resp.Status != "failed" || resp.Error == "" {
			t.Fatalf("unexpected response: %+v", resp)
		}
	})
}

func TestSpawnAPI_GetUnknownTaskReturns404(t *testing.T) {
	al := newTestAgentLoop(t, &gatewayTestProvider{}, nil)
	h := newSpawnGetHandler(al, 64)
	req := httptest.NewRequest(http.MethodGet, "/api/spawn/unknown", nil)
	rr := httptest.NewRecorder()
	h(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func waitGatewayTask(t *testing.T, al *agent.AgentLoop, taskID, want string) *tools.SubagentTask {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		task, ok := al.FindSubagentTask(taskID)
		if ok && task.Status == want {
			return task
		}
		time.Sleep(10 * time.Millisecond)
	}
	task, _ := al.FindSubagentTask(taskID)
	t.Fatalf("task %s did not reach %s, got %+v", taskID, want, task)
	return nil
}
