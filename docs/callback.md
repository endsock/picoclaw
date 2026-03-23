# 异步 Subagent Callback 回传修改方案

## 概述

当前 `spawn` 异步 subagent 完成后，会通过 callback 将结果重新注入为一条 `system` 入站消息，再由 `processSystemMessage()` 继续处理。

现有实现能够保证异步结果不会丢失，但有一个关键问题：

- `processSystemMessage()` 固定使用 `default agent`
- 没有恢复发起 `spawn` 的原父 agent
- 也没有恢复原父 agent 的会话上下文

这会导致：

1. 异步结果可能由错误的 agent 继续处理
2. 异步结果可能写入错误的 session
3. 多 agent 场景下，结果语义和上下文都可能串掉

本方案的目标是：

1. 让异步 subagent 完成后，结果准确回传给原父 agent
2. 尽量复用现有 bus + system message 机制
3. 以最小改动修复 agent 和 session 恢复问题
4. 不保留旧格式兼容分支，让协议问题尽早暴露

---

## 当前实现分析

### 当前回传链路

```text
父 agent 执行 spawn
    ↓
spawn.ExecuteAsync(..., callback)
    ↓
SubagentManager.runTask(...)
    ↓
subagent 执行完成，触发 callback(result)
    ↓
callback 发布一条 system inbound message
    ↓
processSystemMessage() 处理 system 消息
    ↓
固定取 default agent
    ↓
default agent 继续跑一轮 agent loop
```

### 当前关键代码

#### 1. callback 回注 system 消息

文件：`pkg/agent/loop.go`

```go
_ = al.bus.PublishInbound(pubCtx, bus.InboundMessage{
    Channel:  "system",
    SenderID: fmt.Sprintf("async:%s", tc.Name),
    ChatID:   fmt.Sprintf("%s:%s", opts.Channel, opts.ChatID),
    Content:  content,
})
```

#### 2. processSystemMessage 固定使用 default agent

文件：`pkg/agent/loop.go`

```go
agent := al.GetRegistry().GetDefaultAgent()
sessionKey := routing.BuildAgentMainSessionKey(agent.ID)
```

### 问题根因

当前异步回注消息只携带了：

- 原始 `channel`
- 原始 `chatID`
- 工具结果内容

但没有携带：

- 父 agent ID
- 父 session key
- 原始 route 信息
- 任何足以恢复执行上下文的标识

因此 `processSystemMessage()` 无法知道：

- 是哪个 agent 发起了这次 `spawn`
- 应该把异步结果续接到哪个 session

所以它只能退化为：

- 固定交给 `default agent`
- 固定写入 `agent:<default-agent>:main`

---

## 修改目标

### 必须解决的问题

1. **恢复原父 agent**
   - 异步结果必须回到发起 `spawn` 的 agent

2. **恢复原父 session**
   - 异步结果必须续接到原父 agent 当时所在的会话

3. **保持现有异步架构不变**
   - 继续使用 `AsyncCallback`
   - 继续使用 bus 回注 `system` 消息
   - 不改成同步等待

4. **强制统一协议**
   - `processSystemMessage()` 只接受新的结构化 callback payload
   - 不再兼容旧格式
   - 发现协议问题时直接报错，而不是兜底吞掉

### 非目标

本次不处理以下问题：

1. 不重构整个 routing 架构
2. 不引入新的持久化任务状态表
3. 不修改 `RunToolLoop()` 的完成判定逻辑
4. 不处理 callback 直接发 `ForUser` 可能造成的重复输出问题

---

## 推荐方案

推荐采用：

**方案 A：在异步回注的 system 消息中显式携带父 agent ID + 父 session key。**

这是当前代码结构下改动最小、语义最稳定、实现最直接的方案。

### 核心思路

在父 agent 执行 `spawn` 时，callback 不仅要发布结果内容，还要把当时的执行上下文一起编码进 system 消息：

- `parent_agent_id`
- `parent_session_key`
- 原始 `channel`
- 原始 `chatID`
- 工具名 / sender
- 结果内容

然后 `processSystemMessage()` 在收到这条 system 消息时：

1. 严格解析父 agent ID
2. 严格查 registry 拿到该 agent
3. 严格使用传回的父 session key
4. 在该 agent 的上下文中继续跑 agent loop
5. 任一步缺失或不合法都直接返回错误，不再回退到 default agent

这样就能保证异步结果“回到原发起者”，同时把协议错误直接暴露出来。

---

## 详细设计

### 阶段 1：定义 system callback payload

#### 文件：`pkg/agent/loop.go`

当前实现把上下文信息塞进了 `SenderID` 和 `ChatID`，表达能力不足。

建议新增一个内部 payload 结构，用 JSON 序列化到 `InboundMessage.Content`。

建议新增：

```go
type asyncToolCallbackPayload struct {
    Version          int    `json:"version"`
    ToolName         string `json:"tool_name"`
    ParentAgentID    string `json:"parent_agent_id"`
    ParentSessionKey string `json:"parent_session_key"`
    OriginChannel    string `json:"origin_channel"`
    OriginChatID     string `json:"origin_chat_id"`
    Content          string `json:"content"`
}
```

字段说明：

| 字段 | 作用 |
|------|------|
| `Version` | 固定协议版本，便于显式校验 |
| `ToolName` | 标记是哪一个异步工具返回 |
| `ParentAgentID` | 恢复原父 agent |
| `ParentSessionKey` | 恢复原父会话 |
| `OriginChannel` | 回用户输出时继续使用原 channel |
| `OriginChatID` | 回用户输出时继续使用原 chat |
| `Content` | 提供给父 agent 的 `ForLLM` 内容 |

要求：

- `Version` 必须存在且当前固定为 `1`
- 以上字段全部视为必需字段
- 缺任一字段都视为协议错误

---

### 阶段 2：在异步 callback 中记录父 agent 上下文

#### 文件：`pkg/agent/loop.go`

当前 callback 创建位置在执行工具调用时：

```go
asyncCallback := func(_ context.Context, result *tools.ToolResult) {
    ...
}
```

这里其实已经能拿到：

- `agent.ID`
- 当前 `sessionKey`
- `opts.Channel`
- `opts.ChatID`
- `tc.Name`

因此只需要在 callback 中把这些数据打包进 payload。

#### 修改建议

将当前：

```go
_ = al.bus.PublishInbound(pubCtx, bus.InboundMessage{
    Channel:  "system",
    SenderID: fmt.Sprintf("async:%s", tc.Name),
    ChatID:   fmt.Sprintf("%s:%s", opts.Channel, opts.ChatID),
    Content:  content,
})
```

改成类似：

```go
payload := asyncToolCallbackPayload{
    Version:          1,
    ToolName:         tc.Name,
    ParentAgentID:    agent.ID,
    ParentSessionKey: sessionKey,
    OriginChannel:    opts.Channel,
    OriginChatID:     opts.ChatID,
    Content:          content,
}

payloadJSON, err := json.Marshal(payload)
if err != nil {
    logger.ErrorCF("agent", "Failed to marshal async callback payload", map[string]any{
        "tool":  tc.Name,
        "error": err.Error(),
    })
    return
}

_ = al.bus.PublishInbound(pubCtx, bus.InboundMessage{
    Channel:  "system",
    SenderID: "async_callback",
    ChatID:   opts.ChatID,
    Content:  string(payloadJSON),
})
```

#### 这样改的原因

1. `SenderID` 不再承担业务数据编码职责
2. `ChatID` 不再用 `channel:chatID` 这种拼接格式塞额外信息
3. 上下文恢复所需信息完整且清晰
4. 后续如果有其他异步工具，也可以复用这一套 payload
5. 新旧协议边界清晰，便于直接暴露错误

---

### 阶段 3：让 processSystemMessage 严格识别 async callback payload

#### 文件：`pkg/agent/loop.go`

`processSystemMessage()` 需要从“固定 default agent”改为：

1. 只接受新的 async callback payload
2. 严格恢复原父 agent + 原父 session
3. 任何格式错误、字段缺失、agent 缺失都直接报错
4. 不再保留旧格式兼容
5. 不再回退到 default agent

#### 建议新增辅助函数

```go
func parseAsyncToolCallbackPayload(msg bus.InboundMessage) (*asyncToolCallbackPayload, error) {
    if msg.Channel != "system" {
        return nil, fmt.Errorf("processSystemMessage called with non-system channel: %s", msg.Channel)
    }
    if msg.SenderID != "async_callback" {
        return nil, fmt.Errorf("unsupported system sender: %s", msg.SenderID)
    }

    var payload asyncToolCallbackPayload
    if err := json.Unmarshal([]byte(msg.Content), &payload); err != nil {
        return nil, fmt.Errorf("invalid async callback payload json: %w", err)
    }
    if payload.Version != 1 {
        return nil, fmt.Errorf("unsupported async callback payload version: %d", payload.Version)
    }
    if payload.ToolName == "" {
        return nil, fmt.Errorf("missing tool_name in async callback payload")
    }
    if payload.ParentAgentID == "" {
        return nil, fmt.Errorf("missing parent_agent_id in async callback payload")
    }
    if payload.ParentSessionKey == "" {
        return nil, fmt.Errorf("missing parent_session_key in async callback payload")
    }
    if payload.OriginChannel == "" {
        return nil, fmt.Errorf("missing origin_channel in async callback payload")
    }
    if payload.OriginChatID == "" {
        return nil, fmt.Errorf("missing origin_chat_id in async callback payload")
    }
    if payload.Content == "" {
        return nil, fmt.Errorf("missing content in async callback payload")
    }
    return &payload, nil
}
```

#### 修改 `processSystemMessage()` 主逻辑

当前逻辑类似于：

```go
agent := al.GetRegistry().GetDefaultAgent()
sessionKey := routing.BuildAgentMainSessionKey(agent.ID)
```

建议改成：

```go
payload, err := parseAsyncToolCallbackPayload(msg)
if err != nil {
    return "", err
}

agent, found := al.GetRegistry().GetAgent(payload.ParentAgentID)
if !found {
    return "", fmt.Errorf("parent agent not found for async callback: %s", payload.ParentAgentID)
}

return al.runAgentLoop(ctx, agent, processOptions{
    SessionKey:      payload.ParentSessionKey,
    Channel:         payload.OriginChannel,
    ChatID:          payload.OriginChatID,
    UserMessage:     fmt.Sprintf("[System: async:%s] %s", payload.ToolName, payload.Content),
    DefaultResponse: "Background task completed.",
    EnableSummary:   false,
    SendResponse:    true,
})
```

要求：

- `processSystemMessage()` 不再做旧格式解析
- `processSystemMessage()` 不再从 `msg.ChatID` 拆 `channel:chatID`
- `processSystemMessage()` 不再使用 default agent 作为 fallback

---

## 最小改动点清单

### 需要修改的文件

| 文件 | 修改类型 | 说明 |
|------|---------|------|
| `pkg/agent/loop.go` | 修改 | 定义 async callback payload；修改工具执行 callback；重写 `processSystemMessage()` 为严格新协议；新增严格解析辅助函数 |

### 暂时不需要改动的文件

| 文件 | 原因 |
|------|------|
| `pkg/tools/spawn.go` | `spawn` 只负责把 callback 传给 manager，不关心父 agent 恢复逻辑 |
| `pkg/tools/subagent.go` | subagent 完成后仍然只需触发 callback，无需知道父 agent 恢复细节 |
| `pkg/tools/registry.go` | 现有 async callback 传递机制已经够用 |
| `pkg/routing/*` | 本方案直接恢复 sessionKey，不依赖重新 route |

---

## 为什么不推荐“重新走路由恢复父 agent”

另一种思路是：

- callback 只传回原始 channel/chatID
- `processSystemMessage()` 再调用 `ResolveRoute()` 去找 agent

但这条路不推荐，原因是：

1. 当前 system 消息没有完整 route 元信息
   - 可能缺少 `AccountID`
   - 可能缺少 `Peer`
   - 可能缺少 `GuildID`
   - 可能缺少 `Metadata`

2. route 结果可能已经变化
   - 异步任务执行期间，binding 规则可能被修改
   - 回调时重新 route，未必还能命中原 agent

3. route 也无法恢复原 session
   - 即使重新命中了同一个 agent，也不等于拿回原来那条会话

因此：

> **异步结果要回到原父 agent，最可靠的方法不是重新计算，而是在发起时就把父 agent 和父 session 明确保存下来。**

---

## 错误暴露策略

本方案明确不做兼容兜底。

### 原则

1. **协议错误直接报错**
   - 不吞掉
   - 不静默回退
   - 不自动切到 default agent

2. **找不到父 agent 直接报错**
   - 不伪造 fallback 语义

3. **旧格式 system 消息直接视为不支持**
   - 这样可以快速发现所有仍在使用旧协议的调用点

### 预期收益

1. 尽早暴露所有旧协议残留路径
2. 避免“看起来还能跑，但上下文已串”的隐性错误
3. 让测试和运行时行为保持一致
4. 逼迫所有异步 system message 调用点统一到同一协议

---

## 测试计划

### 单元测试

#### 1. 测试 callback payload 编码

目标：验证异步 callback 发布的 system 消息内容包含完整上下文。

建议新增测试：

- `TestAsyncCallbackPublishesParentAgentContext`

验证点：

- `SenderID == "async_callback"`
- payload 中包含 `version=1`
- payload 中包含 `parent_agent_id`
- payload 中包含 `parent_session_key`
- payload 中包含 `origin_channel`
- payload 中包含 `origin_chat_id`
- payload 中包含 `tool_name`

#### 2. 测试 `processSystemMessage()` 恢复原父 agent

建议新增测试：

- `TestProcessSystemMessage_UsesParentAgentFromAsyncPayload`

验证点：

- 不使用 default agent
- 使用 payload 里的 `ParentAgentID`
- 使用 payload 里的 `ParentSessionKey`

#### 3. 测试无效 payload 直接报错

建议新增测试：

- `TestProcessSystemMessage_InvalidPayloadReturnsError`

验证点：

- JSON 非法时报错
- 缺字段时报错
- version 非 1 时报错

#### 4. 测试找不到父 agent 直接报错

建议新增测试：

- `TestProcessSystemMessage_ParentAgentMissingReturnsError`

验证点：

- 原父 agent 不存在时直接返回错误
- 不回退到 default agent

#### 5. 测试旧 sender 直接报错

建议新增测试：

- `TestProcessSystemMessage_RejectsLegacySenderID`

验证点：

- `async:spawn` 这种旧 sender 格式不再兼容
- 明确返回 unsupported system sender 错误

---

## 风险评估

### 风险 1：切换后旧消息会立即失败

影响：

- 所有仍使用旧 system callback 格式的路径会直接报错

说明：

- 这是预期行为，不是副作用
- 目的就是尽快把问题暴露出来

### 风险 2：payload 序列化失败会导致异步结果中断

影响：

- 极端情况下异步结果无法回注

缓解：

- 明确记录 error log
- 测试覆盖 marshal/unmarshal 逻辑

### 风险 3：sessionKey 恢复后上下文长度增加

影响：

- 原父 session 被正确续接后，可能带入更多历史上下文

缓解：

- 这是语义正确行为
- 不属于本次修复的副作用问题

---

## 实现顺序

1. 在 `pkg/agent/loop.go` 中定义 async callback payload 结构体
2. 修改工具执行 callback，将父 agent / session 信息编码进 system message
3. 重写 `processSystemMessage()`，只接受严格的新 payload 协议
4. 删除旧的 `channel:chat_id` 解析逻辑
5. 增加单元测试覆盖正常路径和严格失败路径

---

## 预期效果

修改完成后，异步 `spawn` 的结果回传链路将变为：

```text
父 agent A 在 session S 中执行 spawn
    ↓
subagent 异步执行
    ↓
callback 完成时，发布包含 A + S 信息的 system 消息
    ↓
processSystemMessage() 严格解析 payload
    ↓
恢复到父 agent A
    ↓
恢复到 session S
    ↓
父 agent A 基于自己的上下文继续处理 subagent 结果
```

最终效果：

1. 异步结果回到正确的父 agent
2. 异步结果回到正确的 session
3. 多 agent 场景下不再串 agent
4. 保留现有 bus 异步架构，改动范围小
5. 旧协议问题会立即暴露，不再被 fallback 掩盖

---

## 后续可选优化

本方案完成后，可以继续考虑以下优化，但不建议与本次修复混在一起：

1. **去重输出**
   - callback 直接发 `ForUser`
   - 父 agent 后续又可能再总结一遍
   - 可以后续考虑避免重复回复

2. **统一异步工具回传协议**
   - 不只 `spawn`
   - 其他异步工具也可复用同一 payload 协议

3. **将 callback payload 提升为 bus metadata**
   - 比 JSON 字符串更结构化
   - 但需要改动 bus message 结构

4. **记录 taskID -> parent context 映射**
   - 便于更复杂的异步任务管理
   - 但当前不是必须
