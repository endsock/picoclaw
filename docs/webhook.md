# 通过 HTTP API 触发 Spawn，并以 webhook / 查询接口获取结果的改造方案

## 目标

这次方案的目标明确分成两类场景，且二者的结果回传机制不同：

1. **内部 agent spawn**
   - 保持现有机制：`callback -> system message -> 找回父 agent -> 父 agent 继续处理`
   - **不做降级回原会话**
   - 父 agent 必须存在，这是系统约束；如果不存在，视为系统异常而不是业务分支

2. **POST API spawn**
   - 提供 `POST /api/spawn` 创建异步 subagent 任务
   - **不使用 callback，不依赖父 agent**
   - 如果请求里提供了 webhook，则任务结束时调用 webhook
   - 如果没有提供 webhook，则不做任何主动通知
   - `GET /api/spawn/{task_id}` 仅作为人工查看任务状态的接口

因此，这次改造的正确方向不是“HTTP 层去模拟一次 tool callback 会话”，而是：

- **内部 spawn 继续走父 agent 回流机制**
- **外部 API spawn 走 webhook / GET 查询机制**
- **二者共用同一个 `SubagentManager` 执行内核**

---

## 当前实现现状

### 1. 真正的异步执行入口是 `SubagentManager.Spawn()`

当前创建后台 subagent 的核心入口是：

- `pkg/tools/subagent.go:130`

```go
func (sm *SubagentManager) Spawn(
    ctx context.Context,
    task, label, agentID, modelName, originChannel, originChatID string,
    callback AsyncCallback,
) (string, error)
```

`SpawnTool` 只是参数校验和上下文适配层：

- `pkg/tools/spawn.go:73`
- `pkg/tools/spawn.go:111`

```go
result, err := t.manager.Spawn(ctx, task, label, agentID, modelName, channel, chatID, cb)
```

所以系统设计上应当继续把：

- `SpawnTool` 视为 **agent 内部入口**
- `SubagentManager` 视为 **统一执行内核**

### 2. 当前内部 spawn 的结果回流机制已经是完整链路

内部 agent spawn 当前链路是：

```text
父 agent 发起 spawn
    ↓
SpawnTool.ExecuteAsync(...)
    ↓
SubagentManager.Spawn(..., callback)
    ↓
subagent 后台执行
    ↓
callback(result)
    ↓
system inbound message
    ↓
processSystemMessage()
    ↓
根据 parent_agent_id 找回父 agent
    ↓
父 agent 基于原 session 继续 runAgentLoop
```

关键位置：

- `pkg/agent/loop.go:1352-1407`：构造 async callback，并把 `parent_agent_id / parent_session_key / origin_channel / origin_chat_id` 写入 system message payload
- `pkg/agent/loop.go:872-910`：收到 system message 后恢复父 agent 与原 session

### 3. 这里有一个必须明确的系统约束

对于**内部 agent spawn**，本方案不引入“父 agent 缺失时回原会话”的降级处理。

原因是：

- 内部 spawn 本质上是父 agent 的异步续跑机制
- 它的设计前提就是“父 agent 会回来接结果”
- 如果 `parent_agent_id` 在 callback 回来时不存在，说明系统状态异常，而不是正常业务分支

所以在设计上应当明确：

> 内部 spawn 的父 agent 必须存在；若 `processSystemMessage()` 找不到 `parent_agent_id`，记录错误并终止该次回流，不做业务降级。

### 4. 当前 HTTP 服务挂载点已经具备扩展能力

现有 gateway 已提供共享 HTTP server：

- `cmd/picoclaw/internal/gateway/helpers.go:250-255`
- `pkg/channels/manager.go` 中的 `HandleHTTPFunc(...)`

这使得新增：

- `POST /api/spawn`
- `GET /api/spawn/{task_id}`

都可以自然挂在 shared HTTP server 上。

---

## 推荐方案总览

推荐拆成 4 层：

1. **任务模型扩展**：给 `SubagentTask` 增加来源、错误、完成时间、metadata、webhook 等字段
2. **执行入口扩展**：新增 `SpawnRequest` / `SpawnWithRequest(...)`
3. **HTTP API**：新增 `POST /api/spawn` 和 `GET /api/spawn/{task_id}`
4. **Manager 注册与查询**：让 gateway 能按 agent 找到对应的 `SubagentManager`

核心原则：

- **内部 spawn**：继续依赖 callback 回父 agent
- **HTTP spawn**：不依赖 callback
- **webhook 是 HTTP spawn 的可选通知机制，不替代内部 callback**
- **GET 查询接口仅用于人工查看任务状态，不参与自动回调链路**

---

## 一、接口设计

## 1. 创建任务接口

建议新增：

```text
POST /api/spawn
```

### 请求体建议

```json
{
  "task": "分析当前仓库中 spawn 与 subagent 的调用链",
  "label": "spawn-analysis",
  "agent_id": "research-agent",
  "model_name": "claude-sonnet-4-6",
  "metadata": {
    "biz_id": "order-9527",
    "triggered_by": "workflow-engine"
  },
  "webhook": {
    "url": "http://callback.service.local/subagent",
    "headers": {
      "X-Biz-App": "ops-center"
    },
    "events": ["completed", "failed", "canceled"],
    "timeout_ms": 5000,
    "max_retries": 3
  }
}
```

### 字段说明

#### 基础执行字段

- `task`: 必填，subagent 的任务内容
- `label`: 可选，短标签
- `agent_id`: 可选，目标 agent；为空时使用默认 agent 对应的 manager
- `model_name`: 必填，保持与现有 `SpawnTool` 一致
- `metadata`: 可选，原样透传到 webhook 和 GET 查询结果，便于调用方关联业务上下文

#### webhook 字段

- `webhook.url`: 可选
- `webhook.headers`: 可选
- `webhook.events`: 可选，默认 `completed/failed/canceled`
- `webhook.timeout_ms`: 可选，默认取配置值
- `webhook.max_retries`: 可选，默认取配置值

### 返回体建议

接口只返回“已受理”，不等待 subagent 完成：

```json
{
  "task_id": "subagent-12",
  "status": "accepted",
  "message": "Spawned subagent 'spawn-analysis'",
  "webhook_registered": true,
  "result_url": "/api/spawn/subagent-12"
}
```

### HTTP 状态码建议

- `202 Accepted`：成功入队
- `400 Bad Request`：参数错误
- `404 Not Found`：任务不存在
- `429 Too Many Requests`：队列已满
- `500 Internal Server Error`：内部错误

---

## 2. 查询结果接口

新增：

```text
GET /api/spawn/{task_id}
```

用于：

- 人工查看任务当前状态
- webhook 之外的补充排查
- 调试 / 运维查看任务状态

### 返回体建议

#### 运行中

```json
{
  "task_id": "subagent-12",
  "status": "running",
  "label": "spawn-analysis",
  "agent_id": "research-agent",
  "model_name": "claude-sonnet-4-6",
  "source": "api",
  "metadata": {
    "biz_id": "order-9527"
  },
  "created_at_ms": 1710000000000,
  "finished_at_ms": 0,
  "result": "",
  "error": ""
}
```

#### 成功完成

```json
{
  "task_id": "subagent-12",
  "status": "completed",
  "label": "spawn-analysis",
  "agent_id": "research-agent",
  "model_name": "claude-sonnet-4-6",
  "source": "api",
  "metadata": {
    "biz_id": "order-9527"
  },
  "created_at_ms": 1710000000000,
  "finished_at_ms": 1710000005234,
  "duration_ms": 5234,
  "result": "...",
  "error": ""
}
```

#### 失败 / 取消

```json
{
  "task_id": "subagent-12",
  "status": "failed",
  "label": "spawn-analysis",
  "agent_id": "research-agent",
  "model_name": "claude-sonnet-4-6",
  "source": "api",
  "metadata": {
    "biz_id": "order-9527"
  },
  "created_at_ms": 1710000000000,
  "finished_at_ms": 1710000005234,
  "duration_ms": 5234,
  "result": "",
  "error": "Error: ..."
}
```

### GET 的语义建议

- `404`：`task_id` 不存在
- `200`：存在即返回，不区分是否完成
- 响应中通过 `status` 区分 `running/completed/failed/canceled`

---

## 二、内部模型设计

## 1. API 请求模型

建议新增：

```go
type SpawnAPIRequest struct {
    Task      string            `json:"task"`
    Label     string            `json:"label,omitempty"`
    AgentID   string            `json:"agent_id,omitempty"`
    ModelName string            `json:"model_name"`
    Metadata  map[string]string `json:"metadata,omitempty"`
    Webhook   *SpawnWebhookSpec `json:"webhook,omitempty"`
}

type SpawnWebhookSpec struct {
    URL        string            `json:"url"`
    Headers    map[string]string `json:"headers,omitempty"`
    Events     []string          `json:"events,omitempty"`
    TimeoutMS  int               `json:"timeout_ms,omitempty"`
    MaxRetries int               `json:"max_retries,omitempty"`
}
```

## 2. 统一执行请求模型

建议新增：

```go
type SpawnRequest struct {
    Task      string
    Label     string
    AgentID   string
    ModelName string
    Source    string            // tool | api
    Metadata  map[string]string
    Webhook   *SubagentWebhook
}
```

其中：

- 内部 `SpawnTool` 调 `Spawn(...)` 时，包装成 `Source=tool`
- HTTP API 调 `SpawnWithRequest(...)` 时，显式传 `Source=api`

## 3. 任务模型扩展

当前 `SubagentTask` 只有基础字段，不够支撑 webhook 与 GET 查询。

建议扩展为：

```go
type SubagentTask struct {
    ID         string
    Task       string
    Label      string
    AgentID    string
    ModelName  string
    Status     string
    Result     string
    Error      string
    Created    int64
    Finished   int64

    Source     string            // tool | api
    Metadata   map[string]string
    Webhook    *SubagentWebhook
}
```

新增：

```go
type SubagentWebhook struct {
    URL        string
    Headers    map[string]string
    Events     map[string]bool
    TimeoutMS  int
    MaxRetries int
}
```

### 为什么这样设计

因为同一个 `SubagentManager` 可能同时跑：

- 内部 agent spawn（有 callback，无 webhook）
- API spawn A（无 callback，有 webhook A）
- API spawn B（无 callback，无 webhook，只能 GET 查询）

所以 webhook、metadata、source 必须是**任务实例级属性**，不能是 manager 级全局配置。

---

## 三、为什么 HTTP 层不要直接调用 `SpawnTool.Execute()`

仍然不建议在 HTTP handler 里直接构造 `map[string]any` 去调用 `SpawnTool`，原因不变：

1. `SpawnTool` 依赖 tool context
   - `ToolChannel(ctx)`
   - `ToolChatID(ctx)`
   - 见 `pkg/tools/spawn.go:98-108`

2. `SpawnTool` 的 allowlist 语义是“父 agent -> 子 agent”
   - 见 `pkg/agent/loop.go:250-255`
   - API 请求没有父 agent，不应复用这套语义

3. 真正公共能力本来就在 manager
   - `SpawnTool` 只是内部 adapter
   - `SubagentManager` 才是执行内核

所以推荐保持：

- `SpawnTool`：内部入口
- `SpawnWithRequest(...)`：HTTP 入口

---

## 四、SubagentManager 的改造建议

## 1. 新增 `SpawnWithRequest(...)`

当前 `Spawn(...)` 参数已经开始膨胀，不适合继续往下塞 webhook / metadata / source。

建议新增：

```go
func (sm *SubagentManager) SpawnWithRequest(
    ctx context.Context,
    req SpawnRequest,
    callback AsyncCallback,
) (taskID string, message string, err error)
```

同时保留原有兼容包装：

```go
func (sm *SubagentManager) Spawn(
    ctx context.Context,
    task, label, agentID, modelName, originChannel, originChatID string,
    callback AsyncCallback,
) (string, error)
```

内部可以简单转：

```go
_, msg, err := sm.SpawnWithRequest(ctx, SpawnRequest{
    Task:      task,
    Label:     label,
    AgentID:   agentID,
    ModelName: modelName,
    Source:    "tool",
}, callback)
return msg, err
```

### 注意

这里**不要破坏当前内部 callback 机制**。

也就是说：

- 原 `Spawn()` 保持给内部 tool 使用
- callback 继续按当前逻辑传入 `runTask()`
- API spawn 才传 `callback=nil`

## 2. 增加任务查询方法

当前已有：

- `pkg/tools/subagent.go:391` `GetTask(taskID string)`
- `pkg/tools/subagent.go:398` `ListTasks()`

这很好，`GET /api/spawn/{task_id}` 可以直接复用 `GetTask()`。

如果后面要做多 agent manager 聚合查询，可以再在 `AgentLoop` 层包一层任务路由查询。

---

## 五、内部 callback 机制的设计结论

这一部分需要明确写死，避免后续实现偏掉。

## 1. 内部 agent spawn 的正确语义

内部 spawn 必须保持：

```text
callback -> system message -> 恢复父 agent -> 原 session 继续执行
```

即：

- `callback` 是内部系统机制
- 它的作用不是通知外部系统，而是让父 agent 接上异步结果
- 它依赖 `parent_agent_id` 和 `parent_session_key`

## 2. 不做“父 agent 缺失时回原会话”的降级

本方案明确**不采用**这类降级处理。

原因：

- 这会把内部 spawn 的语义从“父 agent 续跑”污染成“聊天消息兜底投递”
- 对 API spawn 根本不适用，因为 API spawn 没有聊天会话
- 会让 `origin_channel / origin_chat_id` 语义混乱

所以应当明确：

> 对内部 spawn，`processSystemMessage()` 找不到 `parent_agent_id` 时，按系统错误处理，记录日志并返回错误；不做任何降级投递。

## 3. API spawn 不使用 callback

对于 `POST /api/spawn`，建议固定采用：

- `callback = nil`
- webhook 可选
- `GET /api/spawn/{task_id}` 仅用于人工查看状态

即：

- 没有父 agent
- 没有原对话 session
- 不尝试复用内部 system message 回流机制

这也是本方案最重要的边界之一。

---

## 六、webhook 设计

## 1. 投递时机

最佳位置仍然是 `SubagentManager.runTask()` 的统一收口点：

- `pkg/tools/subagent.go:347-388`

在这里可以拿到：

- 最终状态：`completed/failed/canceled`
- 最终结果：`Result`
- 最终错误：`Error`

建议流程：

1. 更新 task 状态字段
2. 如果 `callback != nil`，走内部 callback
3. 如果 `task.Webhook != nil` 且事件匹配，发送 webhook

### callback 与 webhook 的关系

- `callback`：内部父 agent 回流
- `webhook`：外部系统通知

两者并行，不互相替代。

## 2. webhook payload 建议

```go
type SpawnWebhookPayload struct {
    Version      int               `json:"version"`
    Event        string            `json:"event"`
    TaskID       string            `json:"task_id"`
    Label        string            `json:"label,omitempty"`
    AgentID      string            `json:"agent_id,omitempty"`
    ModelName    string            `json:"model_name"`
    Status       string            `json:"status"`
    Source       string            `json:"source"`
    Metadata     map[string]string `json:"metadata,omitempty"`
    Result       string            `json:"result,omitempty"`
    Error        string            `json:"error,omitempty"`
    CreatedAtMS  int64             `json:"created_at_ms"`
    FinishedAtMS int64             `json:"finished_at_ms"`
    DurationMS   int64             `json:"duration_ms"`
}
```

### event / status

统一使用：

- `completed`
- `failed`
- `canceled`

不再额外引入 `finished` 事件层。

## 3. webhook 发送策略

建议新增独立模块，例如：

```text
pkg/webhook/sender.go
```

负责：

- JSON 编码
- timeout
- retries
- 发送日志

### 请求约定

- 方法：`POST`
- `Content-Type: application/json`
- `User-Agent: picoclaw-webhook/1.0`
- `X-PicoClaw-Event: completed|failed|canceled`
- `X-PicoClaw-Task-ID: subagent-12`
- 调用方自定义 headers

### 重试建议

- 网络错误、5xx：重试
- 4xx：默认不重试
- 退避：1s / 2s / 4s

### webhook 失败如何处理

**不改变主任务状态。**

也就是：

- subagent 已完成就按真实状态保存
- webhook 只是附加通知
- webhook 失败只记日志，必要时可扩展记录 `WebhookAttempts / WebhookLastError`

---

## 七、HTTP API 的挂载方案

建议新增文件：

```text
cmd/picoclaw/internal/gateway/spawn_api.go
```

并在 gateway shared HTTP server 初始化后注册：

- `POST /api/spawn`
- `GET /api/spawn/{task_id}`

对应现有挂载点：

- `cmd/picoclaw/internal/gateway/helpers.go:250-255`
- `cmd/picoclaw/internal/gateway/helpers.go:500-504`

建议和 `registerWorkerQueueDebugRoute(...)` 并列新增：

```go
registerSpawnAPIRoutes(services, agentLoop)
```

---

## 八、AgentLoop 需要暴露 manager 查询能力

为了让 gateway handler 能按 `agent_id` 找到对应的 `SubagentManager`，建议在 `AgentLoop` 增加 manager 注册表，例如：

```go
type AgentLoop struct {
    ...
    subagentManagers map[string]*tools.SubagentManager
}
```

在 `registerSharedTools()` 创建 manager 时登记：

- key: 当前 `agentID`
- value: 当前 agent 对应的 `SubagentManager`

并提供辅助方法，例如：

```go
func (al *AgentLoop) GetSubagentManager(agentID string) (*tools.SubagentManager, bool)
func (al *AgentLoop) GetDefaultSubagentManager() (*tools.SubagentManager, string, bool)
```

这样：

- `POST /api/spawn` 可以按 `agent_id` 找 manager
- `GET /api/spawn/{task_id}` 可以遍历 manager 查任务，或维护全局 task 索引

### 关于 GET 查询的实现选择

有两种方案：

#### 方案 A：遍历所有 manager 查任务

优点：
- 改动小
- 复用现有 `GetTask()`

缺点：
- 每次查询需要扫描全部 manager

#### 方案 B：AgentLoop 维护全局 task -> manager 索引

优点：
- 查询直接命中

缺点：
- 需要同步维护索引

**第一版推荐方案 A**，因为 agent 数量通常有限，优先最小改动。

---

## 九、handler 执行流程

## 1. POST /api/spawn

建议流程：

```text
校验方法 POST
    ↓
解析 JSON
    ↓
校验 task / model_name / webhook
    ↓
按 agent_id 找 SubagentManager
    ↓
构造 SpawnRequest{Source: "api"}
    ↓
manager.SpawnWithRequest(ctx, req, nil)
    ↓
返回 202 + task_id + result_url
```

### 伪代码

```go
func registerSpawnAPIRoutes(services *gatewayServices, agentLoop *agent.AgentLoop) {
    if services == nil || services.ChannelManager == nil {
        return
    }

    services.ChannelManager.HandleHTTPFunc("/api/spawn", func(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodPost {
            http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
            return
        }

        var req SpawnAPIRequest
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

        taskID, msg, err := manager.SpawnWithRequest(r.Context(), SpawnRequest{
            Task:      req.Task,
            Label:     req.Label,
            AgentID:   resolvedAgentID,
            ModelName: req.ModelName,
            Source:    "api",
            Metadata:  req.Metadata,
            Webhook:   buildWebhook(req.Webhook),
        }, nil)
        if err != nil {
            writeSpawnError(w, err)
            return
        }

        w.Header().Set("Content-Type", "application/json")
        w.WriteHeader(http.StatusAccepted)
        _ = json.NewEncoder(w).Encode(map[string]any{
            "task_id":            taskID,
            "status":             "accepted",
            "message":            msg,
            "webhook_registered": req.Webhook != nil,
            "result_url":         "/api/spawn/" + taskID,
        })
    })
}
```

## 2. GET /api/spawn/{task_id}

这个接口的定位是：**手工查看任务状态**。

建议流程：

```text
校验方法 GET
    ↓
解析 task_id
    ↓
查询各 manager 的 GetTask(taskID)
    ↓
不存在则 404
    ↓
存在则返回任务详情 JSON
```

### 伪代码

```go
services.ChannelManager.HandleHTTPFunc("/api/spawn/", func(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodGet {
        http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
        return
    }

    taskID := strings.TrimPrefix(r.URL.Path, "/api/spawn/")
    if taskID == "" {
        http.Error(w, "task_id is required", http.StatusBadRequest)
        return
    }

    task, ok := findSubagentTask(agentLoop, taskID)
    if !ok {
        http.NotFound(w, r)
        return
    }

    w.Header().Set("Content-Type", "application/json")
    _ = json.NewEncoder(w).Encode(buildSpawnTaskResponse(task))
})
```

---

## 十、配置建议

由于这是一个内网调用、不对外暴露的服务接口，这版方案**不引入鉴权、签名、SSRF 防护等安全设计**，只保留与功能直接相关的运行配置。

建议在 `pkg/config/config.go` 的 `GatewayConfig` 下新增：

```go
type SpawnAPIConfig struct {
    Enabled bool `json:"enabled"`
}

type OutboundWebhookConfig struct {
    DefaultTimeoutMS  int `json:"default_timeout_ms"`
    DefaultMaxRetries int `json:"default_max_retries"`
    MaxPayloadBytes   int `json:"max_payload_bytes"`
}
```

挂到：

```go
type GatewayConfig struct {
    Host string `json:"host"`
    Port int    `json:"port"`

    SpawnAPI        SpawnAPIConfig        `json:"spawn_api"`
    OutboundWebhook OutboundWebhookConfig `json:"outbound_webhook"`
}
```

### 默认值建议

```json
{
  "gateway": {
    "host": "0.0.0.0",
    "port": 8080,
    "spawn_api": {
      "enabled": false
    },
    "outbound_webhook": {
      "default_timeout_ms": 5000,
      "default_max_retries": 3,
      "max_payload_bytes": 65536
    }
  }
}
```

### 响应大小控制

建议对 webhook 和 GET 返回中的 `result` 做上限控制，例如 64KB。

超过则：

- 截断 `result`
- 增加 `result_truncated=true`
- 增加 `result_size`

避免回调或查询响应过大。

---

## 十一、状态与事件映射

建议统一使用：

- `running`
- `completed`
- `failed`
- `canceled`

### 完成

```json
{
  "event": "completed",
  "status": "completed",
  "result": "...",
  "error": ""
}
```

### 失败

```json
{
  "event": "failed",
  "status": "failed",
  "result": "",
  "error": "Error: ..."
}
```

### 取消

```json
{
  "event": "canceled",
  "status": "canceled",
  "result": "",
  "error": "Task canceled during execution"
}
```

### 入队失败

**不进入 webhook 生命周期，也不会生成可查询任务。**

也就是说：

- `POST /api/spawn` 若返回 4xx/5xx/429，则说明任务未成功创建
- 只有返回了 `202 Accepted` 和 `task_id`，调用方才可以 webhook 等待或 GET 查询

---

## 十二、最小文件改动清单

## 1. 修改 `pkg/tools/subagent.go`

需要做的事：

1. 扩展 `SubagentTask`
2. 新增 `SpawnRequest`
3. 新增 `SubagentWebhook`
4. 新增 `SpawnWithRequest(...)`
5. 保留原 `Spawn(...)` 兼容包装
6. 在 `runTask()` 收口处接入 webhook 投递
7. 保持 callback 逻辑不变

关键点：

- **不要破坏内部 callback**
- **API spawn 通过 `callback=nil` 区分**

## 2. 新增 `pkg/webhook/sender.go`

负责：

- JSON 编码
- 超时
- 重试
- 发送日志

## 3. 修改 `pkg/agent/loop.go`

需要做的事：

- 增加 `subagentManagers map[string]*tools.SubagentManager`
- 在 `registerSharedTools()` 创建 manager 时登记
- 提供 manager 查询方法
- **不修改内部 `processSystemMessage()` 的基本语义**
- 保持“找回父 agent”是内部 spawn 的唯一结果回流机制

## 4. 新增 `cmd/picoclaw/internal/gateway/spawn_api.go`

负责：

- 注册 `POST /api/spawn`
- 注册 `GET /api/spawn/{task_id}`
- 解析 JSON
- 校验参数
- 查 manager
- 创建任务
- 查询任务

## 5. 修改 `cmd/picoclaw/internal/gateway/helpers.go`

在：

- `cmd/picoclaw/internal/gateway/helpers.go:254`
- `cmd/picoclaw/internal/gateway/helpers.go:504`

与 `registerWorkerQueueDebugRoute(services)` 并列增加：

```go
registerSpawnAPIRoutes(services, agentLoop)
```

## 6. 修改 `pkg/config/config.go` / `pkg/config/defaults.go`

新增：

- `gateway.spawn_api`
- `gateway.outbound_webhook`

---

## 十三、数据流总结

## 1. 内部 agent spawn

```text
父 agent 调用 spawn tool
    ↓
SpawnTool.ExecuteAsync(...)
    ↓
SubagentManager.Spawn(..., callback)
    ↓
subagent 后台执行
    ↓
callback(result)
    ↓
system inbound message
    ↓
processSystemMessage()
    ↓
根据 parent_agent_id + parent_session_key 恢复父 agent 与原 session
    ↓
父 agent 继续处理结果
```

设计要求：

- 必须有父 agent
- 不做降级投递
- 找不到父 agent 视为系统错误

## 2. POST API spawn（有 webhook）

```text
外部系统 POST /api/spawn
    ↓
handler 解析、校验、创建任务
    ↓
返回 202 + task_id
    ↓
subagent 后台执行
    ↓
runTask() 结束
    ↓
发送 webhook
```

## 3. POST API spawn（无 webhook）

```text
外部系统 POST /api/spawn
    ↓
handler 解析、校验、创建任务
    ↓
返回 202 + task_id
    ↓
subagent 后台执行
    ↓
任务结束，不做任何主动通知
（可通过 GET /api/spawn/{task_id} 手工查看状态）
```

---

## 十四、测试计划

## 1. `pkg/tools/subagent.go`

新增测试点：

- `TestSpawnWithRequestStoresAPIMetadata`
- `TestSpawnWithRequestPreservesToolCallbackBehavior`
- `TestRunTaskDispatchesCompletedWebhook`
- `TestRunTaskDispatchesFailedWebhook`
- `TestRunTaskDispatchesCanceledWebhook`
- `TestRunTaskWebhookFailureDoesNotChangeTaskStatus`

重点验证：

- 内部 tool spawn 仍然走 callback
- API spawn 可不带 callback
- webhook 不影响任务真实状态

## 2. `pkg/webhook/sender.go`

新增测试点：

- `TestSenderRetriesOn5xx`
- `TestSenderDoesNotRetryOn4xx`
- `TestSenderSetsHeaders`

## 3. gateway API 集成测试

新增测试点：

- `TestSpawnAPI_AcceptsRequest`
- `TestSpawnAPI_RejectsInvalidWebhookURL`
- `TestSpawnAPI_QueueFullReturns429`
- `TestSpawnAPI_GetTaskReturnsRunning`
- `TestSpawnAPI_GetTaskReturnsCompletedResult`
- `TestSpawnAPI_GetTaskReturns404ForUnknownTask`

---

## 十五、推荐实施顺序

### 第 1 步

先改 `pkg/tools/subagent.go`：

- 增加 `SpawnRequest`
- 扩展 `SubagentTask`
- 增加 `SpawnWithRequest()`
- 保持旧 `Spawn()` 兼容

### 第 2 步

新增 `pkg/webhook/sender.go`：

- 先完成最小可用投递
- 默认超时
- 默认有限重试

### 第 3 步

在 `runTask()` 收口处接入 webhook 分发。

### 第 4 步

让 `AgentLoop` 暴露各 agent 对应的 `SubagentManager`。

### 第 5 步

新增 gateway 路由：

- `POST /api/spawn`
- `GET /api/spawn/{task_id}`

### 第 6 步

补测试。

---

## 十六、结论

针对这次需求，正确设计应当是：

### 内部 agent spawn

- 保持现有 `callback -> system message -> 父 agent` 机制
- 不做降级处理
- 父 agent 必须存在，这是系统约束

### POST API spawn

- 新增 `POST /api/spawn`
- **不使用 callback，不依赖父 agent**
- 有 webhook 就回调
- 没有 webhook 就不做任何主动通知
- `GET /api/spawn/{task_id}` 仅用于人工查看状态

### 执行内核

- HTTP spawn 与内部 spawn 继续共用 `SubagentManager`
- `SpawnTool` 继续作为内部 tool 入口
- `SpawnWithRequest(...)` 作为 HTTP 入口

### 结果获取机制

- 内部 spawn：父 agent 异步续跑
- API spawn：若配置 webhook，则通过 webhook 通知；`GET /api/spawn/{task_id}` 仅用于人工查看状态

### 最关键的一句话

> 这次改造的核心，不是把 SpawnTool 暴露成 HTTP 接口，而是把 SubagentManager 作为统一异步执行内核，同时明确区分两套结果回传机制：内部 spawn 走 callback 回父 agent，外部 API spawn 走 webhook / GET 查询。
