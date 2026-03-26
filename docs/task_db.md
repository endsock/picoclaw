# spawn subagent 数据库记录方案

## 1. 先说结论：统一入口在哪里

目前如果目标是**给 spawn subagent 做统一数据库记录**，真正需要收口的不是 `cron`、`spawn_api`、主 agent 三处分别写，而是收口到 **`SubagentManager`**。

### 1.1 统一提交入口

所有真正创建 subagent 任务的路径，最终都会汇聚到：

- `pkg/tools/subagent.go:207` `func (sm *SubagentManager) SpawnWithRequest(...)`

这是最适合做“任务提交时 insert”的地方。

### 1.2 统一运行入口

所有 subagent 真正开始执行、结束执行的生命周期，最终都会汇聚到：

- `pkg/tools/subagent.go:457` `func (sm *SubagentManager) runTask(...)`

这是最适合做：

- 开始运行时更新状态
- 结束时更新状态、结果、错误、耗时

的地方。

---

## 2. 现在三条链路分别怎么走

## 2.1 spawn_api 链路

入口：

- `cmd/picoclaw/internal/gateway/spawn_api.go:74` `newSpawnCreateHandler(...)`
- `cmd/picoclaw/internal/gateway/spawn_api.go:113` 调用 `manager.SpawnWithRequest(...)`

调用链：

```text
POST /api/spawn
  -> newSpawnCreateHandler
  -> resolveSubagentManager
  -> SubagentManager.SpawnWithRequest
  -> SubagentManager.runTask
```

这条链路是**直接进入统一入口**的。

---

## 2.2 主 agent 通过 spawn tool 创建 subagent 的链路

spawn tool 注册位置：

- `pkg/agent/loop.go:244`
- `pkg/agent/loop.go:261` `tools.NewSpawnTool(subagentManager)`

主 agent 实际调用位置：

- `pkg/tools/spawn.go:77` `func (t *SpawnTool) execute(...)`
- `pkg/tools/spawn.go:115` 调用 `t.manager.Spawn(...)`
- `pkg/tools/subagent.go:190` `func (sm *SubagentManager) Spawn(...)`
- `pkg/tools/subagent.go:195` 内部再调用 `sm.SpawnWithRequest(...)`

调用链：

```text
主 agent LLM 触发 spawn tool
  -> SpawnTool.execute
  -> SubagentManager.Spawn
  -> SubagentManager.SpawnWithRequest
  -> SubagentManager.runTask
```

这条链路也是**最终统一汇聚到 `SpawnWithRequest/runTask`**。

---

## 2.3 cron 链路

cron 触发入口：

- `cmd/picoclaw/internal/gateway/helpers.go:724` `cronService.SetOnJob(...)`
- `pkg/tools/cron.go:278` `func (t *CronTool) ExecuteJob(...)`

当 cron 以 `deliver=false` 方式交给主 agent 处理时，会走：

- `pkg/tools/cron.go:333` `t.executor.ProcessDirectWithChannel(...)`
- `pkg/agent/loop.go:756` `func (al *AgentLoop) ProcessDirectWithChannel(...)`
- `pkg/agent/loop.go:797` `processMessage(...)`

调用链：

```text
cron job 触发
  -> CronTool.ExecuteJob
  -> AgentLoop.ProcessDirectWithChannel
  -> 主 agent 处理消息
  -> 如果主 agent 决定调用 spawn tool
      -> SpawnTool.execute
      -> SubagentManager.Spawn
      -> SubagentManager.SpawnWithRequest
      -> SubagentManager.runTask
```

### 关键点

**cron 本身并不会直接创建 subagent。**

cron 只是把一条消息重新喂给主 agent；只有当主 agent 的 LLM 在这次处理里选择调用 `spawn` 工具时，才会真正创建 subagent。

所以，如果你的目标是：

- 只记录 **spawn subagent 任务**

那么**不需要在 cron 单独加写库逻辑**；只要在 `SubagentManager` 统一打点即可，cron 触发出来的 spawn 也会自动被记录。

---

## 3. 设计目标

目标是记录 subagent 任务完整生命周期：

1. **任务提交时 insert**
2. **任务开始运行时 update status=running**
3. **任务结束时 update status=completed/failed/canceled**
4. 保存完整任务信息，包括：
   - task_id
   - source（api/tool）
   - agent_id
   - model_name
   - label
   - task 原文
   - origin_channel / origin_chat_id
   - metadata
   - webhook
   - 最终结果 `loopResult.Content`
   - 回传给上游 agent 的 `ToolResult.ForLLM`
   - 回传给用户/渠道的 `ToolResult.ForUser`
   - error
   - started_at / finished_at / duration_ms

---

## 4. 为什么建议只改 SubagentManager

原因很简单：

### 4.1 改动最小

如果分别在：

- `spawn_api.go`
- `spawn.go`
- `cron.go`

里各自插写库逻辑，会出现：

- 逻辑重复
- 状态不一致
- 后续加新入口时容易漏

而 `SpawnWithRequest/runTask` 已经是统一内核。

### 4.2 生命周期最完整

`SpawnWithRequest` 已掌握：

- 请求参数
- source
- metadata
- webhook
- origin channel/chat_id
- task_id

`runTask` 已掌握：

- 真正开始执行时机
- 最终结果
- 最终错误
- 结束时间
- callbackResult（包含上游 LLM 回传用文本）

### 4.3 对外行为最稳定

把 DB 写入作为 **SubagentManager 的可选依赖** 注入，不改变现有入口语义，最容易保证兼容性。

---

## 5. 推荐实现结构

建议新增一个独立包，例如：

- `pkg/taskdb`

职责：

- 初始化 MySQL / GORM
- 定义表模型
- 提供 `CreateSubmitted / MarkRunning / FinishTask` 三类操作

建议不要在 `pkg/tools/subagent.go` 里直接写 GORM 细节，而是依赖一个很小的接口。

---

## 6. 推荐接口设计

建议在 `pkg/tools/subagent.go` 侧只依赖接口，不直接依赖 GORM：

```go
type SubagentTaskRecorder interface {
    CreateSubmitted(ctx context.Context, task *SubagentTaskRecord) error
    MarkRunning(ctx context.Context, taskID string, startedAtMS int64) error
    FinishTask(ctx context.Context, taskID string, patch *SubagentTaskFinishPatch) error
}
```

建议配套结构：

```go
type SubagentTaskRecord struct {
    TaskID           string
    Source           string
    AgentID          string
    ModelName        string
    Label            string
    Task             string
    OriginChannel    string
    OriginChatID     string
    MetadataJSON     []byte
    WebhookJSON      []byte
    Status           string
    SubmittedAtMS    int64
}

type SubagentTaskFinishPatch struct {
    Status           string
    Result           string
    Error            string
    CallbackForLLM   string
    CallbackForUser  string
    Iterations       int
    StartedAtMS      *int64
    FinishedAtMS     int64
    DurationMS       int64
}
```

这样 `tools` 包不关心 DB 细节，只关心“记一条记录”。

---

## 7. 表结构设计（推荐 v1：单表）

v1 不建议一开始拆事件表，**一个主表就够用**。

表名建议：

- `subagent_tasks`

### 7.1 建表 SQL

```sql
CREATE TABLE `subagent_tasks` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `task_id` VARCHAR(64) NOT NULL COMMENT '内存任务ID，例如 subagent-1',
  `source` VARCHAR(32) NOT NULL COMMENT 'api / tool',
  `agent_id` VARCHAR(64) DEFAULT NULL,
  `model_name` VARCHAR(128) NOT NULL,
  `label` VARCHAR(255) DEFAULT NULL,
  `task_text` LONGTEXT NOT NULL COMMENT 'subagent 接收到的任务原文',

  `origin_channel` VARCHAR(64) DEFAULT NULL,
  `origin_chat_id` VARCHAR(191) DEFAULT NULL,

  `status` VARCHAR(32) NOT NULL COMMENT 'submitted/running/completed/failed/canceled/submit_failed',
  `result_text` LONGTEXT DEFAULT NULL COMMENT 'subagent 最终原始结果 loopResult.Content',
  `error_text` LONGTEXT DEFAULT NULL,

  `callback_for_llm` LONGTEXT DEFAULT NULL COMMENT '回传给上游 agent 的 ForLLM',
  `callback_for_user` LONGTEXT DEFAULT NULL COMMENT '回传给用户/渠道的 ForUser',
  `iterations` INT UNSIGNED DEFAULT NULL,

  `metadata_json` JSON DEFAULT NULL,
  `webhook_json` JSON DEFAULT NULL,

  `submitted_at_ms` BIGINT NOT NULL,
  `started_at_ms` BIGINT DEFAULT NULL,
  `finished_at_ms` BIGINT DEFAULT NULL,
  `duration_ms` BIGINT DEFAULT NULL,

  `created_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  `updated_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),

  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_subagent_tasks_task_id` (`task_id`),
  KEY `idx_subagent_tasks_status` (`status`),
  KEY `idx_subagent_tasks_source` (`source`),
  KEY `idx_subagent_tasks_agent_id` (`agent_id`),
  KEY `idx_subagent_tasks_origin` (`origin_channel`, `origin_chat_id`),
  KEY `idx_subagent_tasks_submitted_at` (`submitted_at_ms`),
  KEY `idx_subagent_tasks_finished_at` (`finished_at_ms`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
```

---

## 8. 字段说明

| 字段 | 含义 |
|---|---|
| `task_id` | 当前系统里的 subagent 任务 ID |
| `source` | 来源，当前主要是 `api` / `tool` |
| `agent_id` | 指定运行的 agent_id |
| `model_name` | 运行 subagent 使用的模型 |
| `label` | 可选短标签 |
| `task_text` | subagent 的任务原文 |
| `origin_channel` / `origin_chat_id` | 来源会话，用于异步回传 |
| `status` | DB 状态 |
| `result_text` | subagent 的最终回答原文 |
| `error_text` | 失败/取消错误 |
| `callback_for_llm` | 回给父 agent 的文本，便于排查上游上下文 |
| `callback_for_user` | 回给渠道/用户的文本 |
| `iterations` | subagent 内部 tool loop 迭代次数 |
| `metadata_json` | 请求 metadata 快照 |
| `webhook_json` | webhook 配置快照 |
| `submitted_at_ms` | 提交时刻 |
| `started_at_ms` | 真正开始运行时刻 |
| `finished_at_ms` | 结束时刻 |
| `duration_ms` | 执行耗时 |

---

## 9. GORM 模型设计

建议模型如下：

```go
package taskdb

import "time"

type SubagentTaskModel struct {
    ID              uint64     `gorm:"primaryKey;autoIncrement"`
    TaskID           string     `gorm:"size:64;not null;uniqueIndex:uk_task_id"`
    Source           string     `gorm:"size:32;not null;index"`
    AgentID          *string    `gorm:"size:64;index"`
    ModelName        string     `gorm:"size:128;not null"`
    Label            *string    `gorm:"size:255"`
    TaskText         string     `gorm:"type:longtext;not null"`

    OriginChannel    *string    `gorm:"size:64;index:idx_origin"`
    OriginChatID     *string    `gorm:"size:191;index:idx_origin"`

    Status           string     `gorm:"size:32;not null;index"`
    ResultText       *string    `gorm:"type:longtext"`
    ErrorText        *string    `gorm:"type:longtext"`

    CallbackForLLM   *string    `gorm:"type:longtext"`
    CallbackForUser  *string    `gorm:"type:longtext"`
    Iterations       *int

    MetadataJSON     []byte     `gorm:"type:json"`
    WebhookJSON      []byte     `gorm:"type:json"`

    SubmittedAtMS    int64      `gorm:"not null;index"`
    StartedAtMS      *int64
    FinishedAtMS     *int64     `gorm:"index"`
    DurationMS       *int64

    CreatedAt        time.Time
    UpdatedAt        time.Time
}
```

说明：

- JSON 字段直接用 `[]byte` 就够，不额外引入别的类型
- `origin_chat_id` 用 `191`，避免 utf8mb4 索引长度问题
- v1 不建议一开始搞太多关联表

---

## 10. 状态设计

建议数据库状态使用以下枚举：

- `submitted`
- `running`
- `completed`
- `failed`
- `canceled`
- `submit_failed`

### 为什么要有 `submit_failed`

当前 `SpawnWithRequest` 在工作队列提交失败时会直接返回错误：

- `pkg/tools/subagent.go:251`
- `pkg/tools/subagent.go:260`

如果你希望“任务提交时 insert”尽量完整，最稳妥方案是：

1. 先插入 `submitted`
2. 如果 `workQueue.Submit(...)` 失败，再更新成 `submit_failed`

这样不会丢失失败现场。

### 是否要修改内存态 `SubagentTask.Status`

**不建议 v1 修改对外 API 行为。**

当前内存态在 `SpawnWithRequest` 里直接设置的是：

- `pkg/tools/subagent.go:236` `Status: "running"`

为了最小改动，建议：

- **数据库状态** 用 `submitted -> running -> final`
- **内存状态** 先保持现状，避免影响现有 `/api/spawn/{task_id}` 和测试行为

后续如果你愿意统一语义，再把内存态也改成 `submitted`。

---

## 11. 生命周期落点设计

## 11.1 提交时 insert

位置：

- `pkg/tools/subagent.go:207` `SpawnWithRequest(...)`

建议落点：

1. 参数校验通过
2. 生成 `taskID`
3. 构造 `SubagentTask`
4. **调用 recorder.CreateSubmitted(...) 插入数据库**
5. 再尝试进入 work queue / goroutine
6. 若 enqueue 失败，更新为 `submit_failed`

建议伪代码：

```go
submittedAt := time.Now().UnixMilli()

record := buildDBRecordFromRequest(taskID, req, submittedAt)
if recorder != nil {
    _ = recorder.CreateSubmitted(ctx, record)
}

if err := workQueue.Submit(...); err != nil {
    if recorder != nil {
        _ = recorder.FinishTask(ctx, taskID, &SubagentTaskFinishPatch{
            Status:       "submit_failed",
            Error:        fmt.Sprintf("failed to enqueue subagent: %v", err),
            FinishedAtMS: time.Now().UnixMilli(),
        })
    }
    return ..., err
}
```

---

## 11.2 开始运行时 update running

位置：

- `pkg/tools/subagent.go:457` `runTask(...)`

建议落点：

- `runTask` 一进入、准备真正执行前
- 设置：
  - `status = running`
  - `started_at_ms = now`

建议伪代码：

```go
startedAt := time.Now().UnixMilli()
if recorder != nil {
    _ = recorder.MarkRunning(ctx, task.ID, startedAt)
}
```

---

## 11.3 结束时 update final status/result/error

位置：

- `pkg/tools/subagent.go:565` 之后已经有最终 `taskSnapshot`

这是最好的收口点，因为这时已经拿到了：

- `finalStatus`
- `finalResult`
- `finalError`
- `taskSnapshot`
- `callbackResult`

建议更新字段：

- `status`
- `result_text`
- `error_text`
- `callback_for_llm`
- `callback_for_user`
- `iterations`
- `finished_at_ms`
- `duration_ms`

### iterations 怎么拿

当前 `runTask` 里，`loopResult.Iterations` 只被拼进 `callbackResult.ForLLM`，没有单独保存到 task 上。

建议最小改动：

- 在 `runTask` 中增加局部变量 `iterations`
- `loopResult` 成功时记录 `iterations = loopResult.Iterations`
- finish update 时一并写库

---

## 12. 完整信息里“包含上游 LLM 返回结果”怎么理解

这里建议分成两类都保存：

### 12.1 subagent 自己的最终结果

也就是：

- `loopResult.Content`

保存到：

- `result_text`

### 12.2 回给上游父 agent 的包装结果

也就是当前代码里的：

- `callbackResult.ForLLM`

例如：

```text
Subagent 'xxx' completed (iterations: 3): ...
```

保存到：

- `callback_for_llm`

### 12.3 回给用户/渠道的结果

也就是：

- `callbackResult.ForUser`

保存到：

- `callback_for_user`

这样三类信息都在：

- 原始结果
- 上游 agent 看到的结果
- 用户/渠道收到的结果

这比只存一个 `result` 更利于排查问题。

---

## 13. 推荐新增包结构

建议新增：

```text
pkg/taskdb/
  db.go           // 初始化 gorm + mysql
  model.go        // GORM model
  store.go        // CreateSubmitted / MarkRunning / FinishTask
  noop.go         // 空实现，便于不开启 DB 时保持兼容
```

### 13.1 db.go

职责：

- 读取配置
- `mysql.Open(dsn)`
- `gorm.Open(...)`
- AutoMigrate

### 13.2 store.go

职责：

- 实现 `SubagentTaskRecorder`
- 对 `subagent_tasks` 表做增改

### 13.3 noop.go

职责：

- 未配置数据库时，提供空实现
- 保持现有逻辑零侵入

---

## 14. 配置建议

建议在配置中新增一段，例如：

```json
{
  "task_db": {
    "enabled": true,
    "dsn": "user:pass@tcp(127.0.0.1:3306)/picoclaw?charset=utf8mb4&parseTime=True&loc=Local"
  }
}
```

Go 依赖：

```go
require (
    gorm.io/gorm v1.31.1
    gorm.io/driver/mysql v1.6.0
)
```

建议额外设置：

- `SetMaxOpenConns`
- `SetMaxIdleConns`
- `SetConnMaxLifetime`

避免长连接问题。

---

## 15. 初始化与注入方案

推荐初始化位置：

- `cmd/picoclaw/internal/gateway/helpers.go`

原因：

- 这里已经负责组装 `AgentLoop / CronTool / SubagentManager`
- `SubagentManager` 也是在这里创建并注入到 agent 的

现有创建位置：

- `pkg/agent/loop.go:246` `tools.NewSubagentManager(...)`

建议改造方式：

### 方案 A（最小改动，推荐）

在 `tools.NewSubagentManager(...)` 创建后，通过 setter 注入：

```go
subagentManager.SetTaskRecorder(taskRecorder)
```

需要新增：

```go
func (sm *SubagentManager) SetTaskRecorder(r SubagentTaskRecorder)
```

并在 `SubagentManager` 结构体增加：

```go
taskRecorder SubagentTaskRecorder
```

### 方案 B

直接把 recorder 加入 `NewSubagentManager(...)` 构造参数。

不推荐 v1 这样做，因为会扩大构造函数变更面。

---

## 16. 具体改动点清单

如果后续按这个方案落地，建议改这些文件：

### 必改

1. `pkg/tools/subagent.go`
   - 增加 `taskRecorder` 字段
   - 增加 `SetTaskRecorder(...)`
   - 在 `SpawnWithRequest(...)` 做 insert
   - 在 `runTask(...)` 开始时标记 running
   - 在 `runTask(...)` 结束时写入最终结果

2. `cmd/picoclaw/internal/gateway/helpers.go`
   - 初始化 task DB
   - 创建 recorder
   - 注入到所有 `SubagentManager`

3. `pkg/config/config.go`
   - 增加 `task_db` 配置项

4. `go.mod`
   - 引入：
     - `gorm.io/gorm v1.31.1`
     - `gorm.io/driver/mysql v1.6.0`

5. 新增 `pkg/taskdb/*`

### 不建议改

1. `cmd/picoclaw/internal/gateway/spawn_api.go`
   - 不建议在这里直接写库

2. `pkg/tools/spawn.go`
   - 不建议在这里直接写库

3. `pkg/tools/cron.go`
   - 不建议在这里直接写库

原因：这些都只是入口适配层，不是统一内核。

---

## 17. 并发与幂等注意事项

### 17.1 幂等键

- 用 `task_id` 做唯一键

### 17.2 状态更新条件

建议更新时尽量带条件，避免乱序覆盖：

- `submitted -> running`
- `running -> completed/failed/canceled`
- `submitted -> submit_failed`

例如：

```sql
UPDATE subagent_tasks
SET status = 'running', started_at_ms = ?
WHERE task_id = ? AND status = 'submitted';
```

### 17.3 callback / webhook 顺序

当前 `runTask` 中：

- 先更新内存 task
- 再 callback
- 再 webhook

数据库建议：

- **先写最终状态到 DB**
- 再 callback
- 再 webhook

这样外部系统查询数据库时，拿到的是已经落盘的最终态。

---

## 18. 兼容性建议

为了不影响现有行为，建议：

1. DB 为**可选能力**
2. 没配 DSN 时走 noop recorder
3. DB 写失败只记日志，不阻断主流程

即：

- spawn 不因数据库故障而失败
- runTask 不因数据库故障而失败

这样可用性最好。

---

## 19. 最终推荐方案（落地版摘要）

### 统一入口

- 提交统一入口：`pkg/tools/subagent.go:207` `SpawnWithRequest`
- 运行统一入口：`pkg/tools/subagent.go:457` `runTask`

### 统一策略

- `spawn_api`：不单独写库
- 主 agent `spawn`：不单独写库
- `cron`：不单独写库
- **全部只在 `SubagentManager` 统一写库**

### 数据库记录时机

- `SpawnWithRequest`：insert `submitted`
- `runTask` 开始：update `running`
- `runTask` 结束：update `completed/failed/canceled` + result/error/callback/duration
- enqueue 失败：update `submit_failed`

### 推荐表

- `subagent_tasks`

### 推荐依赖

```go
require (
    gorm.io/gorm v1.31.1
    gorm.io/driver/mysql v1.6.0
)
```

---

## 20. 一句话总结

**你要做的“spawn subagent 数据库记录”，最佳统一收口点就是 `SubagentManager.SpawnWithRequest + SubagentManager.runTask`。**

这样可以一次覆盖：

- `spawn_api`
- 主 agent 的 `spawn` 工具
- cron 场景下由主 agent 间接触发的 spawn

同时改动最小，生命周期最完整，也最不容易漏。