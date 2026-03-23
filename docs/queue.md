# subagent / cron 全局队列改造计划

## 目标

在**不改变主 agent 当前并发模型**的前提下，只为以下两类后台任务增加统一的全局 worker queue：

1. `subagent` / `spawn` 触发的后台子任务
2. `cron` 到期触发的任务

这样做的目的：

- 避免短时间内大量 `spawn` 直接创建过多 goroutine 和 LLM/tool 执行
- 避免同一时刻大量 `cron` job 触发时挤爆系统资源
- 保持主 agent 的 tool call 并发行为不变，降低改动风险

---

## 当前现状

### 1. subagent 目前是直接后台 goroutine 执行

位置：`pkg/tools/subagent.go`

当前 `SubagentManager.Spawn()` 中：

- 创建任务记录后
- 直接 `go sm.runTask(ctx, subagentTask, callback)`

这意味着：

- 没有排队
- 没有最大并发限制
- 没有 backpressure

### 2. cron 目前是顺序扫描 + 直接执行

位置：`pkg/cron/service.go`

当前 `checkJobs()` 中：

- 每秒扫描到期任务
- 收集 `dueJobIDs`
- 然后直接循环调用 `cs.executeJobByID(jobID)`

这意味着：

- 没有统一队列
- 虽然不是无限并发，但大量任务会集中在检查循环里直接执行
- 后续如果 job 内部又进入 agent/subagent，会继续放大压力

### 3. 主 agent 当前不纳入本次方案

位置：`pkg/agent/loop.go`

主 agent 的 tool calls 当前仍保留现状，不进入新队列。原因：

- 改动面更大
- 对现有交互延迟影响更敏感
- 本轮目标是先收敛后台任务并发风险

---

## 方案概述

新增一个**进程级全局工作队列**，专门承接：

- subagent 执行任务
- cron 执行任务

主 agent 不使用该队列。

同时需要**暴露一个 HTTP 监控接口**，用于查看 worker queue 的详细状态信息，便于外部监控系统或人工排查直接读取当前队列状态。

### 设计原则

1. **最小改动**：尽量不改现有业务逻辑，只改“如何启动执行”
2. **统一入口**：subagent 和 cron 都通过同一个 queue 提交任务
3. **可配置**：worker 数量、队列长度走配置
4. **可降级**：队列满时返回明确错误，而不是静默丢失
5. **生命周期明确**：queue 在 gateway/agent 启动时创建，在退出时停止

---

## 拟新增模块

## 1. 新增 `pkg/workqueue/queue.go`

建议新增一个独立包：

- `pkg/workqueue/queue.go`

提供一个非常小的通用 worker queue。

### 建议结构

```go
package workqueue

type Job struct {
    Name string
    Run  func(context.Context)
}

type Queue struct {
    jobs chan Job
    wg   sync.WaitGroup
}
```

### 建议 API

```go
func New(size int) *Queue
func (q *Queue) Start(ctx context.Context, workers int)
func (q *Queue) Submit(ctx context.Context, job Job) error
func (q *Queue) Stop()
func (q *Queue) Snapshot() Snapshot
```

### 建议增加的状态结构

为了支持监控接口，queue 需要提供一个线程安全快照结构，例如：

```go
type Snapshot struct {
    Enabled        bool           `json:"enabled"`
    Workers        int            `json:"workers"`
    QueueSize      int            `json:"queue_size"`
    Queued         int            `json:"queued"`
    Active         int            `json:"active"`
    SubmittedTotal uint64         `json:"submitted_total"`
    StartedTotal   uint64         `json:"started_total"`
    FinishedTotal  uint64         `json:"finished_total"`
    FailedTotal    uint64         `json:"failed_total"`
    RejectedTotal  uint64         `json:"rejected_total"`
    RunningJobs    []RunningJob   `json:"running_jobs,omitempty"`
    PendingPreview []PendingJob   `json:"pending_preview,omitempty"`
}

type RunningJob struct {
    Name      string `json:"name"`
    StartedAt int64  `json:"started_at_ms"`
}

type PendingJob struct {
    Name       string `json:"name"`
    EnqueuedAt int64  `json:"enqueued_at_ms"`
}
```

### 说明

- `Queued`：当前排队中的任务数
- `Active`：当前正在执行的任务数
- `SubmittedTotal`：累计提交数
- `StartedTotal`：累计开始执行数
- `FinishedTotal`：累计执行完成数
- `FailedTotal`：任务执行函数内部 panic / 失败计数
- `RejectedTotal`：因队列满或关闭而拒绝入队的次数
- `RunningJobs`：当前运行中任务明细
- `PendingPreview`：等待队列的预览信息，建议只保留前 N 条，避免接口返回过大

### 行为要求

#### `Start(ctx, workers)`

- 启动固定数量 worker
- worker 循环从 `jobs` channel 取任务
- `ctx.Done()` 后退出

#### `Submit(ctx, job)`

建议使用**非阻塞提交**：

- 有空位则立即入队
- 队列满则返回 error，例如：`work queue is full`
- 调用方自行决定如何把错误回写到任务状态/日志

#### `Stop()`

- 关闭 queue
- 等待 worker 退出

---

## 配置改动计划

## 2. 在 `pkg/config/config.go` 增加 worker queue 配置

建议在顶层 `Config` 增加：

```go
type WorkerQueueConfig struct {
    Enabled   bool `json:"enabled"`
    Workers   int  `json:"workers"`
    QueueSize int  `json:"queue_size"`
}
```

并挂到：

```go
type Config struct {
    ...
    WorkerQueue WorkerQueueConfig `json:"worker_queue"`
}
```

### 默认值

在 `pkg/config/defaults.go` 中设置默认值：

```go
WorkerQueue: WorkerQueueConfig{
    Enabled:   true,
    Workers:   4,
    QueueSize: 128,
},
```

### 说明

- `Enabled=false` 时，可以退回旧行为，便于灰度与回滚
- `Workers` 控制 subagent + cron 的总后台并发
- `QueueSize` 控制高峰期缓存容量

---

## 注入与生命周期

## 3. 在 gateway 启动路径创建全局 queue

主要接入点：`cmd/picoclaw/internal/gateway/helpers.go`

这是当前最关键的运行时组装位置，因为：

- `agentLoop` 在这里创建
- `cronService` 在这里创建
- config reload 也在这里做服务重建

### 建议做法

在 gateway 服务结构体里新增：

```go
type gatewayServices struct {
    ...
    WorkQueue *workqueue.Queue
}
```

### 在 `setupAndStartServices()` 中：

1. 根据 `cfg.WorkerQueue` 创建 queue
2. 创建一个生命周期 context（或复用 gateway 主 ctx）
3. 启动 worker
4. 将 queue 注入 cron setup 与 agentLoop 使用到的 subagent manager
5. 将 queue 注册到 HTTP 监控接口

### 监控接口暴露位置

当前 gateway 已经有统一 HTTP server 与 health endpoint，位置：`cmd/picoclaw/internal/gateway/helpers.go`

建议复用现有健康检查服务的 HTTP 路由体系，新增一个只读接口：

- `GET /debug/worker-queue`

如果你希望接口更偏运维语义，也可以用：

- `GET /metrics/worker-queue`

但从当前项目已有 `/health`、`/ready` 风格看，建议优先使用：

- `GET /debug/worker-queue`

### 接口返回内容建议

返回 JSON，例如：

```json
{
  "enabled": true,
  "workers": 4,
  "queue_size": 128,
  "queued": 7,
  "active": 4,
  "submitted_total": 102,
  "started_total": 95,
  "finished_total": 91,
  "failed_total": 1,
  "rejected_total": 6,
  "running_jobs": [
    {
      "name": "subagent:subagent-12",
      "started_at_ms": 1710000000000
    }
  ],
  "pending_preview": [
    {
      "name": "cron:abc123",
      "enqueued_at_ms": 1710000001000
    }
  ]
}
```

### 接口实现建议

为了做到最小改动，建议不要单独再起一个 server，而是：

1. 由 `workqueue.Queue` 暴露 `Snapshot()`
2. 在 gateway 初始化 HTTP server 时，注册一个 handler
3. handler 内调用 `services.WorkQueue.Snapshot()`
4. 直接返回 `application/json`

### 安全与暴露范围

该接口会暴露运行中任务名称，因此建议：

1. 默认只绑定到当前 gateway 的监听地址
2. 如果 gateway 对公网开放，后续可以再加鉴权或仅在 debug 模式开放
3. 第一版先保持只读，不提供清队列/取消任务等写操作

### 在关闭/重载时：

- 在 `stopAndCleanupServices()` 中停止 queue
- 在 config reload 时，旧 queue 跟随旧服务一起停掉，再按新配置创建新 queue

---

## subagent 改动计划

## 4. `SubagentManager` 增加队列依赖

文件：`pkg/tools/subagent.go`

### 结构调整

为 `SubagentManager` 增加字段：

```go
workQueue *workqueue.Queue
```

### 构造函数调整

当前：

```go
func NewSubagentManager(
    provider providers.LLMProvider,
    defaultModel, workspace string,
    parentTools *ToolRegistry,
    maxIterations int,
) *SubagentManager
```

建议改为：

```go
func NewSubagentManager(
    provider providers.LLMProvider,
    defaultModel, workspace string,
    parentTools *ToolRegistry,
    maxIterations int,
    workQueue *workqueue.Queue,
) *SubagentManager
```

### `Spawn()` 的改动

当前逻辑：

```go
go sm.runTask(ctx, subagentTask, callback)
```

改为：

```go
err := sm.workQueue.Submit(ctx, workqueue.Job{
    Name: taskID,
    Run: func(runCtx context.Context) {
        sm.runTask(runCtx, subagentTask, callback)
    },
})
```

### 队列不可用/满时的处理

如果：

- `workQueue == nil`
- `Submit` 返回 error

建议：

1. 将 `task.Status` 更新为 `failed`
2. `task.Result` 写入 enqueue 失败原因
3. 返回明确错误给上层 tool

例如：

- `failed to enqueue subagent: work queue is full`

### 为什么不改 `runTask()`

`runTask()` 保持原样即可，因为本次目标只是把“启动执行”改成“入队后执行”。

---

## agent 注册 subagent 工具的改动计划

## 5. `registerSharedTools()` 需要把 queue 传给 `SubagentManager`

文件：`pkg/agent/loop.go`

当前 `registerSharedTools()` 里：

```go
subagentManager := tools.NewSubagentManager(provider, agent.Model, agent.Workspace, agent.Tools, cfg.Agents.Defaults.SubagentMaxIterations)
```

因为 `SubagentManager` 构造函数要新增 queue 参数，所以这里需要一起调整。

### 建议改法

给 `NewAgentLoop()` / `registerSharedTools()` 增加 queue 参数透传，例如：

```go
func NewAgentLoop(cfg *config.Config, msgBus *bus.MessageBus, provider providers.LLMProvider, workQueue *workqueue.Queue) *AgentLoop
```

然后：

```go
registerSharedTools(cfg, msgBus, registry, provider, workQueue)
```

再到：

```go
subagentManager := tools.NewSubagentManager(..., workQueue)
```

### 范围说明

虽然本次不让主 agent 的 tool call 进入队列，但 `subagent` 工具是由 agent 注册出来的，因此 **AgentLoop 的构造与 shared tools 注册仍然需要感知 queue**。

这只是“依赖注入”，不意味着主 agent 的执行模型发生变化。

---

## cron 改动计划

## 6. `CronService` 增加队列依赖

文件：`pkg/cron/service.go`

### 结构调整

为 `CronService` 增加字段：

```go
workQueue *workqueue.Queue
```

### 构造函数调整

当前：

```go
func NewCronService(storePath string, onJob JobHandler) *CronService
```

建议改为：

```go
func NewCronService(storePath string, onJob JobHandler, workQueue *workqueue.Queue) *CronService
```

### `checkJobs()` 的改动

当前：

```go
for _, jobID := range dueJobIDs {
    cs.executeJobByID(jobID)
}
```

改为：

```go
for _, jobID := range dueJobIDs {
    id := jobID
    err := cs.workQueue.Submit(context.Background(), workqueue.Job{
        Name: "cron:" + id,
        Run: func(runCtx context.Context) {
            cs.executeJobByID(id)
        },
    })
    if err != nil {
        cs.markJobEnqueueError(id, err)
    }
}
```

### 增加一个最小辅助方法

建议新增：

```go
func (cs *CronService) markJobEnqueueError(jobID string, err error)
```

职责：

- 找到 job
- 更新：
  - `LastRunAtMS`
  - `LastStatus = "error"`
  - `LastError = "failed to enqueue job: ..."`
- 重新计算下次执行时间（按现有 cron/every/at 语义）
- 保存 store

### 为什么这里要补这个方法

因为 cron 的语义和 subagent 不一样：

- subagent enqueue 失败时，失败信息直接返回给当前 agent/tool 调用即可
- cron enqueue 失败时，触发点已经脱离用户请求现场，必须把错误落到 job state 里，否则会变成静默失败

---

## gateway wiring 改动计划

## 7. 修改 `setupCronTool()` 以传入 queue

文件：`cmd/picoclaw/internal/gateway/helpers.go`

当前：

```go
cronService := cron.NewCronService(cronStorePath, nil)
```

改为：

```go
cronService := cron.NewCronService(cronStorePath, nil, services.WorkQueue)
```

或者如果 queue 不挂在 `services` 上，也可以作为参数显式传入：

```go
func setupCronTool(..., workQueue *workqueue.Queue) *cron.CronService
```

### 同时修改 AgentLoop 创建

当前：

```go
agentLoop := agent.NewAgentLoop(cfg, msgBus, provider)
```

建议改为：

```go
agentLoop := agent.NewAgentLoop(cfg, msgBus, provider, workQueue)
```

然后让其只用于 subagent manager 注入。

---

## CLI helper 兼容改动

## 8. 增加 worker queue 监控接口

文件：

- `cmd/picoclaw/internal/gateway/helpers.go`
- 以及当前 health server / HTTP 路由注册相关位置

### 目标

暴露一个 HTTP 接口，让外部可以实时查看 worker queue 的详细状态。

### 建议路由

```text
GET /debug/worker-queue
```

### 建议返回字段

至少包含：

- `enabled`
- `workers`
- `queue_size`
- `queued`
- `active`
- `submitted_total`
- `started_total`
- `finished_total`
- `failed_total`
- `rejected_total`
- `running_jobs`
- `pending_preview`

### 最小实现方式

- queue 内部维护运行时计数器和当前任务快照
- gateway handler 只负责读快照并编码为 JSON
- 如果 queue 未启用，则返回：

```json
{
  "enabled": false
}
```

### 为什么监控接口放在 gateway

因为：

- 只有 gateway 模式会真正长期运行 cron / subagent 后台任务
- gateway 已经统一托管 HTTP server
- 不需要额外新增端口或独立服务

---

## 9. 修正所有 `NewCronService(...)` 调用点

除了 gateway 外，还要改这些 helper：

- `cmd/picoclaw/internal/cron/helpers.go`

这些 CLI 命令只是：

- list
- remove
- enable/disable

它们**不启动执行循环**，只操作存储，因此可直接传 `nil` queue：

```go
cron.NewCronService(storePath, nil, nil)
```

这样兼容成本最低。

---

## 回滚/兼容策略

## 10. 建议保留 `Enabled` 开关

如果配置：

```json
"worker_queue": {
  "enabled": false
}
```

建议行为：

- `subagent` 退回原来的 `go sm.runTask(...)`
- `cron` 退回原来的直接 `cs.executeJobByID(...)`

这样便于：

- 灰度发布
- 调试性能问题
- 遇到兼容性问题快速回退

### 最小实现方式

在调用处判断：

- `workQueue != nil` → 入队
- `workQueue == nil` → 旧行为

这样甚至不需要在 queue 内部实现额外分支。

---

## 错误处理策略

## 11. 建议统一策略

### subagent enqueue 失败

返回 tool error，并更新任务状态：

- `Status = failed`
- `Result = failed to enqueue subagent: ...`

### cron enqueue 失败

写入 job state：

- `LastStatus = error`
- `LastError = failed to enqueue job: ...`

### 日志

建议加统一日志字段：

- `queue=worker_queue`
- `job_type=subagent|cron`
- `job_name`

方便高峰期排查。

---

## 本次明确不做的事情

以下内容**不在这次最小方案内**：

1. 主 agent tool calls 入队
2. queue 优先级调度
3. cron 与 subagent 拆分成两个独立队列
4. 动态扩缩容 worker
5. 持久化任务队列
6. job 重试机制

理由：

- 会明显扩大改动面
- 先解决“后台任务没有统一并发收口”的核心问题

---

## 建议实施顺序

### 第 1 步
新增 `pkg/workqueue/queue.go`

### 第 2 步
在 `config.go` / `defaults.go` 增加 `WorkerQueueConfig`

### 第 3 步
修改 `SubagentManager`：

- 增加 `workQueue`
- `Spawn()` 改为入队执行

### 第 4 步
修改 `CronService`：

- 增加 `workQueue`
- `checkJobs()` 改为入队执行
- 增加 `markJobEnqueueError()`

### 第 5 步
修改 `AgentLoop` / `registerSharedTools()`：

- 只是把 queue 透传给 `SubagentManager`
- 不改主 agent tool call 并发行为

### 第 6 步
修改 gateway wiring：

- 创建 queue
- 启动 queue
- 注入给 agentLoop / cronService
- 停止与 reload 时正确销毁和重建

### 第 7 步
修复 `cmd/picoclaw/internal/cron/helpers.go` 的构造函数调用

---

## 验收标准

### 功能验收

1. 连续触发多个 `spawn` 时：
   - 不再直接无限创建后台执行
   - 任务进入统一队列
   - 最多只有 `workers` 个 subagent/cron 任务同时运行

2. 同一时间大量 cron 到期时：
   - 不在 `checkJobs()` 内直接密集执行
   - 而是提交到统一队列

3. 主 agent 普通 tool call 行为保持不变

4. 队列满时：
   - subagent 返回明确失败
   - cron job state 能看到 enqueue 失败原因

5. HTTP 监控接口可访问：
   - `GET /debug/worker-queue`
   - 能返回当前 worker queue 的详细状态 JSON

### 回归验收

1. CLI agent 模式仍能正常运行
2. gateway 模式启动/停止正常
3. config reload 后 queue 能随新配置重建
4. cron list/remove/enable/disable 命令仍可用

---

## 后续可选增强

如果这版稳定，后面可以继续做：

1. 为 subagent / cron 分离成两个池，避免互相抢占
2. 增加队列状态接口（长度、活跃 worker 数）
3. 增加简单优先级：subagent > cron
4. 增加队列等待超时与丢弃策略
5. 把主 agent tool call 也纳入更细粒度的限流体系

---

## 结论

本次最小方案的核心是：

- **主 agent 不动**
- **subagent 与 cron 共享一个全局 worker queue**
- 通过依赖注入把 queue 接入 `SubagentManager` 和 `CronService`
- 通过配置控制 worker 数量与队列长度
- 通过 gateway 启动/重载逻辑管理 queue 生命周期

这是当前代码结构下最小、最稳、最容易落地的一版方案。
