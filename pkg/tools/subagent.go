package tools

import (
	"context"
	"fmt"
	"github.com/sipeed/picoclaw/pkg/logger"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/skills"
)

// AgentLookup 提供通过 agent_id 查询 agent 配置的能力
// 定义在 pkg/tools/subagent.go 中，由 pkg/agent 实现
type AgentLookup interface {
	LookupAgent(agentID string) (AgentConfig, bool)
}

// AgentConfig 包含 subagent 运行时需要的配置信息
type AgentConfig struct {
	Workspace   string
	MaxTokens   int
	Temperature float64
	Tools       *ToolRegistry
}

// subagentConfig 包含 subagent 运行时需要的所有配置
type subagentConfig struct {
	model       string
	workspace   string
	maxTokens   int
	temperature float64
	tools       *ToolRegistry
}

type SubagentTask struct {
	ID            string
	Task          string
	Label         string
	AgentID       string
	ModelName     string // 新增：指定的模型名称（必传）
	OriginChannel string
	OriginChatID  string
	Status        string
	Result        string
	Created       int64
}

type SubagentManager struct {
	tasks          map[string]*SubagentTask
	mu             sync.RWMutex
	provider       providers.LLMProvider
	defaultModel   string
	workspace      string
	tools          *ToolRegistry // 父 agent 的工具注册表（用于未指定 agent_id 时）
	maxIterations  int
	maxTokens      int
	temperature    float64
	hasMaxTokens   bool
	hasTemperature bool
	nextID         int

	// 新增字段
	agentLookup AgentLookup // 用于通过 agent_id 查询 agent 配置
}

func NewSubagentManager(
	provider providers.LLMProvider,
	defaultModel, workspace string,
	parentTools *ToolRegistry,
) *SubagentManager {
	if parentTools == nil {
		parentTools = NewToolRegistry()
	}
	return &SubagentManager{
		tasks:         make(map[string]*SubagentTask),
		provider:      provider,
		defaultModel:  defaultModel,
		workspace:     workspace,
		tools:         parentTools,
		maxIterations: 10,
		nextID:        1,
	}
}

// SetLLMOptions sets max tokens and temperature for subagent LLM calls.
func (sm *SubagentManager) SetLLMOptions(maxTokens int, temperature float64) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.maxTokens = maxTokens
	sm.hasMaxTokens = true
	sm.temperature = temperature
	sm.hasTemperature = true
}

// SetTools sets the tool registry for subagent execution.
// If not set, subagent will have access to the provided tools.
func (sm *SubagentManager) SetTools(tools *ToolRegistry) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.tools = tools
}

// SetAgentLookup 设置 agent 查询接口（可选）
func (sm *SubagentManager) SetAgentLookup(lookup AgentLookup) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.agentLookup = lookup
}

// RegisterTool registers a tool for subagent execution.
func (sm *SubagentManager) RegisterTool(tool Tool) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.tools.Register(tool)
}

func (sm *SubagentManager) Spawn(
	ctx context.Context,
	task, label, agentID, modelName, originChannel, originChatID string,
	callback AsyncCallback,
) (string, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	taskID := fmt.Sprintf("subagent-%d", sm.nextID)
	sm.nextID++

	subagentTask := &SubagentTask{
		ID:            taskID,
		Task:          task,
		Label:         label,
		AgentID:       agentID,
		ModelName:     modelName,
		OriginChannel: originChannel,
		OriginChatID:  originChatID,
		Status:        "running",
		Created:       time.Now().UnixMilli(),
	}
	sm.tasks[taskID] = subagentTask

	// Start task in background with context cancellation support
	go sm.runTask(ctx, subagentTask, callback)

	if label != "" {
		return fmt.Sprintf("Spawned subagent '%s' for task: %s", label, task), nil
	}
	return fmt.Sprintf("Spawned subagent for task: %s", task), nil
}

// resolveConfig 解析 subagent 的运行时配置
func (sm *SubagentManager) resolveConfig(agentID, modelName string) *subagentConfig {
	cfg := &subagentConfig{
		model:       modelName, // 直接使用传入的 model（必传参数）
		workspace:   sm.workspace,
		maxTokens:   sm.maxTokens,
		temperature: sm.temperature,
		tools:       sm.tools, // 默认使用父 agent 的工具
	}

	// 如果指定了 agent_id，通过 AgentLookup 接口获取目标 agent 配置
	if agentID != "" && sm.agentLookup != nil {
		if ac, ok := sm.agentLookup.LookupAgent(agentID); ok {
			logger.InfoCF("subagent", "Resolved agent config",
				map[string]any{
					"agent_id":  agentID,
					"workspace": ac.Workspace,
				})
			cfg.workspace = ac.Workspace
			cfg.maxTokens = ac.MaxTokens
			cfg.temperature = ac.Temperature
			cfg.tools = ac.Tools // 使用目标 agent 的工具
		}
	}

	return cfg
}

// buildSubagentSystemPrompt 从 workspace 加载上下文文件构建 system prompt
func (sm *SubagentManager) buildSubagentSystemPrompt(workspace string) string {
	var sb strings.Builder

	// 1. 基础身份和工作目录
	sb.WriteString("# Subagent\n\n")
	sb.WriteString("You are a subagent executing a delegated task. ")
	sb.WriteString("Complete the task independently and report the result.\n\n")

	// 明确告知工作目录
	sb.WriteString(fmt.Sprintf("## Workspace\n\nYour workspace is at: %s\n\n", workspace))

	// 2. 从 workspace 加载上下文文件
	bootstrapFiles := []string{
		"AGENTS.md",
		"SOUL.md",
		"USER.md",
	}

	for _, filename := range bootstrapFiles {
		path := filepath.Join(workspace, filename)
		if content, err := os.ReadFile(path); err == nil && len(content) > 0 {
			sb.WriteString(fmt.Sprintf("## %s\n\n", filename))
			sb.WriteString(string(content))
			sb.WriteString("\n\n")
		}
	}

	// 3. 加载 memory/MEMORY.md
	memoryPath := filepath.Join(workspace, "memory", "MEMORY.md")
	if content, err := os.ReadFile(memoryPath); err == nil && len(content) > 0 {
		sb.WriteString("## Memory\n\n")
		sb.WriteString(string(content))
		sb.WriteString("\n\n")
	}

	// 4. 工具使用提示
	sb.WriteString("## Instructions\n\n")
	sb.WriteString("You have access to tools - use them as needed to complete your task. ")
	sb.WriteString("After completing the task, provide a clear summary of what was done.\n")

	return sb.String()
}

// buildSubagentSkillsSummary 加载指定 agent 的 skills 摘要
func (sm *SubagentManager) buildSubagentSkillsSummary(workspace string) string {
	// 复用 skills.SkillsLoader 的逻辑
	skillsLoader := skills.NewSkillsLoader(workspace,
		filepath.Join(getGlobalConfigDir(), "skills"),
		getBuiltinSkillsDir())

	return skillsLoader.BuildSkillsSummary()
}

// getGlobalConfigDir 获取全局配置目录
func getGlobalConfigDir() string {
	if home := os.Getenv("PICOCLAW_HOME"); home != "" {
		return home
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".picoclaw")
}

// getBuiltinSkillsDir 获取内置 skills 目录
func getBuiltinSkillsDir() string {
	if dir := os.Getenv("PICOCLAW_BUILTIN_SKILLS"); dir != "" {
		return dir
	}
	wd, _ := os.Getwd()
	return filepath.Join(wd, "skills")
}

func (sm *SubagentManager) runTask(ctx context.Context, task *SubagentTask, callback AsyncCallback) {
	task.Status = "running"
	task.Created = time.Now().UnixMilli()

	// 解析配置
	sm.mu.RLock()
	cfg := sm.resolveConfig(task.AgentID, task.ModelName)
	maxIter := sm.maxIterations
	sm.mu.RUnlock()

	// 构建 system prompt（从 workspace 加载）
	systemPrompt := sm.buildSubagentSystemPrompt(cfg.workspace)

	// 构建 skills 摘要
	skillsSummary := sm.buildSubagentSkillsSummary(cfg.workspace)
	if skillsSummary != "" {
		systemPrompt += "\n\n" + skillsSummary
	}

	messages := []providers.Message{
		{
			Role:    "system",
			Content: systemPrompt,
		},
		{
			Role:    "user",
			Content: task.Task,
		},
	}

	// Check if context is already canceled before starting
	select {
	case <-ctx.Done():
		sm.mu.Lock()
		task.Status = "canceled"
		task.Result = "Task canceled before execution"
		sm.mu.Unlock()
		return
	default:
	}

	// 构建 LLM 选项
	var llmOptions map[string]any
	if cfg.maxTokens > 0 || cfg.temperature > 0 {
		llmOptions = map[string]any{}
		if cfg.maxTokens > 0 {
			llmOptions["max_tokens"] = cfg.maxTokens
		}
		if cfg.temperature > 0 {
			llmOptions["temperature"] = cfg.temperature
		}
	}

	// 运行工具循环，使用解析后的模型和配置
	loopResult, err := RunToolLoop(ctx, ToolLoopConfig{
		Provider:      sm.provider,
		Model:         cfg.model,
		Tools:         cfg.tools,
		MaxIterations: maxIter,
		LLMOptions:    llmOptions,
	}, messages, task.OriginChannel, task.OriginChatID)

	sm.mu.Lock()
	var result *ToolResult
	defer func() {
		sm.mu.Unlock()
		// Call callback if provided and result is set
		if callback != nil && result != nil {
			callback(ctx, result)
		}
	}()

	if err != nil {
		task.Status = "failed"
		task.Result = fmt.Sprintf("Error: %v", err)
		// Check if it was canceled
		if ctx.Err() != nil {
			task.Status = "canceled"
			task.Result = "Task canceled during execution"
		}
		result = &ToolResult{
			ForLLM:  task.Result,
			ForUser: "",
			Silent:  false,
			IsError: true,
			Async:   false,
			Err:     err,
		}
	} else {
		task.Status = "completed"
		task.Result = loopResult.Content
		result = &ToolResult{
			ForLLM: fmt.Sprintf(
				"Subagent '%s' completed (iterations: %d): %s",
				task.Label,
				loopResult.Iterations,
				loopResult.Content,
			),
			ForUser: loopResult.Content,
			Silent:  false,
			IsError: false,
			Async:   false,
		}
	}
}

func (sm *SubagentManager) GetTask(taskID string) (*SubagentTask, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	task, ok := sm.tasks[taskID]
	return task, ok
}

func (sm *SubagentManager) ListTasks() []*SubagentTask {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	tasks := make([]*SubagentTask, 0, len(sm.tasks))
	for _, task := range sm.tasks {
		tasks = append(tasks, task)
	}
	return tasks
}

// SubagentTool executes a subagent task synchronously and returns the result.
// Unlike SpawnTool which runs tasks asynchronously, SubagentTool waits for completion
// and returns the result directly in the ToolResult.
type SubagentTool struct {
	manager *SubagentManager
}

func NewSubagentTool(manager *SubagentManager) *SubagentTool {
	return &SubagentTool{
		manager: manager,
	}
}

func (t *SubagentTool) Name() string {
	return "subagent"
}

func (t *SubagentTool) Description() string {
	return "Execute a subagent task synchronously and return the result. Use this for delegating specific tasks to an independent agent instance. Returns execution summary to user and full details to LLM."
}

func (t *SubagentTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"task": map[string]any{
				"type":        "string",
				"description": "The task for subagent to complete",
			},
			"label": map[string]any{
				"type":        "string",
				"description": "Optional short label for the task (for display)",
			},
			"agent_id": map[string]any{
				"type":        "string",
				"description": "Optional target agent ID to delegate the task to",
			},
			"model_name": map[string]any{
				"type":        "string",
				"description": "Required model name to use (e.g., claude-sonnet-4-6)",
			},
		},
		"required": []string{"task", "model_name"},
	}
}

func (t *SubagentTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	task, ok := args["task"].(string)
	if !ok {
		return ErrorResult("task is required").WithError(fmt.Errorf("task parameter is required"))
	}

	modelName, ok := args["model_name"].(string)
	if !ok || strings.TrimSpace(modelName) == "" {
		return ErrorResult("model_name is required").WithError(fmt.Errorf("model_name parameter is required"))
	}

	label, _ := args["label"].(string)
	agentID, _ := args["agent_id"].(string)

	if t.manager == nil {
		return ErrorResult("Subagent manager not configured").WithError(fmt.Errorf("manager is nil"))
	}

	// 解析配置（通过 AgentLookup 接口查询 agent 配置）
	sm := t.manager
	sm.mu.RLock()
	cfg := sm.resolveConfig(agentID, modelName)
	maxIter := sm.maxIterations
	sm.mu.RUnlock()

	// 构建带上下文的 system prompt
	systemPrompt := sm.buildSubagentSystemPrompt(cfg.workspace)
	skillsSummary := sm.buildSubagentSkillsSummary(cfg.workspace)
	if skillsSummary != "" {
		systemPrompt += "\n\n" + skillsSummary
	}

	messages := []providers.Message{
		{
			Role:    "system",
			Content: systemPrompt,
		},
		{
			Role:    "user",
			Content: task,
		},
	}

	// 构建 LLM 选项
	var llmOptions map[string]any
	if cfg.maxTokens > 0 || cfg.temperature > 0 {
		llmOptions = map[string]any{}
		if cfg.maxTokens > 0 {
			llmOptions["max_tokens"] = cfg.maxTokens
		}
		if cfg.temperature > 0 {
			llmOptions["temperature"] = cfg.temperature
		}
	}

	// Fall back to "cli"/"direct" for non-conversation callers
	channel := ToolChannel(ctx)
	if channel == "" {
		channel = "cli"
	}
	chatID := ToolChatID(ctx)
	if chatID == "" {
		chatID = "direct"
	}

	loopResult, err := RunToolLoop(ctx, ToolLoopConfig{
		Provider:      sm.provider,
		Model:         cfg.model,
		Tools:         cfg.tools,
		MaxIterations: maxIter,
		LLMOptions:    llmOptions,
	}, messages, channel, chatID)
	if err != nil {
		return ErrorResult(fmt.Sprintf("Subagent execution failed: %v", err)).WithError(err)
	}

	// ForUser: Brief summary for user (truncated if too long)
	userContent := loopResult.Content
	maxUserLen := 500
	if len(userContent) > maxUserLen {
		userContent = userContent[:maxUserLen] + "..."
	}

	// ForLLM: Full execution details
	labelStr := label
	if labelStr == "" {
		labelStr = "(unnamed)"
	}
	llmContent := fmt.Sprintf("Subagent task completed:\nLabel: %s\nIterations: %d\nResult: %s",
		labelStr, loopResult.Iterations, loopResult.Content)

	return &ToolResult{
		ForLLM:  llmContent,
		ForUser: userContent,
		Silent:  false,
		IsError: false,
		Async:   false,
	}
}
