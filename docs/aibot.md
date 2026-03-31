# WeCom AI Bot 长连接改造方案

## 目标

参考 `aibot-node-sdk` 的 WebSocket 实现，把当前基于 **HTTP webhook + stream 轮询 + response_url 回补** 的 `pkg/channels/wecom/aibot.go`，扩展为一套 **WebSocket 长连接版** `pkg/channels/wecom/aibot_websocket.go`。

这份文档只讨论**最小改动方案**，目标是：

1. **保留现有 `aibot.go` 不动**，继续支持 webhook 版。
2. 新增 `aibot_websocket.go`，实现企业微信 AI Bot WebSocket 长连接。
3. 尽量**复用现有 BaseChannel / Manager / bus**，不大改全局接口。
4. 先把**文本消息、欢迎语、基础事件、最终回复**跑通；媒体与模板卡片按阶段补齐。

---

## 现状总结

### 当前 `aibot.go` 的工作方式

当前实现是典型的 webhook 拉流模式：

- 企业微信通过 HTTP 回调消息到 `ServeHTTP`：`pkg/channels/wecom/aibot.go:278`
- 文本消息进入 `handleTextMessage` 后，先创建 `streamTask`：`pkg/channels/wecom/aibot.go:483`
- 首次立即返回一个 `finish=false` 的 stream 响应：`pkg/channels/wecom/aibot.go:561`
- 后续企业微信不断以 `msgtype=stream` 轮询，服务端在 `handleStreamMessage` / `getStreamResponse` 中返回下一步：`pkg/channels/wecom/aibot.go:565`、`pkg/channels/wecom/aibot.go:683`
- 如果超过 deadline，就先关闭 stream，再改用 `response_url` 主动发最终结果：`pkg/channels/wecom/aibot.go:694`、`pkg/channels/wecom/aibot.go:760`

也就是说，它是：

- **入口**：HTTP webhook
- **流式协议**：企业微信反复轮询
- **超时兜底**：`response_url`

### 这套模式和 WebSocket 版的核心差异

WebSocket 版不是“企业微信来轮询我”，而是：

- 我方主动连接 `wss://openws.work.weixin.qq.com`
- 连接建立后发 `aibot_subscribe` 做认证
- 企业微信通过长连接主动推送：
  - `aibot_msg_callback`
  - `aibot_event_callback`
- 我方通过同一条长连接回发：
  - `aibot_respond_msg`
  - `aibot_respond_welcome_msg`
  - `aibot_respond_update_msg`
  - `aibot_send_msg`
  - `aibot_upload_media_*`

所以 WebSocket 版不再依赖：

- URL 验签
- AES 解密 webhook 包
- HTTP stream 轮询

但仍然可以保留：

- `response_url` 作为超时/回执失败兜底
- `BaseChannel.HandleMessage` 作为统一入站桥
- `Manager` 的出站派发与重试逻辑

---

## 参考 `aibot-node-sdk` 后，应吸收的关键点

参考文件：

- `aibot-node-sdk/src/ws.ts`
- `aibot-node-sdk/src/client.ts`
- `aibot-node-sdk/src/message-handler.ts`
- `aibot-node-sdk/src/types/message.ts`

### 1. 连接模型

Node SDK 的连接模型很清晰：

- 默认地址：`wss://openws.work.weixin.qq.com`
- open 后立即发送认证帧：`aibot_subscribe`
- 认证成功后启动 heartbeat
- heartbeat 用 `ping`
- 断线指数退避重连
- `disconnected_event` 表示该 bot 有新连接建立，旧连接应主动退出且**不要立即重连**

这个模型应基本原样搬到 Go。

### 2. 回复不是按 chat_id，而是按 req_id 串行

Node SDK 最重要的设计点不是“怎么连”，而是“怎么回”。

它对每个 `req_id` 维护：

- 回复队列
- 正在等待 ack 的 pending
- ack 超时控制

原因是 WebSocket 被动回复时，真正的回包关联键是 **回调帧里的 `headers.req_id`**，不是 `chatid`。

这点和当前 `aibot.go` 很不一样：

- 当前 webhook 版靠 `chatTasks[chatID]` 做 FIFO 关联
- WebSocket 版真正的协议关联应是 `req_id`

### 3. 事件与消息是两条回调通道

Node SDK 区分：

- `aibot_msg_callback`
- `aibot_event_callback`

Go 版也应照此区分，不要继续混成 webhook 里的 `msgtype == event` 那一套单入口分发。

### 4. 文件下载与媒体上传应走独立能力

Node SDK 长连接版支持：

- 下载消息里的加密文件 URL，并用消息内 `aeskey` 解密
- 通过 WebSocket 做分片上传 `init -> chunk -> finish`
- 被动回复媒体 / 主动发送媒体

这说明 WebSocket 版企业微信 AI Bot 的能力实际上比当前 webhook 版强。

---

## 推荐的最小改动路线

## 结论

**不要把 `aibot.go` 直接改成 WebSocket。**

应该采用下面的结构：

- 保留现有：`pkg/channels/wecom/aibot.go`（webhook 轮询版）
- 新增：`pkg/channels/wecom/aibot_websocket.go`（WebSocket 长连接版）
- 在 `pkg/channels/wecom/init.go` / factory 里按配置选择实例化哪一个实现

这是最小改动、风险最低的方案。

### 为什么不建议直接覆盖 `aibot.go`

因为两套协议差异太大：

1. webhook 版依赖 HTTP handler、验签、解密、轮询
2. websocket 版依赖长连接、认证帧、心跳、回执、重连
3. 二者只有“消息体字段”相似，生命周期完全不同

如果把两套协议硬塞进一个文件里，最终会得到一堆：

- `if mode == "websocket" { ... } else { ... }`
- 共享结构体里塞两套互斥字段
- 清理逻辑、Send 逻辑、错误处理都混在一起

这会让后续维护非常差。

---

## 推荐的文件改动清单

### 1. 新增主实现

新增：

- `pkg/channels/wecom/aibot_websocket.go`

建议先把主逻辑都放这个文件里；如果后面超过 600 行，再拆：

- `aibot_websocket_types.go`
- `aibot_websocket_media.go`

但第一版完全可以先只新增一个主文件。

### 2. 配置扩展

修改：

- `pkg/config/config.go`

给 `WeComAIBotConfig` 增加 WebSocket 模式所需字段。

推荐新增字段：

```go
type WeComAIBotConfig struct {
    Enabled            bool
    Mode               string // "webhook" | "websocket"，默认 webhook

    // webhook 模式
    Token              string
    EncodingAESKey     string
    WebhookPath        string

    // websocket 模式
    BotID              string
    Secret             string
    WSURL              string // 默认 wss://openws.work.weixin.qq.com
    Scene              int
    PlugVersion        string
    HeartbeatInterval  int // 秒，默认 30
    ReconnectInterval  int // 秒，作为指数退避基数，默认 1
    MaxReconnectAttempts int
    MaxAuthFailureAttempts int
    ReplyAckTimeout    int // 秒，默认 5
    MaxReplyQueueSize  int // 默认 500

    // 通用
    AllowFrom          FlexibleStringSlice
    ReplyTimeout       int
    MaxSteps           int
    WelcomeMessage     string
    ReasoningChannelID string
}
```

说明：

- `Token` / `EncodingAESKey` 仅 webhook 模式需要
- `BotID` / `Secret` 仅 websocket 模式需要
- `WSURL` 默认值应与 Node SDK 一致

### 3. factory 切换

修改：

- `pkg/channels/wecom/init.go`

推荐继续保留 channel 名称 `wecom_aibot`，但按 `Mode` 决定实例：

```go
channels.RegisterFactory("wecom_aibot", func(cfg *config.Config, b *bus.MessageBus) (channels.Channel, error) {
    if strings.EqualFold(cfg.Channels.WeComAIBot.Mode, "websocket") {
        return NewWeComAIBotWebSocketChannel(cfg.Channels.WeComAIBot, b)
    }
    return NewWeComAIBotChannel(cfg.Channels.WeComAIBot, b)
})
```

这样做的好处：

- 不影响上层 binding / agent routing
- 不需要新增新的 channel name
- 用户只切配置，不改其他路由

---

## `aibot_websocket.go` 的核心设计

## 1. 结构体设计

建议主结构：

```go
type WeComAIBotWebSocketChannel struct {
    *channels.BaseChannel
    config config.WeComAIBotConfig

    ctx    context.Context
    cancel context.CancelFunc

    conn    *websocket.Conn
    connMu  sync.RWMutex
    writeMu sync.Mutex

    authenticated atomic.Bool

    reconnectAttempts   int
    authFailureAttempts int
    reconnectTimer      *time.Timer

    heartbeatTicker *time.Ticker
    missedPongCount int

    reqTasks map[string]*wsTask      // req_id -> task
    chatTasks map[string][]*wsTask   // chatID -> FIFO
    taskMu   sync.RWMutex

    replyQueues map[string][]*replyItem // req_id -> queued replies
    pendingAcks map[string]*pendingAck  // req_id -> waiting ack
    replyMu    sync.Mutex
}
```

其中任务结构：

```go
type wsTask struct {
    ReqID       string
    StreamID    string
    MsgID       string
    ChatID      string
    ChatType    string
    UserID      string
    ResponseURL string
    CreatedTime time.Time

    ctx    context.Context
    cancel context.CancelFunc

    Finished bool
}
```

`replyItem` / `pendingAck` 基本照着 Node SDK 的思路实现即可。

### 为什么仍然保留 `chatTasks`

虽然 WebSocket 协议的真正回包键是 `req_id`，但当前 `Send(ctx, msg bus.OutboundMessage)` 的输入只有：

- `Channel`
- `ChatID`
- `Content`

并没有 `req_id`。

也就是说，在**不改 bus.OutboundMessage** 的前提下，`Send()` 依旧只能像现在的 `aibot.go` 一样，靠 `chatID` 找“当前这条会话里最早未完成的任务”。

所以最小改动方案是：

- 协议层：真正发送时用 `req_id`
- 桥接层：`Send()` 仍通过 `chatTasks[chatID]` 做 FIFO 找任务

这和当前 `aibot.go` 的设计保持一致，改动最小。

> 这也是整个方案里最关键的“少改全局接口”的点。

---

## 2. Start / Stop 生命周期

### Start

`Start(ctx)` 中做这些事：

1. 保存 `c.ctx, c.cancel = context.WithCancel(ctx)`
2. 初始化 task map / reply queue / pending ack
3. 调用 `connect()` 建立 WebSocket
4. 启动 reader goroutine
5. 启动清理 goroutine
6. 启动重连控制
7. `SetRunning(true)`

### Stop

`Stop(ctx)` 中做这些事：

1. `SetRunning(false)`
2. `cancel()`
3. 关闭 heartbeat
4. 关闭连接
5. 清理 pending acks，唤醒等待中的发送
6. 取消所有 task ctx
7. 清空 map

这块可以参考：

- `pkg/channels/onebot/onebot.go`
- `pkg/channels/pico/pico.go`
- `aibot-node-sdk/src/ws.ts`

---

## 3. WebSocket 连接管理

### 建连

建立到：

- `wss://openws.work.weixin.qq.com`

如果配置里有 `WSURL`，允许覆盖。

### 认证

连接成功后立即发送：

```json
{
  "cmd": "aibot_subscribe",
  "headers": {"req_id": "aibot_subscribe_xxx"},
  "body": {
    "bot_id": "...",
    "secret": "...",
    "scene": 1,
    "plug_version": "..."
  }
}
```

认证成功后：

- `authenticated = true`
- 重置 `reconnectAttempts`
- 启动 heartbeat

认证失败后：

- 关闭连接
- 按 auth failure 计数器指数退避重连

### 心跳

按 Node SDK 方案：

- 周期发送 `ping`
- 如果连续 2 次未收到 ack，则认为连接已死，主动断开

### 重连

重连分两类：

1. **网络断连**：`reconnectAttempts`
2. **认证失败**：`authFailureAttempts`

都用指数退避，但计数分开。

### `disconnected_event`

如果服务端推送 `event.disconnected_event`，表示：

- 有新的连接已经顶掉当前连接
- 当前连接应关闭
- 不应立即自动重连，否则会互相踢

这块应与 Node SDK 保持一致。

---

## 4. 收包分发模型

长连接读循环里，收到帧后分 4 类：

### A. 消息回调

```json
{ "cmd": "aibot_msg_callback", "headers": {"req_id": "..."}, "body": {...} }
```

处理函数：

- `handleMessageCallback(frame)`

### B. 事件回调

```json
{ "cmd": "aibot_event_callback", "headers": {"req_id": "..."}, "body": {...} }
```

处理函数：

- `handleEventCallback(frame)`

### C. 认证 / 心跳 / 回复回执

这类通常没有 `cmd`，主要看：

- `headers.req_id`
- `errcode`
- `errmsg`

分类逻辑可以直接仿 Node SDK：

- `req_id` 前缀是 `aibot_subscribe` -> 认证回执
- `req_id` 前缀是 `ping` -> 心跳回执
- `req_id` 存在于 `pendingAcks` -> 回复 ack

### D. 未知帧

只打日志，不继续分发。

---

## 5. 文本消息处理：从轮询改为被动流式回包

这是整个改造最关键的一段。

### 当前 webhook 版的逻辑

当前 `aibot.go` 收到文本后：

- 生成 `streamID`
- 立即返回 `finish=false`
- 企业微信再来轮询
- LLM 回答后，再在轮询响应里返回 `finish=true`

### WebSocket 版的对应逻辑

收到 `aibot_msg_callback` 的文本消息后：

1. 从 `frame.headers.req_id` 取到 `reqID`
2. 生成 `streamID`
3. 建立 `wsTask`
4. 把它放进：
   - `reqTasks[reqID]`
   - `chatTasks[chatID]`
5. **立即通过 WebSocket 发送第一帧**：

```json
{
  "cmd": "aibot_respond_msg",
  "headers": {"req_id": "原始 req_id"},
  "body": {
    "msgtype": "stream",
    "stream": {
      "id": "生成的 streamID",
      "finish": false,
      "content": ""
    }
  }
}
```

6. 然后异步调用 `HandleMessage(...)` 把内容送进 bus / agent
7. 等后续 `Send()` 被 manager 调用时，再用同一个 `reqID + streamID` 发最终内容

### 这一步的重要意义

这相当于把 webhook 版“首次 HTTP 返回 finish=false”的语义，平移成了“首次 WebSocket 回一帧 finish=false”。

这样做有 3 个好处：

1. 和当前 `aibot.go` 的行为一致，认知成本低
2. 即使 agent 较慢，也已经完成了第一帧回复，不会卡在入口
3. 后续最终结果可以继续沿用同一个 `streamID`

---

## 6. `Send()` 的新语义

当前 `aibot.go` 的 `Send()` 做的是：

- 根据 `msg.ChatID` 找 task
- 如果 stream 已关闭，走 `response_url`
- 否则把结果塞进 `answerCh`

WebSocket 版应该改成：

1. 根据 `msg.ChatID` 在 `chatTasks` 里找队首 task
2. 拿到它的 `ReqID` 和 `StreamID`
3. 用 `aibot_respond_msg` 直接回企业微信
4. 成功后 `removeTask(task)`
5. 失败时按规则兜底

也就是说，WebSocket 版不再需要 `answerCh` 这种“等轮询时再取”的机制。

### 建议的 `Send()` 处理顺序

```text
Send(msg)
  -> 按 chatID 找 task
  -> 找不到：记录 debug，返回 nil
  -> 找到：把 msg.Content 按协议上限切片
      -> 前 N-1 片：finish=false
      -> 最后一片：finish=true
  -> 每一片都走 req_id 串行回复队列
  -> 全部 ack 成功后 removeTask
```

---

## 7. 为什么 WebSocket 版要禁用 Manager 的自动分片

当前 `aibot.go` 在 `NewBaseChannel(...)` 里设置了：

```go
channels.WithMaxMessageLength(2048)
```

这会触发 `Manager` 在通道外层把长消息切成多个 `bus.OutboundMessage`。这个设计对普通文本通道是好的，但对 WebSocket AI Bot **不合适**。

原因是：

- WebSocket AI Bot 的多段回复必须共用同一个 `req_id + stream.id`
- `Manager` 外层分片后，`Send()` 拿到的是多条独立消息，无法保证它们一定对应同一条被动回复语义
- 真正知道如何按 stream 协议分片的，应该是 channel 自己

### 所以建议

`aibot_websocket.go` 的 `BaseChannel` 不设置 `WithMaxMessageLength(...)`，或者设置为 `0`。

然后在 `Send()` 内部自己按 WebSocket 协议做分片。

### 分片建议

优先按**字节长度**而不是 rune 数切，目标上限按企业微信 stream content 限制来。

建议：

- 每片保守控制在 `<= 16KB` 文本字节
- 前面的片：`finish=false`
- 最后一片：`finish=true`

---

## 8. 回执队列：必须实现

这块不要省。

Node SDK 里最值得抄的是：

- `replyQueues map[reqID][]ReplyQueueItem`
- `pendingAcks map[reqID]pendingAck`
- 同一个 `req_id` 串行发送
- 每条发送后等 ack 或 timeout，再发下一条

Go 版建议也这样做。

### 为什么必须做串行 ack 队列

因为同一个 `req_id` 下，可能会发送：

1. 初始 `finish=false`
2. 若干中间片段 `finish=false`
3. 最后一片 `finish=true`

如果不串行等待 ack：

- 服务端可能乱序处理
- 超时与 ack 容易竞态
- 同一个 `req_id` 的多帧状态会混乱

### 最小实现建议

提供 3 个内部方法：

- `enqueueReply(reqID string, frame wsFrame) error`
- `processReplyQueue(reqID string)`
- `handleReplyAck(reqID string, ack wsFrame)`

ack 超时建议默认 5 秒，和 Node SDK 保持一致。

---

## 9. 失败兜底策略

WebSocket 版不能只靠一条链路。

推荐兜底顺序：

### 第一优先级：被动回复

优先使用：

- `aibot_respond_msg`

这是语义最正确的方式，因为它和收到的 `req_id` 一一对应。

### 第二优先级：`response_url`

如果出现这些情况：

- 当前连接断了
- ack 明确失败
- 服务端返回 req_id 无效
- task 已经过了被动回复窗口

且消息里带有 `response_url`，则回退到：

- `sendViaResponseURL(responseURL, content)`

这里可以直接复用当前 `aibot.go` 的 `sendViaResponseURL`：`pkg/channels/wecom/aibot.go:764`

### 第三优先级：主动发送消息

如果：

- 没有 `response_url`
- 但 WebSocket 已重新认证成功

可以用：

- `aibot_send_msg`

按 `chatID` 主动推一条 markdown 消息作为兜底。

### 兜底说明

语义上优先级是：

```text
被动回复 > response_url > aibot_send_msg
```

因为：

- 被动回复最符合原消息上下文
- `response_url` 次之，但它是一次性的、消息级回补
- `aibot_send_msg` 更像主动消息，最后兜底

---

## 10. 事件处理方案

### `enter_chat`

这个最容易先支持。

收到：

- `aibot_event_callback`
- `body.event.eventtype == "enter_chat"`

如果 `config.WelcomeMessage != ""`，就发送：

- `aibot_respond_welcome_msg`

这相当于把当前 webhook 版 `handleEventMessage()` 里的欢迎语能力平移过去。

### `template_card_event`

第一期建议：

- 先识别
- 先打日志
- 暂不接 bus

因为当前项目里并没有 AI Bot 模板卡片的统一上层抽象。

### `feedback_event`

第一期建议：

- 先识别
- 打日志
- 如有需要，可以作为 metadata 发布给 agent

---

## 11. 媒体能力建议分阶段做

## 第一阶段：只做文本长连接

先支持：

- text 入站
- enter_chat 欢迎语
- WebSocket reply
- response_url fallback

这是最小可用闭环。

## 第二阶段：入站媒体

参考 `aibot-node-sdk/src/types/message.ts`，WebSocket 长连接版的图片/文件/视频/语音消息会多带：

- `url`
- `aeskey`

建议实现方式：

1. 收到媒体消息
2. 下载加密文件
3. 用 `aeskey` 解密
4. 存入现有 `MediaStore`
5. 调 `HandleMessage(..., mediaRefs, ...)`

这块可以参考仓库里 OneBot 的媒体入站处理方式：`pkg/channels/onebot/onebot.go:713`

## 第三阶段：出站媒体

利用现有 `channels.MediaSender`：`pkg/channels/media.go:13`

在 `aibot_websocket.go` 中实现：

- `SendMedia(ctx, msg bus.OutboundMediaMessage) error`

内部走：

1. `aibot_upload_media_init`
2. `aibot_upload_media_chunk`
3. `aibot_upload_media_finish`
4. 再用 `aibot_respond_msg` 或 `aibot_send_msg` 发 media_id

这一层完全可以做成第二批，不阻塞文本链路。

---

## 12. 建议复用 / 提取的公共代码

为了保持**最小改动**，不建议一开始做大抽象。

### 可以复用的

直接复用：

- `generateStreamID()`：`pkg/channels/wecom/aibot.go:898`
- `sendViaResponseURL()`：`pkg/channels/wecom/aibot.go:764`

### 可以小范围提取的

如果后面发现重复，再提到 `aibot_common.go`：

- stream ID 生成
- response_url 发送
- chatID 推导逻辑（群聊用 chatid，单聊用 userid）
- sender / peer / metadata 组装

### 不建议现在抽的

不建议一开始就把 webhook 和 websocket 的任务模型硬合并，因为它们差异太大：

- webhook 版是 `streamTask + poll`
- websocket 版是 `wsTask + req_id ack`

强行共用只会增加复杂度。

---

## 13. 与现有 bus / manager 的兼容性判断

## 好消息

下面这些都能直接复用：

- `BaseChannel.HandleMessage()`：`pkg/channels/base.go:232`
- `Manager` 的 outbound worker：`pkg/channels/manager.go:508`
- `MediaSender` 扩展点：`pkg/channels/media.go:13`
- `WebhookHandler` 是可选接口，WebSocket 版可以不实现：`pkg/channels/webhook.go:5`

## 唯一真正的接口缺口

`bus.OutboundMessage` 没有 correlation metadata：`pkg/bus/types.go:32`

所以 WebSocket 版没法像协议层那样直接以 `req_id` 为主键完成 `Send()`；必须继续沿用当前 `aibot.go` 的策略：

- 按 `chatID` 维护 FIFO
- `Send()` 时取队首 task

### 这是否可接受？

**短期可接受。**

原因：

- 当前 webhook 版本来就是这么做的
- 这能做到最小改动
- 大多数场景下，同一个 chat 的并发消息不会高到让 FIFO 失效

### 什么时候需要升级 bus

如果以后要彻底把 AI Bot 做成严格协议级回复，应考虑给 `bus.OutboundMessage` 增加 `Metadata` 或 `CorrelationID`，让 agent 回包能显式携带：

- `req_id`
- `stream_id`
- `reply_mode`

但这不是第一阶段必须做的事。

---

## 14. 实施步骤建议

### Phase 1：打通 WebSocket 文本闭环

1. 扩展 `WeComAIBotConfig`
2. 在 `init.go` 里按 `mode` 选择实现
3. 新增 `aibot_websocket.go`
4. 实现：
   - connect / auth / heartbeat / reconnect
   - 读循环与帧分类
   - 文本消息回调
   - 初始 `finish=false`
   - `Send()` 最终 `finish=true`
   - `response_url` fallback
   - `enter_chat` 欢迎语
5. 为 websocket 通道禁用 manager 自动分片

### Phase 2：完善可靠性

1. ack 串行队列
2. cleanup 过期 task
3. `disconnected_event` 处理
4. `aibot_send_msg` 主动兜底
5. 更完整的错误分类

### Phase 3：媒体与扩展能力

1. 入站图片/文件/视频/语音下载解密
2. `SendMedia`
3. 分片上传媒体
4. 模板卡片 / feedback_event

---

## 15. 测试建议

至少补这些测试：

### 单元测试

1. **认证回执分类**
   - `req_id` 为 `aibot_subscribe_xxx` 时进入 auth 分支
2. **心跳回执分类**
   - `req_id` 为 `ping_xxx` 时重置 missed pong
3. **回复 ack 队列**
   - 同一 req_id 多条消息必须串行
4. **chatTasks FIFO**
   - 同一 chat 两条并发消息时，`Send()` 应取队首任务
5. **response_url fallback**
   - 模拟 ws ack 失败时转发到 response_url
6. **长文本切片**
   - 多片 stream，前片 `finish=false`，最后一片 `finish=true`

### 集成测试

1. fake ws server 模拟：
   - open
   - auth ack
   - 推送 `aibot_msg_callback`
   - 验证客户端立即回第一帧 `finish=false`
   - 再验证 `Send()` 后回最终 `finish=true`
2. 模拟断线重连
3. 模拟 `disconnected_event`
4. 模拟欢迎语事件

---

## 16. 最终建议

如果目标是“尽快得到一个可工作的长连接版”，我建议按下面的边界执行：

### 第一版必须做

- `aibot_websocket.go`
- 配置增加 `mode/bot_id/secret/ws_url`
- factory 按 `mode` 切换
- WebSocket 连接、认证、心跳、重连
- 文本消息接收
- 初始 `finish=false`
- `Send()` 用 `aibot_respond_msg` 回最终结果
- `response_url` fallback
- `enter_chat` 欢迎语

### 第一版不要做太多抽象

不要一上来就：

- 合并 webhook 与 websocket 两套 task 模型
- 改 bus 的全局消息结构
- 抽象一个过大的 WeCom AI Bot 通用框架

### 第一版最重要的设计点

只有 3 个：

1. **连接管理要稳定**：auth / heartbeat / reconnect / disconnected_event
2. **回复关联要正确**：协议层用 `req_id`，桥接层用 `chatID FIFO`
3. **分片必须在 channel 内部做**：不能继续依赖 manager 外部分片

---

## 一句话结论

最合适的改法不是“重写 `aibot.go`”，而是：

> **保留现有 webhook 版 `aibot.go`，新增一个 `aibot_websocket.go`，通过 `mode=websocket` 切换；协议层按 `req_id` 做被动回复，兼容层继续沿用 `chatID -> FIFO task`，这样改动最小、风险最低、最符合当前仓库结构。**
