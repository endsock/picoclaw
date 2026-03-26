package tools

import (
	"context"
	"errors"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/providers"
	webhookpkg "github.com/sipeed/picoclaw/pkg/webhook"
	"github.com/sipeed/picoclaw/pkg/workqueue"
)

type failingMockLLMProvider struct{}

func (m *failingMockLLMProvider) Chat(ctx context.Context, messages []providers.Message, tools []providers.ToolDefinition, model string, options map[string]any) (*providers.LLMResponse, error) {
	return nil, errors.New("boom")
}

func (m *failingMockLLMProvider) GetDefaultModel() string { return "test-model" }
func (m *failingMockLLMProvider) SupportsTools() bool     { return false }
func (m *failingMockLLMProvider) GetContextWindow() int   { return 4096 }

type recorderCall struct {
	kind  string
	task  *SubagentTaskRecord
	id    string
	patch *SubagentTaskFinishPatch
	start int64
}

type mockSubagentTaskRecorder struct {
	mu    sync.Mutex
	calls []recorderCall
}

func (m *mockSubagentTaskRecorder) CreateSubmitted(ctx context.Context, task *SubagentTaskRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	copied := *task
	copied.MetadataJSON = append([]byte(nil), task.MetadataJSON...)
	copied.WebhookJSON = append([]byte(nil), task.WebhookJSON...)
	m.calls = append(m.calls, recorderCall{kind: "submitted", task: &copied})
	return nil
}

func (m *mockSubagentTaskRecorder) MarkRunning(ctx context.Context, taskID string, startedAtMS int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, recorderCall{kind: "running", id: taskID, start: startedAtMS})
	return nil
}

func (m *mockSubagentTaskRecorder) FinishTask(ctx context.Context, taskID string, patch *SubagentTaskFinishPatch) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	copied := *patch
	if patch.StartedAtMS != nil {
		startedAt := *patch.StartedAtMS
		copied.StartedAtMS = &startedAt
	}
	m.calls = append(m.calls, recorderCall{kind: "finished", id: taskID, patch: &copied})
	return nil
}

func (m *mockSubagentTaskRecorder) snapshot() []recorderCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]recorderCall, len(m.calls))
	copy(out, m.calls)
	return out
}

func newTestSubagentManager(t *testing.T, provider providers.LLMProvider, queue *workqueue.Queue) *SubagentManager {
	t.Helper()
	return NewSubagentManager(provider, "test-model", t.TempDir(), nil, 10, queue)
}

func waitForSubagentTask(t *testing.T, manager *SubagentManager, taskID, wantStatus string) *SubagentTask {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		task, ok := manager.GetTask(taskID)
		if ok && task.Status == wantStatus {
			return task
		}
		time.Sleep(10 * time.Millisecond)
	}
	task, _ := manager.GetTask(taskID)
	t.Fatalf("task %s did not reach status %s, got %+v", taskID, wantStatus, task)
	return nil
}

func TestNewSubagentTaskIDFormat(t *testing.T) {
	seen := make(map[string]struct{})
	pattern := regexp.MustCompile(`^subagent-\d{8}$`)
	for i := 0; i < 50; i++ {
		id, err := newSubagentTaskID()
		if err != nil {
			t.Fatalf("newSubagentTaskID failed: %v", err)
		}
		if !pattern.MatchString(id) {
			t.Fatalf("id = %q, want subagent- followed by 8 digits", id)
		}
		if _, exists := seen[id]; exists {
			t.Fatalf("duplicate id generated: %q", id)
		}
		seen[id] = struct{}{}
	}
}

func TestSpawnWithRequestStoresAPIMetadata(t *testing.T) {
	provider := &MockLLMProvider{}
	queue := workqueue.New(1, 1)
	manager := newTestSubagentManager(t, provider, queue)

	taskID, message, err := manager.SpawnWithRequest(context.Background(), SpawnRequest{
		Task:      "analyze code",
		Label:     "api-task",
		AgentID:   "main",
		ModelName: "claude-sonnet-4-6",
		Source:    "api",
		Metadata:  map[string]string{"biz_id": "42"},
		Webhook: &SubagentWebhook{
			URL:        "http://example.com/webhook",
			Headers:    map[string]string{"X-Test": "1"},
			TimeoutMS:  2000,
			MaxRetries: 2,
		},
	}, nil)
	if err != nil {
		t.Fatalf("SpawnWithRequest failed: %v", err)
	}
	if taskID == "" || message == "" {
		t.Fatalf("expected task id and message, got %q %q", taskID, message)
	}

	task, ok := manager.GetTask(taskID)
	if !ok {
		t.Fatalf("task %s not found", taskID)
	}
	if task.Source != "api" {
		t.Fatalf("Source = %q, want api", task.Source)
	}
	if task.Metadata["biz_id"] != "42" {
		t.Fatalf("Metadata = %#v", task.Metadata)
	}
	if task.Webhook == nil || task.Webhook.URL != "http://example.com/webhook" {
		t.Fatalf("Webhook = %#v", task.Webhook)
	}
	if !task.Webhook.Events["completed"] || !task.Webhook.Events["failed"] || !task.Webhook.Events["canceled"] {
		t.Fatalf("default webhook events not set: %#v", task.Webhook.Events)
	}
	if task.Status != "running" {
		t.Fatalf("Status = %q, want running", task.Status)
	}
}

func TestSpawnPreservesToolCallbackBehavior(t *testing.T) {
	provider := &MockLLMProvider{}
	manager := newTestSubagentManager(t, provider, nil)
	callbackCh := make(chan *ToolResult, 1)

	message, err := manager.Spawn(context.Background(), "write summary", "tool-task", "", "claude-sonnet-4-6", "cli", "direct", func(ctx context.Context, result *ToolResult) {
		callbackCh <- result
	})
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}
	if message == "" {
		t.Fatal("expected spawn message")
	}

	select {
	case result := <-callbackCh:
		if result == nil || result.IsError {
			t.Fatalf("unexpected callback result: %+v", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("callback not invoked")
	}

	tasks := manager.ListTasks()
	if len(tasks) != 1 {
		t.Fatalf("ListTasks len = %d, want 1", len(tasks))
	}
	if tasks[0].Source != "tool" {
		t.Fatalf("Source = %q, want tool", tasks[0].Source)
	}
}

func TestSpawnWithRequestRecordsSubmittedAndSubmitFailed(t *testing.T) {
	provider := &MockLLMProvider{}
	queue := workqueue.New(1, 1)
	if err := queue.Submit(context.Background(), workqueue.Job{Name: "occupied", Run: func(context.Context) {}}); err != nil {
		t.Fatalf("prefill queue: %v", err)
	}
	manager := newTestSubagentManager(t, provider, queue)
	recorder := &mockSubagentTaskRecorder{}
	manager.SetTaskRecorder(recorder)

	_, _, err := manager.SpawnWithRequest(context.Background(), SpawnRequest{
		Task:      "analyze code",
		Label:     "api-task",
		AgentID:   "main",
		ModelName: "claude-sonnet-4-6",
		Source:    "api",
		Metadata:  map[string]string{"biz_id": "42"},
		Webhook: &SubagentWebhook{
			URL: "http://example.com/webhook",
		},
	}, nil)
	if !errors.Is(err, workqueue.ErrQueueFull) {
		t.Fatalf("err = %v, want %v", err, workqueue.ErrQueueFull)
	}

	calls := recorder.snapshot()
	if len(calls) != 2 {
		t.Fatalf("calls len = %d, want 2", len(calls))
	}
	if calls[0].kind != "submitted" {
		t.Fatalf("first call = %q, want submitted", calls[0].kind)
	}
	if calls[0].task == nil || calls[0].task.Status != "submitted" {
		t.Fatalf("submitted task = %+v", calls[0].task)
	}
	if calls[0].task.Source != "api" || calls[0].task.ModelName != "claude-sonnet-4-6" {
		t.Fatalf("submitted task fields = %+v", calls[0].task)
	}
	if len(calls[0].task.MetadataJSON) == 0 || len(calls[0].task.WebhookJSON) == 0 {
		t.Fatalf("submitted json fields missing: %+v", calls[0].task)
	}
	if calls[1].kind != "finished" {
		t.Fatalf("second call = %q, want finished", calls[1].kind)
	}
	if calls[1].patch == nil || calls[1].patch.Status != "submit_failed" {
		t.Fatalf("finish patch = %+v", calls[1].patch)
	}
	if calls[1].patch.Error == "" {
		t.Fatal("expected submit_failed error")
	}
}

func TestRunTaskSetsFailedAndCanceledFields(t *testing.T) {
	failedManager := newTestSubagentManager(t, &failingMockLLMProvider{}, nil)
	failedTask := &SubagentTask{ID: "subagent-failed", Task: "fail", ModelName: "test-model", Created: time.Now().UnixMilli()}
	failedManager.runTask(context.Background(), failedTask, nil)
	if failedTask.Status != "failed" {
		t.Fatalf("failed status = %q", failedTask.Status)
	}
	if failedTask.Error == "" {
		t.Fatal("expected failed task error")
	}
	if failedTask.Finished == 0 {
		t.Fatal("expected finished timestamp for failed task")
	}

	canceledManager := newTestSubagentManager(t, &MockLLMProvider{}, nil)
	canceledTask := &SubagentTask{ID: "subagent-canceled", Task: "cancel", ModelName: "test-model", Created: time.Now().UnixMilli()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	canceledManager.runTask(ctx, canceledTask, nil)
	if canceledTask.Status != "canceled" {
		t.Fatalf("canceled status = %q", canceledTask.Status)
	}
	if canceledTask.Error == "" {
		t.Fatal("expected canceled task error")
	}
	if canceledTask.Finished == 0 {
		t.Fatal("expected finished timestamp for canceled task")
	}
}

func TestRunTaskRecordsLifecycleBeforeCallbackAndWebhook(t *testing.T) {
	provider := &MockLLMProvider{}
	manager := newTestSubagentManager(t, provider, nil)
	recorder := &mockSubagentTaskRecorder{}
	manager.SetTaskRecorder(recorder)
	manager.SetWebhookConfig(SubagentWebhookConfig{DefaultTimeoutMS: 1000, DefaultMaxRetries: 1, MaxPayloadBytes: 1024})

	orderMu := sync.Mutex{}
	order := make([]string, 0, 3)
	manager.sendWebhook = func(ctx context.Context, req webhookpkg.SendRequest) error {
		orderMu.Lock()
		order = append(order, "webhook")
		orderMu.Unlock()
		return nil
	}

	task := &SubagentTask{
		ID:        "subagent-order",
		Task:      "ok",
		Label:     "label",
		ModelName: "test-model",
		Source:    "tool",
		Created:   time.Now().UnixMilli(),
		Webhook: &SubagentWebhook{
			URL:    "http://example.com/webhook",
			Events: map[string]bool{"completed": true},
		},
	}

	manager.runTask(context.Background(), task, func(ctx context.Context, result *ToolResult) {
		orderMu.Lock()
		order = append(order, "callback")
		orderMu.Unlock()
	})

	calls := recorder.snapshot()
	if len(calls) != 2 {
		t.Fatalf("recorder calls len = %d, want 2", len(calls))
	}
	if calls[0].kind != "running" || calls[1].kind != "finished" {
		t.Fatalf("recorder calls = %+v", calls)
	}
	if calls[1].patch == nil {
		t.Fatal("expected finish patch")
	}
	if calls[1].patch.Status != "completed" {
		t.Fatalf("finish status = %q, want completed", calls[1].patch.Status)
	}
	if calls[1].patch.CallbackForLLM == "" || calls[1].patch.CallbackForUser == "" {
		t.Fatalf("finish callback fields = %+v", calls[1].patch)
	}
	if calls[1].patch.Iterations <= 0 {
		t.Fatalf("iterations = %d, want > 0", calls[1].patch.Iterations)
	}
	if calls[1].patch.StartedAtMS == nil || *calls[1].patch.StartedAtMS == 0 {
		t.Fatalf("started_at = %+v", calls[1].patch.StartedAtMS)
	}

	orderMu.Lock()
	defer orderMu.Unlock()
	if len(order) != 2 {
		t.Fatalf("order len = %d, want 2 (%v)", len(order), order)
	}
	if order[0] != "callback" || order[1] != "webhook" {
		t.Fatalf("order = %v, want [callback webhook]", order)
	}
}

func TestRunTaskWebhookFailureDoesNotChangeTaskStatus(t *testing.T) {
	provider := &MockLLMProvider{}
	manager := newTestSubagentManager(t, provider, nil)
	called := 0
	manager.sendWebhook = func(ctx context.Context, req webhookpkg.SendRequest) error {
		called++
		return errors.New("webhook down")
	}
	manager.SetWebhookConfig(SubagentWebhookConfig{DefaultTimeoutMS: 1000, DefaultMaxRetries: 1, MaxPayloadBytes: 1024})

	task := &SubagentTask{
		ID:        "subagent-webhook",
		Task:      "ok",
		Label:     "label",
		ModelName: "test-model",
		Source:    "api",
		Created:   time.Now().UnixMilli(),
		Webhook: &SubagentWebhook{
			URL:    "http://example.com/webhook",
			Events: map[string]bool{"completed": true},
		},
	}
	manager.runTask(context.Background(), task, nil)
	if called != 1 {
		t.Fatalf("webhook called %d times, want 1", called)
	}
	if task.Status != "completed" {
		t.Fatalf("Status = %q, want completed", task.Status)
	}
	if task.Error != "" {
		t.Fatalf("Error = %q, want empty", task.Error)
	}
	if task.Finished == 0 {
		t.Fatal("expected finished timestamp")
	}
}
