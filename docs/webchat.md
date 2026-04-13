# WebChat 对接说明

## 结论

当前系统里虽然 `channels` 配置项中没有单独的 `webchat`，但源码里已经存在一个可直接用于网页聊天的原生 WebSocket 通道：`pico`。

网页聊天当前使用的地址：

```text
ws://192.168.10.21:18790/pico/ws?token=00ef90f7b09202706a20f470a4bb13bc&session_id=53cb0968-2b4c-4fd2-94ce-01fe1369cd27
```

这说明现有 WebChat 实际上不是一个名为 `webchat` 的独立 channel，而是通过 `pico` channel 完成接入。

因此，如果要做一个新的 WebChat 对接当前系统，**优先方案不是新增一个后端 channel，而是直接复用 `pico` 协议**。

---

## 源码结论

### 1. WebSocket 网页聊天入口就是 `/pico/ws`

源码位置：`pkg/channels/pico/pico.go`

关键逻辑：

- `WebhookPath()` 返回 `/pico/`
- `ServeHTTP()` 处理 `/pico/ws`
- `handleWebSocket()` 完成鉴权、升级连接、绑定 `session_id`

也就是说，前端网页聊天直接连 `/pico/ws` 即可，不需要额外 HTTP webhook。

### 2. `pico` 已经注册为标准 channel

源码位置：`pkg/channels/pico/init.go`

`pico` 通过 `channels.RegisterFactory("pico", ...)` 注册到系统，因此它本身就是当前框架认可的正式通道。

### 3. `pico` 已经实现了 WebChat 需要的核心能力

源码位置：`pkg/channels/pico/pico.go`

已具备：

- WebSocket 连接管理
- token 鉴权
- `session_id` 会话隔离
- 收消息：客户端发 `message.send`
- 发消息：服务端回 `message.create`
- 消息更新：`message.update`
- 打字态：`typing.start` / `typing.stop`
- 心跳：`ping` / `pong`
- 占位消息：placeholder

这套能力本身就是一个完整的 WebChat 协议通道。

### 4. 网关也会自动生成 `/pico/ws`

源码位置：`web/backend/api/gateway_host.go`

`buildWsURL()` 会拼出：

```text
ws://<host>:<port>/pico/ws
```

这进一步说明，系统层面已经把 `pico` 当成网页聊天入口。

---

## 推荐方案

## 方案 A：直接复用现有 `pico` channel

这是最小改动、最符合当前仓库结构的方案。

适合场景：

- 你只是要新增一个网页聊天前端
- 后端仍然接当前 picoclaw 系统
- 不需要设计一套完全不同的私有协议

### 接入方式

前端直接建立 WebSocket 连接：

```text
ws://<gateway-host>:<gateway-port>/pico/ws?token=<token>&session_id=<session_id>
```

其中：

- `token`：后端 `pico` channel 配置的鉴权 token
- `session_id`：前端为当前会话生成并持久化的会话 ID

### 推荐前端约定

#### 1. `session_id` 的使用建议

同一个浏览器会话内保持固定 `session_id`，这样后端就会把这一轮聊天上下文识别为同一个聊天会话：

- 首次进入页面生成一个 UUID
- 存到 localStorage 或业务侧会话存储
- 后续重连继续复用该值

#### 2. token 的使用建议

开发环境可以使用 query 参数：

```text
?token=xxx
```

如果前端环境允许，也可以改成 `Authorization: Bearer <token>`，源码里两种都支持；但 query 参数只有在 `allow_token_query=true` 时才允许。

---

## `pico` 协议说明

源码位置：`pkg/channels/pico/protocol.go`

### 客户端 -> 服务端

#### 发送文本消息

```json
{
  "type": "message.send",
  "id": "msg-001",
  "session_id": "session-001",
  "payload": {
    "content": "你好"
  }
}
```

#### 发送心跳

```json
{
  "type": "ping",
  "id": "ping-001"
}
```

### 服务端 -> 客户端

#### 新消息

```json
{
  "type": "message.create",
  "session_id": "session-001",
  "timestamp": 1710000000000,
  "payload": {
    "content": "你好，我是助手"
  }
}
```

#### 更新消息

```json
{
  "type": "message.update",
  "session_id": "session-001",
  "timestamp": 1710000000000,
  "payload": {
    "message_id": "placeholder-001",
    "content": "更新后的内容"
  }
}
```

#### 打字状态

```json
{
  "type": "typing.start",
  "session_id": "session-001"
}
```

```json
{
  "type": "typing.stop",
  "session_id": "session-001"
}
```

#### 心跳响应

```json
{
  "type": "pong",
  "id": "ping-001"
}
```

#### 错误响应

```json
{
  "type": "error",
  "timestamp": 1710000000000,
  "payload": {
    "code": "empty_content",
    "message": "message content is empty"
  }
}
```

---

## 后端配置示例

`pico` 的配置结构位于：`pkg/config/config.go`

对应字段：

- `enabled`
- `token`
- `allow_token_query`
- `allow_origins`
- `ping_interval`
- `read_timeout`
- `max_connections`
- `placeholder`

示例：

```yaml
channels:
  pico:
    enabled: true
    token: "your-secret-token"
    allow_token_query: true
    allow_origins:
      - "*"
    ping_interval: 30
    read_timeout: 60
    max_connections: 100
    placeholder:
      enabled: true
      text: "思考中..."
```

### 字段说明

#### `allow_origins`

用于 WebSocket 的 Origin 校验。

- 如果为空，当前实现默认全部放行
- 生产环境建议明确配置前端域名

#### `allow_token_query`

控制是否允许通过 URL query 传 token。

- 开发联调可以开
- 生产环境更建议走 Header 鉴权

#### `placeholder`

启用后，系统可以先推一个占位消息，后续再通过 `message.update` 更新成最终内容。

这对网页聊天的“正在思考中”体验很有帮助。

---

## 前端最小实现示例

```html
<script>
class PicoWebChat {
  constructor({ url, token, sessionId }) {
    this.url = `${url}?token=${encodeURIComponent(token)}&session_id=${encodeURIComponent(sessionId)}`
    this.sessionId = sessionId
    this.ws = null
  }

  connect() {
    this.ws = new WebSocket(this.url)

    this.ws.onopen = () => {
      console.log('ws connected')
    }

    this.ws.onmessage = (event) => {
      const msg = JSON.parse(event.data)
      this.handleMessage(msg)
    }

    this.ws.onclose = () => {
      console.log('ws closed')
    }

    this.ws.onerror = (err) => {
      console.error('ws error', err)
    }
  }

  sendText(content) {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) return

    this.ws.send(JSON.stringify({
      type: 'message.send',
      id: crypto.randomUUID(),
      session_id: this.sessionId,
      payload: { content }
    }))
  }

  ping() {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) return

    this.ws.send(JSON.stringify({
      type: 'ping',
      id: crypto.randomUUID()
    }))
  }

  handleMessage(msg) {
    switch (msg.type) {
      case 'message.create':
        console.log('new message:', msg.payload?.content)
        break
      case 'message.update':
        console.log('update message:', msg.payload?.content)
        break
      case 'typing.start':
        console.log('assistant typing...')
        break
      case 'typing.stop':
        console.log('assistant stop typing')
        break
      case 'pong':
        console.log('pong')
        break
      case 'error':
        console.error('server error:', msg.payload)
        break
    }
  }
}

const sessionId = localStorage.getItem('pico-session-id') || crypto.randomUUID()
localStorage.setItem('pico-session-id', sessionId)

const chat = new PicoWebChat({
  url: 'ws://192.168.10.21:18790/pico/ws',
  token: '00ef90f7b09202706a20f470a4bb13bc',
  sessionId
})

chat.connect()
</script>
```

---

## 如果一定要做独立的 `webchat` channel，该怎么做

只有在你满足下面条件时，才建议新建一个独立 channel：

- 你不想复用 `pico` 协议
- 你需要和现有 Web 前端协议完全不同
- 你希望后端把网页聊天与 pico 客户端完全隔离

这时可以参考 `pkg/channels/pico/` 新建：

```text
pkg/channels/webchat/
  init.go
  webchat.go
  protocol.go
```

同时需要改动：

### 1. 在 `pkg/config/config.go` 新增配置结构

例如：

```go
type WebChatConfig struct {
    Enabled bool   `json:"enabled"`
    Token   string `json:"token"`
}
```

并挂到：

```go
type ChannelsConfig struct {
    ...
    WebChat WebChatConfig `json:"webchat"`
}
```

### 2. 在 `pkg/channels/webchat/init.go` 注册 factory

```go
func init() {
    channels.RegisterFactory("webchat", func(cfg *config.Config, b *bus.MessageBus) (channels.Channel, error) {
        return NewWebChatChannel(cfg.Channels.WebChat, b)
    })
}
```

### 3. 在 `webchat.go` 中实现 channel

最少需要参考 `pkg/channels/pico/pico.go` 实现这些能力：

- `Start(ctx)`
- `Stop(ctx)`
- `Send(ctx, msg)`
- 如果要挂到共享 HTTP server，还需要实现 `WebhookPath()` 和 `ServeHTTP()`
- 如果要支持编辑消息、打字态、占位消息，还要实现对应 capability interface

### 4. 前端改连新的路径

例如：

```text
/ws/webchat
```

但这条路本质上是在重复实现一遍当前 `pico` 已经有的能力。

---

## 推荐落地方式

如果你的目标只是“做一个新的网页聊天 UI，对接当前系统”，建议直接这样做：

1. 后端启用 `channels.pico`
2. 为网页前端分配 token
3. 前端以 WebSocket 连接 `/pico/ws`
4. 每个网页会话持久化一个 `session_id`
5. 按 `message.send` / `message.create` 协议收发消息
6. 根据 `typing.start/stop`、`message.update` 做交互增强

这样可以做到：

- **后端零或极少改动**
- **直接复用现有消息总线和 agent 处理链路**
- **兼容当前系统架构**
- **风险最低**

---

## 一句话结论

当前系统里的 WebChat 实际就是 `pico` channel 提供的 WebSocket 能力；如果你要新增一个网页聊天前端，最合理的做法是**直接复用 `/pico/ws` 和 `pico` 协议**，而不是再新造一个独立 `webchat` channel。