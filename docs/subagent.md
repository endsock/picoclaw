# Subagent 增强修改计划

## 概述

本计划旨在增强 `spawn` 和 `subagent` 工具，使其能够：
1. 支持指定模型名称
2. 使用指定 agent 的 workspace
3. 从 workspace 加载上下文文件（SOUL.md, USER.md, AGENTS.md, memory/MEMORY.md）
4. 使用指定 agent 的 skills
5. 使用指定 agent 的 tools（read_file, write_file, exec, web_search 等已注册的工具）

---

## 当前架构分析

### 现有代码结构

```
pkg/tools/
├── spawn.go           # SpawnTool - 异步启动 subagent
├── subagent.go        # SubagentTool - 同步执行 subagent
│                       # SubagentManager - 管理 subagent 任务
└── tool_loop.go       # RunToolLoop - 工具循环执行

pkg/agent/
├── context.go         # ContextBuilder - 构建 system prompt，加载 .md 文件
├── memory.go          # MemoryStore - 管理 memory/MEMORY.md
├── registry.go        # AgentRegistry - 管理所有 agent 实例
├── instance.go        # AgentInstance - agent 实例定义
└── loop.go            # AgentLoop - 初始化 SubagentManager
```

### 当前问题

1. **SpawnTool 参数不足**：
   - 只有 `task`, `label`, `agent_id`
   - 缺少 `model_name` 参数

2. **SubagentManager 未使用 workspace**：
   - workspace 字段存储了但从未使用
   - system prompt 是硬编码的简单字符串
   - 没有加载任何 workspace 文件

3. **SubagentManager 与 AgentRegistry 隔离**：
   - 无法通过 agent_id 获取目标 agent 的配置
   - 无法访问目标 agent 的 workspace、model、skills

4. **缺少 ContextBuilder 集成**：
   - 没有从 workspace 加载 SOUL.md, USER.md, AGENTS.md
   - 没有从 memory/MEMORY.md 加载记忆

5. **SubagentManager 未使用目标 agent 的 tools**：
   - 构造时创建空的 `NewToolRegistry()`，没有注册任何工具
   - 指定 `agent_id` 时无法获取目标 agent 已注册的工具（read_file, write_file, exec 等）
   - 导致 subagent 只能做纯文本生成，无法执行任何工具调用

---

## 详细修改计划

### 阶段 1: 扩展 SpawnTool 参数

#### 文件: `pkg/tools/spawn.go`

**1.1 添加 `model_name` 参数**

修改 `Parameters()` 方法：

```go
func (t *SpawnTool) Parameters() map[string]any {
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
            "model_name": map[string]any{  // 新增，必传
                "type":        "string",
                "description": "Required model name to use (e.g., claude-sonnet-4-6)",
            },
        },
        "required": []string{"task", "model_name"},
    }
}
```

**1.2 修改 `execute()` 方法**

提取新参数并传递给 SubagentManager：

```go
func (t *SpawnTool) execute(ctx context.Context, args map[string]any, cb AsyncCallback) *ToolResult {
    task, ok := args["task"].(string)
    if !ok || strings.TrimSpace(task) == "" {
        return ErrorResult("task is required and must be a non-empty string")
    }

    modelName, ok := args["model_name"].(string)
    if !ok || strings.TrimSpace(modelName) == "" {
        return ErrorResult("model_name is required and must be a non-empty string")
    }

    label, _ := args["label"].(string)
    agentID, _ := args["agent_id"].(string)

    // ... allowlist check ...

    // 修改 Spawn 调用，添加 modelName 参数
    result, err := t.manager.Spawn(ctx, task, label, agentID, modelName, channel, chatID, cb)
    // ...
}
```

---

### 阶段 2: 扩展 SubagentManager

#### 文件: `pkg/tools/subagent.go`

**2.1 修改 SubagentManager 结构体**

添加 `AgentLookup` 接口（避免循环依赖）：

```go
// AgentLookup 提供通过 agent_id 查询 agent 配置的能力
// 定义在 pkg/tools/subagent.go 中，由 pkg/agent 实现
type AgentLookup interface {
    LookupAgent(agentID string) (AgentConfig, bool)
}

type AgentConfig struct {
    Workspace   string
    MaxTokens   int
    Temperature float64
    Tools       *ToolRegistry
}

type SubagentManager struct {
    tasks          map[string]*SubagentTask
    mu             sync.RWMutex
    provider       providers.LLMProvider
    workspace      string
    tools          *ToolRegistry  // 父 agent 的工具注册表（用于未指定 agent_id 时）
    maxIterations  int
    maxTokens      int
    temperature    float64
    hasMaxTokens   bool
    hasTemperature bool
    nextID         int

    // 新增字段
    agentLookup    AgentLookup  // 用于通过 agent_id 查询 agent 配置
}
```

**2.2 修改 SubagentTask 结构体**

```go
type SubagentTask struct {
    ID            string
    Task          string
    Label         string
    AgentID       string
    ModelName     string  // 新增：指定的模型名称
    OriginChannel string
    OriginChatID  string
    Status        string
    Result        string
    Created       int64
}
```

**2.3 修改 NewSubagentManager 构造函数**

```go
func NewSubagentManager(
    provider providers.LLMProvider,
    workspace string,
    parentTools *ToolRegistry,  // 父 agent 的工具注册表
) *SubagentManager {
    return &SubagentManager{
        tasks:         make(map[string]*SubagentTask),
        provider:      provider,
        workspace:     workspace,
        tools:         parentTools,  // 使用父 agent 的工具
        maxIterations: 10,
        nextID:        1,
    }
}

// SetAgentLookup 设置 agent 查询接口（可选）
func (sm *SubagentManager) SetAgentLookup(lookup AgentLookup) {
    sm.mu.Lock()
    defer sm.mu.Unlock()
    sm.agentLookup = lookup
}
```

**2.4 修改 Spawn 方法签名**

```go
func (sm *SubagentManager) Spawn(
    ctx context.Context,
    task, label, agentID, modelName string,  // 添加 modelName 参数
    originChannel, originChatID string,
    callback AsyncCallback,
) (string, error) {
    // ...

    subagentTask := &SubagentTask{
        ID:            taskID,
        Task:          task,
        Label:         label,
        AgentID:       agentID,
        ModelName:     modelName,  // 新增
        OriginChannel: originChannel,
        OriginChatID:  originChatID,
        Status:        "running",
        Created:       time.Now().UnixMilli(),
    }
    // ...
}
```

**2.5 新增辅助方法：解析运行时配置**

```go
// subagentConfig 包含 subagent 运行时需要的所有配置
type subagentConfig struct {
    model       string
    workspace   string
    maxTokens   int
    temperature float64
    skillsDir   string
    tools       *ToolRegistry  // 新增：subagent 使用的工具注册表
}

// resolveConfig 解析 subagent 的运行时配置
func (sm *SubagentManager) resolveConfig(agentID, modelName string) *subagentConfig {
    cfg := &subagentConfig{
        model:       modelName,  // 直接使用传入的 model（必传参数）
        workspace:   sm.workspace,
        maxTokens:   sm.maxTokens,
        temperature: sm.temperature,
        tools:       sm.tools,  // 默认使用父 agent 的工具
    }

    // 如果指定了 agent_id，通过 AgentLookup 接口获取目标 agent 配置
    if agentID != "" && sm.agentLookup != nil {
        if ac, ok := sm.agentLookup.LookupAgent(agentID); ok {
            cfg.workspace = ac.Workspace
            cfg.maxTokens = ac.MaxTokens
            cfg.temperature = ac.Temperature
            cfg.tools = ac.Tools  // 使用目标 agent 的工具
        }
    }

    return cfg
}
```

**2.6 新增辅助方法：构建 Subagent System Prompt**

```go
// buildSubagentSystemPrompt 从 workspace 加载上下文文件构建 system prompt
func (sm *SubagentManager) buildSubagentSystemPrompt(workspace string) string {
    var sb strings.Builder

    // 1. 基础身份
    sb.WriteString("# Subagent\n\n")
    sb.WriteString("You are a subagent executing a delegated task. ")
    sb.WriteString("Complete the task independently and report the result.\n\n")

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
```

**2.7 新增辅助方法：加载 Skills 摘要**

```go
// buildSubagentSkillsSummary 加载指定 agent 的 skills 摘要
func (sm *SubagentManager) buildSubagentSkillsSummary(workspace string) string {
    // 复用 skills.SkillsLoader 的逻辑
    skillsLoader := skills.NewSkillsLoader(workspace,
        filepath.Join(getGlobalConfigDir(), "skills"),
        getBuiltinSkillsDir())

    return skillsLoader.BuildSkillsSummary()
}

// 辅助函数（从 pkg/agent/context.go 复用）
func getGlobalConfigDir() string {
    // 参考 pkg/agent/context.go 中的实现
    // ...
}

func getBuiltinSkillsDir() string {
    // 参考 pkg/agent/context.go 中的实现
    // ...
}
```

**2.8 修改 runTask 方法**

```go
func (sm *SubagentManager) runTask(ctx context.Context, task *SubagentTask, callback AsyncCallback) {
    task.Status = "running"
    task.Created = time.Now().UnixMilli()

    // 解析配置
    cfg := sm.resolveConfig(task.AgentID, task.ModelName)

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

    // ... context cancellation check ...

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
        Model:         cfg.model,   // 使用解析后的模型
        Tools:         cfg.tools,   // 使用解析后的工具（目标 agent 的工具）
        MaxIterations: sm.maxIterations,
        LLMOptions:    llmOptions,
    }, messages, task.OriginChannel, task.OriginChatID)

    // ... 结果处理 ...
}
```

---

### 阶段 3: 修改 AgentLoop 初始化

#### 文件: `pkg/agent/loop.go`

**3.1 修改 SubagentManager 创建**

```go
// 在 registerSharedTools 函数中
if cfg.Tools.IsToolEnabled("spawn") {
    if cfg.Tools.IsToolEnabled("subagent") {
        subagentManager := tools.NewSubagentManager(provider, agent.Workspace, agent.Tools)
        subagentManager.SetLLMOptions(agent.MaxTokens, agent.Temperature)

        // 新增：注入 AgentLookup 接口
        subagentManager.SetAgentLookup(&agentLookupAdapter{registry: registry})

        spawnTool := tools.NewSpawnTool(subagentManager)
        currentAgentID := agentID
        spawnTool.SetAllowlistChecker(func(targetAgentID string) bool {
            return registry.CanSpawnSubagent(currentAgentID, targetAgentID)
        })
        agent.Tools.Register(spawnTool)
    }
}
```

**3.2 新增 agentLookupAdapter**

```go
// agentLookupAdapter 将 AgentRegistry 适配为 tools.AgentLookup 接口
type agentLookupAdapter struct {
    registry *AgentRegistry
}

func (a *agentLookupAdapter) LookupAgent(agentID string) (tools.AgentConfig, bool) {
    agent, ok := a.registry.GetAgent(agentID)
    if !ok {
        return tools.AgentConfig{}, false
    }
    return tools.AgentConfig{
        Workspace:   agent.Workspace,
        MaxTokens:   agent.MaxTokens,
        Temperature: agent.Temperature,
        Tools:       agent.Tools,
    }, true
}
```

---

### 阶段 4: 同步修改 SubagentTool

#### 文件: `pkg/tools/subagent.go`

**4.1 修改 SubagentTool.Parameters()**

与 SpawnTool 保持一致，`model_name` 为必传参数，`agent_id` 为可选参数。

**4.2 修改 SubagentTool.Execute()**

```go
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
    agentID, _ := args["agent_id"].(string)  // 可选

    // 解析配置（通过 AgentLookup 接口查询 agent 配置）
    sm := t.manager
    cfg := sm.resolveConfig(agentID, modelName)

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

    // ... 执行 RunToolLoop，使用 cfg.model 和 cfg.tools ...
}
```

---

## Agent 加载 Skill 的逻辑分析

### 核心组件

#### 1. SkillsLoader (`pkg/skills/loader.go`)

负责从三个来源加载 skills，按优先级：`workspace > global > builtin`

```go
type SkillsLoader struct {
    workspace       string
    workspaceSkills string // workspace/skills (项目级)
    globalSkills    string // ~/.picoclaw/skills (全局)
    builtinSkills   string // 内置 skills
}

func NewSkillsLoader(workspace, globalSkills, builtinSkills string) *SkillsLoader
```

**关键方法：**

| 方法 | 作用 |
|-----|------|
| `ListSkills()` | 扫描所有 skill 目录，返回 `[]SkillInfo` |
| `LoadSkill(name)` | 按优先级加载指定 skill 内容 |
| `BuildSkillsSummary()` | 生成 XML 格式的 skills 摘要，嵌入 system prompt |
| `SkillRoots()` | 返回所有 skill 根目录路径 |

#### 2. MemoryStore (`pkg/agent/memory.go`)

管理 memory/MEMORY.md 和每日笔记：

```go
type MemoryStore struct {
    workspace  string
    memoryDir  string  // workspace/memory
    memoryFile string  // workspace/memory/MEMORY.md
}

func NewMemoryStore(workspace string) *MemoryStore

// 关键方法
func (ms *MemoryStore) ReadLongTerm() string      // 读取 MEMORY.md
func (ms *MemoryStore) GetMemoryContext() string  // 获取完整 memory 上下文
```

#### 3. ContextBuilder (`pkg/agent/context.go`)

**构建 system prompt 的流程：**

```go
// 第 65-79 行
func NewContextBuilder(workspace string) *ContextBuilder {
    // 确定 skills 目录
    builtinSkillsDir := os.Getenv("PICOCLAW_BUILTIN_SKILLS")
    if builtinSkillsDir == "" {
        wd, _ := os.Getwd()
        builtinSkillsDir = filepath.Join(wd, "skills")
    }
    globalSkillsDir := filepath.Join(getGlobalConfigDir(), "skills")  // ~/.picoclaw/skills

    return &ContextBuilder{
        workspace:    workspace,
        skillsLoader: skills.NewSkillsLoader(workspace, globalSkillsDir, builtinSkillsDir),
        memory:       NewMemoryStore(workspace),
    }
}

// 第 131-161 行 - 构建 system prompt
func (cb *ContextBuilder) BuildSystemPrompt() string {
    parts := []string{}

    // 1. Core identity section
    parts = append(parts, cb.getIdentity())

    // 2. Bootstrap files (AGENTS.md, SOUL.md, USER.md, IDENTITY.md)
    bootstrapContent := cb.LoadBootstrapFiles()
    if bootstrapContent != "" {
        parts = append(parts, bootstrapContent)
    }

    // 3. Skills summary
    skillsSummary := cb.skillsLoader.BuildSkillsSummary()
    if skillsSummary != "" {
        parts = append(parts, fmt.Sprintf(`# Skills
The following skills extend your capabilities...
%s`, skillsSummary))
    }

    // 4. Memory context (MEMORY.md + daily notes)
    memoryContext := cb.memory.GetMemoryContext()
    if memoryContext != "" {
        parts = append(parts, "# Memory\n\n"+memoryContext)
    }

    return strings.Join(parts, "\n\n---\n\n")
}

// 第 434-451 行 - 加载 Bootstrap 文件
func (cb *ContextBuilder) LoadBootstrapFiles() string {
    bootstrapFiles := []string{
        "AGENTS.md",
        "SOUL.md",
        "USER.md",
        "IDENTITY.md",
    }

    var sb strings.Builder
    for _, filename := range bootstrapFiles {
        filePath := filepath.Join(cb.workspace, filename)
        if data, err := os.ReadFile(filePath); err == nil {
            fmt.Fprintf(&sb, "## %s\n\n%s\n\n", filename, data)
        }
    }
    return sb.String()
}
```

### 加载流程图

```
NewContextBuilder(workspace)
    │
    ├── skills.NewSkillsLoader(workspace, globalSkillsDir, builtinSkillsDir)
    │
    └── NewMemoryStore(workspace)
            │
            ▼
    BuildSystemPrompt()
            │
            ├── getIdentity()              → 基础身份信息
            │
            ├── LoadBootstrapFiles()       → AGENTS.md, SOUL.md, USER.md, IDENTITY.md
            │
            ├── skillsLoader.BuildSkillsSummary()  → XML 格式的 skills 列表
            │
            └── memory.GetMemoryContext()  → MEMORY.md + recent daily notes
                    │
                    ▼
            嵌入到 system prompt 中发送给 LLM
```

### 可复用的组件

**✅ 所有组件都可以直接复用！**

| 组件 | 文件 | 复用方式 | 说明 |
|-----|------|---------|------|
| SkillsLoader | `pkg/skills/loader.go` | `skills.NewSkillsLoader(workspace, globalSkills, builtinSkills)` | 只需传入 workspace 路径 |
| MemoryStore | `pkg/agent/memory.go` | `agent.NewMemoryStore(workspace)` | 只需传入 workspace 路径 |
| LoadBootstrapFiles | `pkg/agent/context.go:434-451` | 复制逻辑或提取为独立函数 | 简单的文件读取 |

**在 subagent 中复用的示例代码：**

```go
import (
    "github.com/sipeed/picoclaw/pkg/agent"
    "github.com/sipeed/picoclaw/pkg/skills"
)

// 构建 subagent 的 system prompt
func (sm *SubagentManager) buildSubagentSystemPrompt(workspace string) string {
    var parts []string

    // 1. 基础身份
    parts = append(parts, `# Subagent

You are a subagent executing a delegated task.
Complete the task independently and report the result.`)

    // 2. 复用 LoadBootstrapFiles 逻辑
    bootstrapFiles := []string{"AGENTS.md", "SOUL.md", "USER.md", "IDENTITY.md"}
    for _, filename := range bootstrapFiles {
        filePath := filepath.Join(workspace, filename)
        if data, err := os.ReadFile(filePath); err == nil && len(data) > 0 {
            parts = append(parts, fmt.Sprintf("## %s\n\n%s", filename, string(data)))
        }
    }

    // 3. 复用 MemoryStore 加载 memory
    memoryStore := agent.NewMemoryStore(workspace)
    if memoryCtx := memoryStore.GetMemoryContext(); memoryCtx != "" {
        parts = append(parts, "# Memory\n\n"+memoryCtx)
    }

    // 4. 复用 SkillsLoader 加载 skills
    skillsLoader := skills.NewSkillsLoader(workspace,
        filepath.Join(getGlobalConfigDir(), "skills"),
        getBuiltinSkillsDir())
    if skillsSummary := skillsLoader.BuildSkillsSummary(); skillsSummary != "" {
        parts = append(parts, "# Skills\n\n"+skillsSummary)
    }

    return strings.Join(parts, "\n\n---\n\n")
}

// 辅助函数
func getGlobalConfigDir() string {
    if home := os.Getenv("PICOCLAW_HOME"); home != "" {
        return home
    }
    home, _ := os.UserHomeDir()
    return filepath.Join(home, ".picoclaw")
}

func getBuiltinSkillsDir() string {
    if dir := os.Getenv("PICOCLAW_BUILTIN_SKILLS"); dir != "" {
        return dir
    }
    wd, _ := os.Getwd()
    return filepath.Join(wd, "skills")
}
```

---

## 依赖关系

```
pkg/tools/subagent.go
├── pkg/skills/loader.go      (复用 SkillsLoader 加载 skills)
├── os.ReadFile                (直接读取 bootstrap .md 文件)
└── pkg/tools/tool_loop.go    (执行工具循环)

pkg/tools/spawn.go
└── pkg/tools/subagent.go     (调用 SubagentManager)

pkg/agent/loop.go
├── pkg/tools/subagent.go     (创建 SubagentManager)
└── pkg/agent/registry.go     (传入 AgentLookup 接口)
```

### 循环依赖解决方案

`pkg/tools` 不能直接导入 `pkg/agent`（会导致循环依赖），因此：

1. **在 `pkg/tools/subagent.go` 中定义接口：**

```go
// AgentLookup 提供通过 agent_id 查询 agent 配置的能力
// 避免 pkg/tools 直接依赖 pkg/agent
type AgentLookup interface {
    LookupAgent(agentID string) (AgentConfig, bool)
}

type AgentConfig struct {
    Workspace   string
    MaxTokens   int
    Temperature float64
    Tools       *ToolRegistry
}
```

2. **在 `pkg/agent/loop.go` 中实现接口：**

```go
// agentLookupAdapter 将 AgentRegistry 适配为 tools.AgentLookup 接口
type agentLookupAdapter struct {
    registry *AgentRegistry
}

func (a *agentLookupAdapter) LookupAgent(agentID string) (tools.AgentConfig, bool) {
    agent, ok := a.registry.GetAgent(agentID)
    if !ok {
        return tools.AgentConfig{}, false
    }
    return tools.AgentConfig{
        Workspace:   agent.Workspace,
        MaxTokens:   agent.MaxTokens,
        Temperature: agent.Temperature,
        Tools:       agent.Tools,
    }, true
}
```

3. **Skills/Memory 直接在 `pkg/tools` 中读取文件：**

```
pkg/tools/subagent.go
├── 直接 import pkg/skills       ← ✅ 无循环依赖
├── 直接 os.ReadFile(.md files)  ← ✅ 无依赖
└── 通过 AgentLookup 接口        ← ✅ 无循环依赖
```

---

## 文件修改清单

| 文件 | 修改类型 | 说明 |
|-----|---------|------|
| `pkg/tools/subagent.go` | 修改 | 新增 `AgentLookup` 接口和 `AgentConfig` 结构体；重构 SubagentManager 添加 `agentLookup` 字段、`resolveConfig()`、`buildSubagentSystemPrompt()`、`buildSubagentSkillsSummary()` 方法；修改 `runTask()` 和 `SubagentTool.Execute()` 使用新逻辑 |
| `pkg/tools/spawn.go` | 修改 | 添加 `model_name` 参数到 `Parameters()`；修改 `execute()` 提取 `modelName` 并传递给 `Spawn()` |
| `pkg/agent/loop.go` | 修改 | 新增 `agentLookupAdapter` 适配器；修改 `registerSharedTools()` 调用 `SetAgentLookup()` |
| `pkg/tools/spawn_test.go` | 修改 | 更新 `Spawn()` 调用签名 |
| `pkg/tools/subagent_tool_test.go` | 修改 | 更新测试用例 |

---

## 向后兼容性

1. **参数变更**：`model_name` 改为必传参数，现有调用需要补充此参数
2. **行为兼容**：`agent_id` 仍为可选参数，不指定时使用父 agent 的配置
3. **默认值**：
   - `agent_id` 未指定：使用父 agent 的 workspace、tools、skills、maxTokens、temperature
   - `model_name`：必须显式指定，无默认值

---

## 测试计划

### 单元测试

1. **TestSpawnToolParameters**
   - 测试 `model_name` 参数缺失时返回错误
   - 测试 `model_name` 参数正确传递

2. **TestSubagentManagerResolveConfig**
   - 测试仅指定 `model_name`（必传）
   - 测试仅指定 `agent_id`（使用目标 agent 的 workspace/tools 等）
   - 测试同时指定两者

3. **TestBuildSubagentSystemPrompt**
   - 测试 workspace 中存在所有 .md 文件
   - 测试 workspace 中缺少部分文件
   - 测试 workspace 为空

4. **TestBuildSubagentSkillsSummary**
   - 测试加载指定 agent 的 skills

### 集成测试

1. **TestSpawnWithModel**
   - 验证 subagent 使用指定模型

2. **TestSpawnWithAgentID**
   - 验证 subagent 使用指定 agent 的 workspace 和 skills

3. **TestSpawnWithBoth**
   - 验证同时指定 model_name 和 agent_id

---

## 风险评估

### 潜在风险

1. ~~**循环依赖**~~ ✅ 已解决
   - ~~`pkg/tools` 导入 `pkg/agent` 可能导致循环依赖~~
   - **解决方案**：通过 `AgentLookup` 接口解耦，在 `pkg/tools` 定义接口，在 `pkg/agent` 实现

2. **性能影响**
   - 每次创建 subagent 都要读取文件
   - **解决方案**：当前实现不添加缓存，保持简单；如有需要可后续添加

3. **安全隔离**
   - subagent 访问其他 agent 的 workspace
   - **解决方案**：保持现有的 allowlist 检查机制（在 SpawnTool 中）

4. **接口一致性**
   - `AgentLookup` 接口需要与 `AgentInstance` 字段保持同步
   - **解决方案**：在 `AgentConfig` 结构体中只包含必要字段，减少耦合

### 缓解措施

1. 通过 `AgentLookup` 接口解耦，避免循环依赖
2. `AgentConfig` 结构体只包含必要字段（Workspace, MaxTokens, Temperature, Tools），减少与 `AgentInstance` 的耦合
3. 保留 allowlist 检查，防止未授权访问其他 agent 的 workspace
4. Skills 和 Memory 加载直接使用 `pkg/skills` 包和文件读取，无需依赖 `pkg/agent`

---

## 实现顺序

1. **Phase 1**: 扩展 SpawnTool 参数（低风险）
2. **Phase 2**: 扩展 SubagentManager（核心修改）
3. **Phase 3**: 修改 AgentLoop 初始化
4. **Phase 4**: 同步修改 SubagentTool
5. **Phase 5**: 更新测试用例
   6. **Phase 6**: 集成测试

---

## 预期效果

完成后，可以这样调用 spawn：

```json
{
  "task": "分析代码库结构",
  "label": "代码分析",
  "agent_id": "research-agent",
  "model_name": "claude-sonnet-4-6"
}
```

subagent 将：
1. 使用 `claude-sonnet-4-6` 模型（由 `model_name` 参数指定，必传）
2. 从 `research-agent` 的 workspace 加载上下文
3. 加载 `research-agent` 的 skills（基于 workspace 路径）
4. 使用 `research-agent` 的 tools（read_file, write_file, exec 等已注册的工具）
5. 使用 `research-agent` 的 temperature 和 max_tokens 配置

如果不指定 `agent_id`，subagent 将使用父 agent 的所有配置（workspace、tools、skills、maxTokens、temperature）。
`model_name` 始终为必传参数，不从任何 agent 配置中继承。
