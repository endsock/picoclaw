# 通过 POST 接口触发 SpawnTool，并在 subagent 正常/异常结束时回调 webhook 的改造方案

## 目标

你希望新增一条 **HTTP POST 接口**，让外部系统可以直接触发一次 `spawn` 异步 subagent 执行；同时在 subagent **正常结束**、**失败结束**、**取消结束** 时，系统自动调用用户提供的 `webhook`。

要求拆开看，其实是两件事：

1. **入口改造**：把现有仅能由 agent 内部 tool call 触发的 `SpawnTool` / `SubagentManager.Spawn()`，暴露为一个服务端 POST API。
2. **回调改造**：在 `SubagentManager.runTask()` 的生命周期收口点，增加统一的 webhook 投递能力。

基于当前代码，最小改造且语义最稳的方案是：

- **不要真的从 HTTP 层去“调用 SpawnTool”对象本身**
- 而是新增一个独立的 **spawn service / spawn API handler**，它直接调用 `SubagentManager.Spawn(...)`
- `SpawnTool` 继续服务于 agent 内部工具调用
- HTTP 接口与 tool 共用同一个 `SubagentManager`
- webhook 回调能力下沉到 `SubagentManager` / `SubagentTask` 层

这样可以最大限度复用现有异步执行链路，同时避免把 HTTP 请求强行伪装成 agent tool 上下文。

---

## 当前实现现状

### 1. spawn 的真实执行入口

当前真正创建后台 subagent 的核心方法是：

- `pkg/tools/subagent.go:130`

```go
func (sm *SubagentManager) Spawn(
    ctx context.Context,
    task, label, agentID, modelName, originChannel, originChatID string,
    callback AsyncCallback,
) (string, error)
```

`SpawnTool` 本质只是参数校验 + 上下文读取 + 转调 manager：

- `pkg/tools/spawn.go:73`
- `pkg/tools/spawn.go:111`

```go
result, err := t.manager.Spawn(ctx, task, label, agentID, modelName, channel, chatID, cb)
```

所以从系统设计上说，**`SubagentManager.Spawn()` 才是公共能力，`SpawnTool` 只是其中一个入口适配层**。

### 2. subagent 结束后的统一收口点

subagent 执行完成后的状态收口，都在：

- `pkg/tools/subagent.go:285`
- `pkg/tools/subagent.go:357-388`

这里会统一把任务标记为：

- `completed`
- `failed`
- `canceled`

并构造 `ToolResult`，最后调用 callback：

```go
defer func() {
    sm.mu.Unlock()
    if callback != nil && result != nil {
        callback(ctx, result)
    }
}()
```

这正是增加 webhook 回调的最佳位置，因为：

- 所有结束路径都会经过这里
- 能拿到最终状态
- 能拿到最终结果 / 错误
- 能统一处理成功、失败、取消

### 3. 当前 HTTP 服务挂载点

项目里已有共享 HTTP 服务能力：

- gateway 共享服务启动：`cmd/picoclaw/internal/gateway/helpers.go:250-255`
- 共享 mux：`pkg/channels/manager.go:320-365`

尤其是：

```go
func (m *Manager) HandleHTTPFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
```

这意味着在 gateway 模式下，新增一个 POST 接口非常自然，直接挂到 shared HTTP server 即可。

---

## 推荐方案总览

推荐拆成 4 层改造：

1. **任务模型扩展**：给 `SubagentTask` 增加 webhook 配置、任务来源、请求 ID 等元数据
2. **webhook 投递器**：新增一个专门负责签名、超时、重试、投递的 sender
3. **HTTP POST 接口**：新增 `POST /api/spawn` 或 `POST /v1/spawn`
4. **Manager 生命周期集成**：HTTP 接口创建任务；任务结束时统一发 webhook

核心原则：

- **HTTP API 只负责入队，不等待执行完成**
- **webhook 才是异步结果通知机制**
- **agent 内部 spawn 与 HTTP spawn 共用同一执行引擎**
- **不要把 webhook 逻辑散落到 agent loop 和 tool callback 两边**

---

## 一、接口与模型设计

## 1. 新增 POST 接口

建议新增：

```text
POST /api/spawn
```

如果你希望更偏开放 API 风格，也可以用：

```text
POST /v1/subagents/spawn
```

但结合当前 web backend 已有 `/api/...` 风格，建议优先：

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
  "origin_channel": "api",
  "origin_chat_id": "external-job-123",
  "metadata": {
    "biz_id": "order-9527",
    "triggered_by": "workflow-engine"
  },
  "webhook": {
    "url": "https://example.com/callback/subagent",
    "secret": "your-shared-secret",
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
- `agent_id`: 可选，目标 agent
- `model_name`: 必填，模型名，保持与现有 `SpawnTool` 一致
- `origin_channel`: 可选，默认 `api`
- `origin_chat_id`: 可选，默认自动生成一个 request/task 关联 ID
- `metadata`: 可选，原样带回 webhook，便于调用方对账

#### webhook 字段

- `webhook.url`: 必填，回调地址
- `webhook.secret`: 可选，用于签名
- `webhook.headers`: 可选，附加 HTTP header
- `webhook.events`: 可选，默认 `completed/failed/canceled`
- `webhook.timeout_ms`: 可选，默认 5000
- `webhook.max_retries`: 可选，默认 0 或 3，二选一要明确

### 返回体建议

接口只返回“已受理”，不等待执行完成：

```json
{
  "task_id": "subagent-12",
  "status": "accepted",
  "message": "Spawned subagent 'spawn-analysis'",
  "webhook_registered": true
}
```

HTTP 状态码建议：

- `202 Accepted`：成功入队
- `400 Bad Request`：参数错误
- `403 Forbidden`：`agent_id` 不允许
- `429 Too Many Requests`：队列已满 / 服务繁忙
- `500 Internal Server Error`：内部错误

---

## 2. 新增内部请求/回调模型

建议在新增模块中定义：

```go
type SpawnAPIRequest struct {
    Task          string            `json:"task"`
    Label         string            `json:"label,omitempty"`
    AgentID       string            `json:"agent_id,omitempty"`
    ModelName     string            `json:"model_name"`
    OriginChannel string            `json:"origin_channel,omitempty"`
    OriginChatID  string            `json:"origin_chat_id,omitempty"`
    Metadata      map[string]string `json:"metadata,omitempty"`
    Webhook       *SpawnWebhookSpec `json:"webhook,omitempty"`
}

type SpawnWebhookSpec struct {
    URL        string            `json:"url"`
    Secret     string            `json:"secret,omitempty"`
    Headers    map[string]string `json:"headers,omitempty"`
    Events     []string          `json:"events,omitempty"`
    TimeoutMS  int               `json:"timeout_ms,omitempty"`
    MaxRetries int               `json:"max_retries,omitempty"`
}
```

以及 webhook 投递的 payload：

```go
type SpawnWebhookPayload struct {
    Version      int               `json:"version"`
    Event        string            `json:"event"`
    TaskID       string            `json:"task_id"`
    Label        string            `json:"label,omitempty"`
    AgentID      string            `json:"agent_id,omitempty"`
    ModelName    string            `json:"model_name"`
    Status       string            `json:"status"`
    OriginChannel string           `json:"origin_channel,omitempty"`
    OriginChatID string            `json:"origin_chat_id,omitempty"`
    Metadata     map[string]string `json:"metadata,omitempty"`
    Result       string            `json:"result,omitempty"`
    Error        string            `json:"error,omitempty"`
    CreatedAtMS  int64             `json:"created_at_ms"`
    FinishedAtMS int64             `json:"finished_at_ms"`
    DurationMS   int64             `json:"duration_ms"`
}
```

### event / status 建议值

建议统一：

- `event=completed` / `status=completed`
- `event=failed` / `status=failed`
- `event=canceled` / `status=canceled`

不要再发一个单独的 `finished` 事件包裹状态，否则调用方还要二次判断。

---

## 二、SubagentTask 需要怎么扩展

当前 `SubagentTask` 只有：

- ID
- Task
- Label
- AgentID
- ModelName
- OriginChannel
- OriginChatID
- Status
- Result
- Created

见：`pkg/tools/subagent.go:41-52`

建议扩展为：

```go
type SubagentTask struct {
    ID            string
    Task          string
    Label         string
    AgentID       string
    ModelName     string
    OriginChannel string
    OriginChatID  string
    Status        string
    Result        string
    Error         string
    Created       int64
    Finished      int64

    Source        string            // tool | api
    Metadata      map[string]string // 业务透传信息
    Webhook       *SubagentWebhook  // webhook 配置
}
```

再新增：

```go
type SubagentWebhook struct {
    URL        string
    Secret     string
    Headers    map[string]string
    Events     map[string]bool
    TimeoutMS  int
    MaxRetries int
}
```

### 为什么要把 webhook 存在 task 上

因为 webhook 是“这次任务实例”的属性，不是 manager 的全局属性。

同一个 `SubagentManager` 可以同时跑：

- 普通 agent 内部 spawn（无 webhook）
- API spawn A（回调到 webhook A）
- API spawn B（回调到 webhook B）

所以必须放在 task 级别，而不是 manager 级别。

---

## 三、不要直接让 HTTP 层调用 SpawnTool 的原因

你原话是“使用 post 接口让 SpawnTool 创建 spawn subagent 异步执行”。

从业务语义上这是对的，但从代码实现上，**不建议真的在 HTTP handler 里 new 一个参数 map 然后调 `SpawnTool.Execute(...)`**。

原因有 4 个：

### 1. SpawnTool 依赖 tool context

`SpawnTool` 会从 `context` 读：

- `ToolChannel(ctx)`
- `ToolChatID(ctx)`

见：`pkg/tools/spawn.go:98-108`

HTTP 请求天然没有这些 agent tool 上下文，硬塞进去会让 API 层和 tool registry 发生不必要耦合。

### 2. SpawnTool 的 allowlist 语义是“父 agent -> 子 agent”

`SpawnTool` 的 allowlist 检查依赖：

```go
spawnTool.SetAllowlistChecker(func(targetAgentID string) bool {
    return registry.CanSpawnSubagent(currentAgentID, targetAgentID)
})
```

这是 agent 内部 delegation 语义。

而 API 入口没有天然的 `parentAgentID`。如果硬调 `SpawnTool`，你反而要伪造一个 parent，这会让权限模型变乱。

### 3. HTTP API 需要自己的鉴权与审计模型

外部 POST 请求的权限控制应该是：

- API token
- IP allowlist
- HMAC
- 网关层鉴权

而不是复用 agent 内部 subagent allowlist。

### 4. 真正公共能力本来就在 manager

`SpawnTool` 只是 adapter。

所以推荐：

- **保留 SpawnTool 给 LLM / agent 内部使用**
- **给 HTTP 单独建一个 SpawnAPIService，直接调 manager**

---

## 四、建议新增一个 HTTP 专用的 Spawn 服务层

建议新增文件：

```text
pkg/subagentapi/service.go
```

或者更简单一些，直接：

```text
pkg/tools/spawn_api.go
```

但从职责分离看，我更推荐单独包：

```text
pkg/subagentapi/
```

### 建议结构

```go
type Service struct {
    registry *agent.AgentRegistry
}
```

不过为了避免 `pkg/subagentapi -> pkg/agent -> pkg/tools` 的依赖过重，更稳的是抽接口：

```go
type SpawnManagerProvider interface {
    GetSubagentManager(agentID string) (*tools.SubagentManager, bool)
    GetDefaultSubagentManager() (*tools.SubagentManager, string, bool)
}
```

然后由 `pkg/agent` 或 gateway 层提供 adapter。

### 更实际的最小方案

如果你追求最小改动，不必先抽新接口，可以直接在 `AgentInstance` 上挂 manager 引用，或者在 `AgentLoop` 保存一个：

```go
type AgentLoop struct {
    ...
    subagentManagers map[string]*tools.SubagentManager
}
```

在 `registerSharedTools()` 创建 manager 时顺手保存：

- key = 当前 agentID
- value = 当前 agent 对应的 subagentManager

然后 HTTP handler 按 agentID 取对应 manager 去 `Spawn(...)`。

这比在 handler 里反查 tool registry 再断言出 `SpawnTool` 更干净。

---

## 五、HTTP 入口如何挂载

当前 gateway 已经支持共享 HTTP server，最合适的挂载点是：

- `cmd/picoclaw/internal/gateway/helpers.go`
- `pkg/channels/manager.go:361-365`

### 推荐做法

在 gateway 初始化共享 HTTP server 后注册：

```go
services.ChannelManager.HandleHTTPFunc("POST /api/spawn", handler)
```

如果当前环境的 `ServeMux` 不支持 method pattern，也可以保守用：

```go
services.ChannelManager.HandleHTTPFunc("/api/spawn", handler)
```

再在 handler 内判断：

```go
if r.Method != http.MethodPost {
    http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
    return
}
```

### 为什么建议挂在 gateway 的 shared HTTP server

因为：

- 当前真正长期运行 subagent 的就是 gateway 进程
- 这里已经有 health / webhook / debug 路由
- 不需要额外新增端口
- 生命周期和 work queue、channel manager 一致

---

## 六、handler 内部执行流程

建议新增 handler：

```text
cmd/picoclaw/internal/gateway/spawn_api.go
```

或者如果你更偏 Web Console API 风格，也可以挂到：

```text
web/backend/api/spawn.go
```

但注意这两个 HTTP 面向对象不一样：

- `web/backend/api/*` 是 launcher / web console 后端
- `cmd/picoclaw/internal/gateway/*` 是真正运行 agent 的 gateway 进程

**如果你的目标是“运行中的 PicoClaw 网关提供 POST spawn 能力”**，应当挂在 **gateway shared HTTP server**，不是 web launcher backend。

所以更推荐：

```text
cmd/picoclaw/internal/gateway/spawn_api.go
```

### handler 伪代码

```go
func registerSpawnAPIRoute(services *gatewayServices, agentLoop *agent.AgentLoop) {
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

        taskID, msg, err := manager.SpawnWithRequest(r.Context(), BuildSpawnRequest(req, resolvedAgentID), nil)
        if err != nil {
            writeSpawnError(w, err)
            return
        }

        w.Header().Set("Content-Type", "application/json")
        w.WriteHeader(http.StatusAccepted)
        _ = json.NewEncoder(w).Encode(map[string]any{
            "task_id": taskID,
            "status": "accepted",
            "message": msg,
            "webhook_registered": req.Webhook != nil,
        })
    })
}
```

这里我建议把原本的 `Spawn(...)` 再包一层请求对象，而不是继续往参数列表硬塞字段。

---

## 七、建议把 Spawn 方法升级成请求对象风格

当前 `Spawn()` 签名：

```go
func (sm *SubagentManager) Spawn(
    ctx context.Context,
    task, label, agentID, modelName, originChannel, originChatID string,
    callback AsyncCallback,
) (string, error)
```

如果继续支持 webhook，需要再塞：

- metadata
- webhook url
- secret
- timeout
- retries
- source
- maybe request id

参数会迅速失控。

### 推荐改造

新增：

```go
type SpawnRequest struct {
    Task          string
    Label         string
    AgentID       string
    ModelName     string
    OriginChannel string
    OriginChatID  string
    Source        string
    Metadata      map[string]string
    Webhook       *SubagentWebhook
}
```

然后改成：

```go
func (sm *SubagentManager) SpawnWithRequest(
    ctx context.Context,
    req SpawnRequest,
    callback AsyncCallback,
) (taskID string, message string, err error)
```

同时保留原 `Spawn(...)` 作为兼容包装：

```go
func (sm *SubagentManager) Spawn(...) (string, error) {
    _, msg, err := sm.SpawnWithRequest(ctx, SpawnRequest{...}, callback)
    return msg, err
}
```

这样：

- `SpawnTool` 不需要大改
- HTTP API 可以走新接口
- 数据模型更清晰

---

## 八、webhook 投递应该怎么做

## 1. 投递时机

最推荐的投递点就是：

- `pkg/tools/subagent.go:347-388` 的任务完成分支之后

也就是在 task 状态、result/error 都已经写好之后，统一调用：

```go
sm.dispatchWebhook(task)
```

### 为什么不放在 callback 里投递

因为 callback 当前语义是“通知父 agent / bus 系统”。

如果 webhook 也挂在 callback 上，会导致：

- HTTP API 也必须伪造 callback
- agent 内部 spawn 和 API spawn 的行为混杂
- webhook 逻辑耦合到 agent loop

所以更干净的方式是：

- callback：内部系统语义
- webhook：外部集成语义

二者并行，不互相依赖。

---

## 2. webhook sender 建议独立成模块

建议新增：

```text
pkg/webhook/sender.go
```

### 建议接口

```go
type Sender struct {
    client *http.Client
}

func NewSender() *Sender
func (s *Sender) Send(ctx context.Context, spec SubagentWebhook, payload SpawnWebhookPayload) error
```

### 具体行为建议

#### 请求方法

固定：

```text
POST
```

#### Content-Type

```text
application/json
```

#### Header 建议

固定附带：

- `Content-Type: application/json`
- `User-Agent: picoclaw-webhook/1.0`
- `X-PicoClaw-Event: completed|failed|canceled`
- `X-PicoClaw-Task-ID: subagent-12`
- `X-PicoClaw-Signature: sha256=...`（如果有 secret）
- 调用方自定义 headers

#### 签名建议

推荐对 **原始 JSON body** 做 HMAC-SHA256：

```text
X-PicoClaw-Signature: sha256=<hex>
```

这样接收方容易校验，也不会受字段顺序之外的问题干扰。

#### 超时建议

每个 webhook 请求使用独立 timeout：

- 默认 5s
- 可通过 `timeout_ms` 覆盖

#### 成功判定建议

建议：

- `2xx` 都算成功
- 非 `2xx` 视为失败

#### 是否重试

建议第一版支持**有限重试**，但不做复杂退避框架。

最小策略：

- `max_retries=0`：不重试
- `max_retries>0`：指数退避，比如 1s / 2s / 4s
- 对网络错误、5xx 重试
- 对 4xx 默认不重试

---

## 九、任务完成态与 webhook payload 如何映射

建议这样映射：

### 1. 完成

当：

```go
task.Status = "completed"
```

发送：

```json
{
  "version": 1,
  "event": "completed",
  "task_id": "subagent-12",
  "status": "completed",
  "result": "...",
  "error": ""
}
```

### 2. 失败

当：

```go
task.Status = "failed"
```

发送：

```json
{
  "version": 1,
  "event": "failed",
  "task_id": "subagent-12",
  "status": "failed",
  "result": "",
  "error": "Error: ..."
}
```

### 3. 取消

当：

```go
task.Status = "canceled"
```

发送：

```json
{
  "version": 1,
  "event": "canceled",
  "task_id": "subagent-12",
  "status": "canceled",
  "error": "Task canceled during execution"
}
```

### 4. 入队失败是否发 webhook

**不建议**。

因为入队失败时其实任务都没真正创建成功，HTTP 接口直接返回错误就够了：

- `429`：队列满
- `500`：enqueue 失败

如果这里也发 webhook，会让“accepted 之前就失败”的语义变复杂。

建议规则：

- **只有返回了 `202 Accepted` 并成功创建了 task_id 的任务，才会进入 webhook 生命周期**

---

## 十、权限与安全设计

这是这类接口里最容易被忽略，但最该先定清楚的部分。

## 1. 不能直接裸开放 POST /api/spawn

因为这相当于一个远程触发 LLM + tools 的执行入口，风险很高。

### 至少要支持一种鉴权机制

建议第一版最小支持：

- `Authorization: Bearer <token>`

配置里新增：

```go
type SpawnAPIConfig struct {
    Enabled bool   `json:"enabled"`
    Token   string `json:"token,omitempty"`
}
```

挂到例如：

```go
type GatewayConfig struct {
    ...
    SpawnAPI SpawnAPIConfig `json:"spawn_api"`
}
```

### handler 校验

- 未启用：404 或 403
- token 不匹配：401

## 2. webhook URL 必须做 SSRF 防护

这是重点。

因为用户 POST 的 webhook URL 本质上是服务端要去请求的地址，如果不做限制，会有 SSRF 风险。

### 至少建议做这些限制

#### A. 只允许 http/https

拒绝：

- file://
- ftp://
- gopher://
- unix://

#### B. 默认禁止内网 / loopback / link-local

建议默认拒绝解析到以下地址的 webhook：

- `127.0.0.0/8`
- `10.0.0.0/8`
- `172.16.0.0/12`
- `192.168.0.0/16`
- `169.254.0.0/16`
- `::1`
- `fc00::/7`
- `fe80::/10`

除非配置显式允许。

#### C. 可选 allowlist

可以新增配置：

```go
type WebhookOutboundConfig struct {
    AllowPrivateIPs bool     `json:"allow_private_ips"`
    AllowedHosts    []string `json:"allowed_hosts,omitempty"`
}
```

第一版最小可先做：

- 默认禁止私网
- 不做 host allowlist 也行

## 3. metadata 大小限制

避免 webhook payload 过大，建议：

- metadata key/value 数量限制
- task 长度限制
- result 长度限制

### result 是否全量发送

建议默认发全量，但提供上限，例如：

- 64KB 以内原样发送
- 超过则截断，并增加：
  - `result_truncated=true`
  - `result_size`

否则 webhook 可能因为 subagent 输出过长而变得不稳定。

---

## 十一、和现有 async callback 机制的关系

当前 agent 内部 spawn 已经有一套 async callback 机制：

- `pkg/agent/loop.go:1381-1407`
- 回调为 system inbound message
- 再由 `processSystemMessage()` 恢复父 agent

这个机制**不要替换**，而是并存。

### 关系建议

#### agent 内部 spawn

保留：

- callback 回父 agent
- 可选 webhook（如果未来 tool 参数也要支持 webhook）

#### HTTP API spawn

通常：

- callback = nil
- webhook = 必填或强烈建议传

也就是说：

- **callback 面向系统内部 agent 续跑**
- **webhook 面向系统外部集成**

两条链并不冲突。

---

## 十二、建议的最小文件改动清单

## 1. 修改 `pkg/tools/subagent.go`

### 需要做的事

1. 扩展 `SubagentTask`
2. 新增 `SubagentWebhook` / `SpawnRequest`
3. 新增 `SpawnWithRequest(...)`
4. 原 `Spawn(...)` 变成兼容包装
5. 在 `runTask()` 收口处增加 webhook 分发
6. 增加 webhook payload 构建辅助函数

### 关键改动点

- `pkg/tools/subagent.go:41-52`
- `pkg/tools/subagent.go:130-180`
- `pkg/tools/subagent.go:285-389`

---

## 2. 新增 `pkg/webhook/sender.go`

### 负责

- URL 校验
- header 构造
- HMAC 签名
- 超时控制
- 重试
- 发送日志

---

## 3. 修改 `pkg/agent/loop.go`

### 需要做的事

让 `AgentLoop` 能暴露或保存每个 agent 对应的 `SubagentManager`。

例如增加：

```go
subagentManagers map[string]*tools.SubagentManager
```

在 `registerSharedTools()` 创建 manager 时登记进去。

### 原因

HTTP handler 需要找到某个 agent 对应的 manager。

如果不做这个登记，就只能：

- 去 tool registry 里拿 `spawn` tool
- 再向下转回 manager

这会更绕。

---

## 4. 新增 `cmd/picoclaw/internal/gateway/spawn_api.go`

### 负责

- 注册 `/api/spawn`
- 鉴权
- 解析 JSON
- 校验请求
- 查找 manager
- 调 `SpawnWithRequest(...)`
- 返回 `202 Accepted`

---

## 5. 修改 `cmd/picoclaw/internal/gateway/helpers.go`

### 需要做的事

在共享 HTTP server 初始化后，新增：

```go
registerSpawnAPIRoute(services, agentLoop)
```

位置就在：

- `cmd/picoclaw/internal/gateway/helpers.go:250-255`

和已有的：

```go
registerWorkerQueueDebugRoute(services)
```

并列即可。

---

## 6. 修改 `pkg/config/config.go` / `pkg/config/defaults.go`

### 新增配置建议

```go
type SpawnAPIConfig struct {
    Enabled bool   `json:"enabled"`
    Token   string `json:"token,omitempty"`
}

type OutboundWebhookConfig struct {
    AllowPrivateIPs bool `json:"allow_private_ips"`
    DefaultTimeoutMS int `json:"default_timeout_ms"`
    DefaultMaxRetries int `json:"default_max_retries"`
    MaxPayloadBytes int `json:"max_payload_bytes"`
}
```

可挂在：

```go
type GatewayConfig struct {
    ...
    SpawnAPI        SpawnAPIConfig        `json:"spawn_api"`
    OutboundWebhook OutboundWebhookConfig `json:"outbound_webhook"`
}
```

### 默认值建议

```json
{
  "gateway": {
    "spawn_api": {
      "enabled": false,
      "token": ""
    },
    "outbound_webhook": {
      "allow_private_ips": false,
      "default_timeout_ms": 5000,
      "default_max_retries": 3,
      "max_payload_bytes": 65536
    }
  }
}
```

默认关闭入口，比默认开启安全得多。

---

## 十三、建议的数据流

## 1. 正常路径

```text
外部系统 POST /api/spawn
    ↓
Gateway handler 鉴权、校验请求
    ↓
定位目标 agent 对应的 SubagentManager
    ↓
SubagentManager.SpawnWithRequest(...)
    ↓
创建 SubagentTask（带 webhook 配置）
    ↓
进入 work queue / goroutine 异步执行
    ↓
runTask() 完成
    ↓
写入 task.Status / Result / Error / Finished
    ↓
若配置了 webhook，则发送 webhook
    ↓
返回 completed/failed/canceled 给外部系统
```

## 2. 和 agent 内部 spawn 共存

```text
Agent tool call spawn
    ↓
SpawnTool.execute(...)
    ↓
SubagentManager.Spawn(...)
    ↓
runTask()
    ├── callback → 回父 agent
    └── webhook  → 若这次任务配置了 webhook，则回外部系统
```

---

## 十四、关于是否让 SpawnTool 参数本身也支持 webhook

这是一个可选项。

### 第一版建议：不要改 SpawnTool 参数

也就是：

- HTTP API spawn 支持 webhook
- agent 内部 `spawn` tool 暂时不支持 webhook 参数

原因：

1. 你的当前需求重点是“POST 接口 + webhook”
2. 先把 HTTP 集成链路打通更稳
3. 不会扩大 LLM tool schema 和 prompt 复杂度

### 第二版再考虑

如果后面你希望 LLM 自己在调用 `spawn` 时也能指定 webhook，再给 `SpawnTool.Parameters()` 新增：

- `webhook_url`
- `webhook_secret`
- `webhook_headers`
- `webhook_events`

但第一版不建议一起上。

---

## 十五、错误处理策略

## 1. HTTP 入参错误

直接返回：

- `400`

例如：

- task 为空
- model_name 为空
- webhook.url 非法
- events 包含未知值

## 2. 目标 agent 不存在

返回：

- `400` 或 `404`

建议 `400`，因为是请求参数错误。

## 3. 队列满 / 入队失败

返回：

- `429 Too Many Requests`

如果能识别 `workqueue.ErrQueueFull`，就映射到 429。

## 4. webhook 发送失败

### 不影响主任务状态

这点非常重要。

subagent 已完成，不应该因为 webhook 发不出去而把任务本身标记成失败。

建议：

- task.Status 仍然保持真实执行状态：`completed/failed/canceled`
- 另外记录 webhook 投递状态到日志，必要时可扩展 task 字段：
  - `WebhookStatus`
  - `WebhookAttempts`
  - `WebhookLastError`

第一版至少打日志。

---

## 十六、测试计划

## 1. `pkg/tools/subagent.go` 单元测试

### 新增测试点

#### `TestSpawnWithRequestStoresWebhook`

验证：

- `SpawnWithRequest()` 创建的 task 中正确保存 webhook 配置、metadata、source

#### `TestRunTaskDispatchesCompletedWebhook`

验证：

- 完成时发送 `completed` webhook

#### `TestRunTaskDispatchesFailedWebhook`

验证：

- 失败时发送 `failed` webhook

#### `TestRunTaskDispatchesCanceledWebhook`

验证：

- 取消时发送 `canceled` webhook

#### `TestRunTaskWebhookFailureDoesNotChangeTaskStatus`

验证：

- webhook 发送失败不会污染 task.Status

---

## 2. `pkg/webhook/sender.go` 单元测试

### 新增测试点

#### `TestSenderSignsPayload`

验证签名 header 正确。

#### `TestSenderRejectsPrivateIPWhenDisabled`

验证 SSRF 防护。

#### `TestSenderRetriesOn5xx`

验证重试逻辑。

#### `TestSenderDoesNotRetryOn4xx`

验证 4xx 不重试。

---

## 3. gateway API 集成测试

### 新增测试点

#### `TestSpawnAPI_AcceptsRequest`

验证：

- POST `/api/spawn` 返回 202
- 返回 task_id

#### `TestSpawnAPI_RejectsUnauthorized`

验证鉴权失败。

#### `TestSpawnAPI_RejectsInvalidWebhookURL`

验证 URL 校验。

#### `TestSpawnAPI_QueueFullReturns429`

验证队列满映射正确。

---

## 十七、推荐实施顺序

### 第 1 步

先改 `pkg/tools/subagent.go`：

- 增加 `SpawnRequest`
- 增加 task webhook 字段
- 增加 `SpawnWithRequest()`

### 第 2 步

新增 `pkg/webhook/sender.go`：

- 先完成最小可用投递
- 默认超时
- 默认无重试或少量重试

### 第 3 步

在 `runTask()` 收口处接入 webhook 分发。

### 第 4 步

让 `AgentLoop` 暴露每个 agent 的 subagent manager。

### 第 5 步

新增 gateway 的 `/api/spawn` 路由。

### 第 6 步

补鉴权、SSRF 防护、测试。

---

## 十八、结论

针对你的需求，最合适的改造思路是：

### 入口层

- 新增 `POST /api/spawn`
- **不要直接让 HTTP handler 调 `SpawnTool.Execute()`**
- 改为让 HTTP handler 直接调用 `SubagentManager` 的新请求对象接口

### 执行层

- 保持现有 `SubagentManager.Spawn()` / `runTask()` 异步执行模型不变
- HTTP spawn 与 agent 内部 spawn 共用同一个 manager

### 回调层

- webhook 能力下沉到 `SubagentTask` / `SubagentManager.runTask()`
- 在 `completed/failed/canceled` 三种结束态统一投递
- webhook 投递失败不改变任务本身状态

### 安全层

- `POST /api/spawn` 默认关闭，必须鉴权
- webhook URL 做 SSRF 防护
- 支持 HMAC 签名

### 最关键的一句话

> 这次改造的核心，不是“把 SpawnTool 暴露成 HTTP 接口”，而是“把 SubagentManager 提升为统一异步任务执行内核，让 tool 调用和 HTTP 调用都共享它，并把 webhook 收敛到任务生命周期里统一处理”。

如果你下一步要我直接按这个方案落代码，我建议先做这几个文件：

- `pkg/tools/subagent.go`
- `pkg/agent/loop.go`
- `cmd/picoclaw/internal/gateway/helpers.go`
- `cmd/picoclaw/internal/gateway/spawn_api.go`
- `pkg/webhook/sender.go`
- `pkg/config/config.go`
- `pkg/config/defaults.go`
