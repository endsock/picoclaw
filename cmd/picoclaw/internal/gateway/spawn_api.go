package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/sipeed/picoclaw/pkg/agent"
	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/tools"
	"github.com/sipeed/picoclaw/pkg/workqueue"
)

type spawnAPIRequest struct {
	Task      string            `json:"task"`
	Label     string            `json:"label,omitempty"`
	AgentID   string            `json:"agent_id,omitempty"`
	ModelName string            `json:"model_name"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	Webhook   *spawnWebhookSpec `json:"webhook,omitempty"`
	Channel   string            `json:"channel,omitempty"`
	ChatID    string            `json:"chat_id,omitempty"`
}

type spawnWebhookSpec struct {
	URL        string            `json:"url"`
	Headers    map[string]string `json:"headers,omitempty"`
	Events     []string          `json:"events,omitempty"`
	TimeoutMS  int               `json:"timeout_ms,omitempty"`
	MaxRetries int               `json:"max_retries,omitempty"`
}

type spawnAcceptedResponse struct {
	TaskID            string `json:"task_id"`
	Status            string `json:"status"`
	Message           string `json:"message"`
	WebhookRegistered bool   `json:"webhook_registered"`
	ResultURL         string `json:"result_url"`
}

type spawnTaskResponse struct {
	TaskID          string            `json:"task_id"`
	Status          string            `json:"status"`
	Label           string            `json:"label,omitempty"`
	AgentID         string            `json:"agent_id,omitempty"`
	ModelName       string            `json:"model_name"`
	Source          string            `json:"source,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`
	CreatedAtMS     int64             `json:"created_at_ms"`
	FinishedAtMS    int64             `json:"finished_at_ms"`
	DurationMS      int64             `json:"duration_ms,omitempty"`
	Result          string            `json:"result,omitempty"`
	Error           string            `json:"error,omitempty"`
	ResultTruncated bool              `json:"result_truncated,omitempty"`
	ResultSize      int               `json:"result_size,omitempty"`
}

func registerSpawnAPIRoutes(services *gatewayServices, agentLoop *agent.AgentLoop) {
	if services == nil || services.ChannelManager == nil || agentLoop == nil {
		return
	}
	cfg := agentLoop.GetConfig()
	if cfg == nil || !cfg.Gateway.SpawnAPI.Enabled {
		return
	}

	services.ChannelManager.HandleHTTPFunc("/api/spawn", newSpawnCreateHandler(agentLoop))
	services.ChannelManager.HandleHTTPFunc("/api/spawn/", newSpawnGetHandler(agentLoop, cfg.Gateway.OutboundWebhook.MaxPayloadBytes))
}

func newSpawnCreateHandler(agentLoop *agent.AgentLoop) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req spawnAPIRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		if err := validateSpawnAPIRequest(req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		manager, resolvedAgentID, err := resolveSubagentManager(agentLoop, req.AgentID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		channel := strings.TrimSpace(req.Channel)
		chatID := strings.TrimSpace(req.ChatID)
		var callback tools.AsyncCallback
		if channel != "" && chatID != "" {
			callback = func(ctx context.Context, result *tools.ToolResult) {
				if result == nil || result.Silent || strings.TrimSpace(result.ForUser) == "" {
					return
				}
				_ = agentLoop.PublishOutbound(ctx, bus.OutboundMessage{
					Channel: channel,
					ChatID:  chatID,
					Content: result.ForUser,
				})
			}
		}

		taskID, message, err := manager.SpawnWithRequest(r.Context(), tools.SpawnRequest{
			Task:          req.Task,
			Label:         req.Label,
			AgentID:       resolvedAgentID,
			ModelName:     req.ModelName,
			Source:        "api",
			Metadata:      cloneMetadata(req.Metadata),
			Webhook:       buildSubagentWebhook(req.Webhook),
			OriginChannel: channel,
			OriginChatID:  chatID,
			SenderID:      strings.TrimSpace(req.Metadata["sender_id"]),
		}, callback)
		if err != nil {
			if errors.Is(err, workqueue.ErrQueueFull) {
				http.Error(w, err.Error(), http.StatusTooManyRequests)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		writeJSON(w, http.StatusAccepted, spawnAcceptedResponse{
			TaskID:            taskID,
			Status:            "accepted",
			Message:           message,
			WebhookRegistered: req.Webhook != nil,
			ResultURL:         "/api/spawn/" + taskID,
		})
	}
}

func newSpawnGetHandler(agentLoop *agent.AgentLoop, maxPayloadBytes int) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		taskID := strings.TrimPrefix(r.URL.Path, "/api/spawn/")
		if taskID == "" {
			http.Error(w, "task_id is required", http.StatusBadRequest)
			return
		}
		task, ok := agentLoop.FindSubagentTask(taskID)
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, buildSpawnTaskResponse(task, maxPayloadBytes))
	}
}

func validateSpawnAPIRequest(req spawnAPIRequest) error {
	if strings.TrimSpace(req.Task) == "" {
		return errors.New("task is required")
	}
	if strings.TrimSpace(req.ModelName) == "" {
		return errors.New("model_name is required")
	}
	channel := strings.TrimSpace(req.Channel)
	chatID := strings.TrimSpace(req.ChatID)
	if (channel == "") != (chatID == "") {
		return errors.New("channel and chat_id must be provided together")
	}
	if req.Webhook == nil {
		return nil
	}
	if strings.TrimSpace(req.Webhook.URL) == "" {
		return errors.New("webhook.url is required")
	}
	parsed, err := url.Parse(req.Webhook.URL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return errors.New("webhook.url must be a valid http or https url")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("webhook.url must be a valid http or https url")
	}
	for _, event := range req.Webhook.Events {
		switch event {
		case "completed", "failed", "canceled":
		default:
			return errors.New("webhook.events contains unsupported event")
		}
	}
	return nil
}

func resolveSubagentManager(agentLoop *agent.AgentLoop, agentID string) (*tools.SubagentManager, string, error) {
	if strings.TrimSpace(agentID) != "" {
		manager, ok := agentLoop.GetSubagentManager(agentID)
		if !ok {
			return nil, "", errors.New("agent_id not found")
		}
		return manager, agentID, nil
	}
	manager, resolvedAgentID, ok := agentLoop.GetDefaultSubagentManager()
	if !ok {
		return nil, "", errors.New("default agent manager not found")
	}
	return manager, resolvedAgentID, nil
}

func buildSubagentWebhook(spec *spawnWebhookSpec) *tools.SubagentWebhook {
	if spec == nil {
		return nil
	}
	events := map[string]bool{}
	for _, event := range spec.Events {
		events[event] = true
	}
	return &tools.SubagentWebhook{
		URL:        spec.URL,
		Headers:    cloneMetadata(spec.Headers),
		Events:     events,
		TimeoutMS:  spec.TimeoutMS,
		MaxRetries: spec.MaxRetries,
	}
}

func cloneMetadata(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]string, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func buildSpawnTaskResponse(task *tools.SubagentTask, maxPayloadBytes int) spawnTaskResponse {
	result := task.Result
	resultSize := len([]byte(result))
	truncated := false
	if maxPayloadBytes > 0 && result != "" && resultSize > maxPayloadBytes {
		result = truncateResult(result, maxPayloadBytes)
		truncated = true
	}
	resp := spawnTaskResponse{
		TaskID:       task.ID,
		Status:       task.Status,
		Label:        task.Label,
		AgentID:      task.AgentID,
		ModelName:    task.ModelName,
		Source:       task.Source,
		Metadata:     cloneMetadata(task.Metadata),
		CreatedAtMS:  task.Created,
		FinishedAtMS: task.Finished,
		DurationMS:   durationMS(task),
		Result:       result,
		Error:        task.Error,
	}
	if truncated {
		resp.ResultTruncated = true
		resp.ResultSize = resultSize
	}
	return resp
}

func truncateResult(s string, maxBytes int) string {
	if maxBytes <= 0 || len([]byte(s)) <= maxBytes {
		return s
	}
	if maxBytes <= 3 {
		return strings.Repeat(".", maxBytes)
	}
	limit := maxBytes - 3
	buf := make([]byte, 0, limit)
	for _, r := range s {
		rb := []byte(string(r))
		if len(buf)+len(rb) > limit {
			break
		}
		buf = append(buf, rb...)
	}
	return string(buf) + "..."
}

func durationMS(task *tools.SubagentTask) int64 {
	if task == nil || task.Created <= 0 || task.Finished <= 0 || task.Finished < task.Created {
		return 0
	}
	return task.Finished - task.Created
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}
