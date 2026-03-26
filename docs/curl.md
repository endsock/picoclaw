# Spawn API curl 示例

## 最小参数版本

```bash
curl -X POST "http://127.0.0.1:18790/api/spawn" \
  -H "Content-Type: application/json" \
  -d '{
    "task": "上海明天天气怎么样",
    "label": "",
    "agent_id": "worker",
    "model_name": "claw-haiku",
    "metadata": {
      "rz_task_id": "slow-002"
    },
    "webhook": {
      "url": "http://127.0.0.1:9000/callback",
      "events": ["completed", "failed", "canceled"],
      "timeout_ms": 5000,
      "max_retries": 2
    },
    "channel":"feishu",
    "chat_id":"oc_24b1ff403b63776d37d3c7e787d3d512"
  }'
```

## 全量参数版本

```bash
curl -X POST "http://127.0.0.1:8080/api/spawn" \
  -H "Content-Type: application/json" \
  -d '{
    "task": "请分析当前仓库的spawn实现，并输出完整执行摘要",
    "label": "manual-full-test",
    "agent_id": "default",
    "model_name": "claude-sonnet-4-6",
    "metadata": {
      "biz_id": "test-001",
      "trace_id": "trace-abc-123",
      "operator": "manual"
    },
    "webhook": {
      "url": "http://127.0.0.1:9000/callback",
      "headers": {
        "X-Test": "1",
        "X-Request-ID": "req-123456",
        "Authorization": "Bearer your-token"
      },
      "events": ["completed", "failed", "canceled"],
      "timeout_ms": 5000,
      "max_retries": 2
    }
  }'
```

## 不带 webhook.events 的版本

`webhook.events` 可省略；省略时默认会订阅：

- `completed`
- `failed`
- `canceled`

```bash
curl -X POST "http://127.0.0.1:8080/api/spawn" \
  -H "Content-Type: application/json" \
  -d '{
    "task": "请读取当前项目的spawn相关实现，并给出一句话总结",
    "label": "spawn-api-default-events",
    "agent_id": "default",
    "model_name": "claude-sonnet-4-6",
    "metadata": {
      "biz_id": "local-test",
      "trace_id": "trace-local-001",
      "operator": "kimsky"
    },
    "webhook": {
      "url": "http://127.0.0.1:9000/callback",
      "headers": {
        "X-Test": "1"
      },
      "timeout_ms": 5000,
      "max_retries": 2
    }
  }'
```

## 查询任务结果

创建成功后，返回体里会包含 `task_id` 和 `result_url`。

```bash
curl "http://127.0.0.1:8080/api/spawn/<task_id>"
```

## 预期返回示例

### 创建成功

```json
{
  "task_id": "subagent-1",
  "status": "accepted",
  "message": "Spawned subagent 'manual-full-test' for task: 请分析当前仓库的spawn实现，并输出完整执行摘要",
  "webhook_registered": true,
  "result_url": "/api/spawn/subagent-1"
}
```

### 查询结果

```json
{
  "task_id": "subagent-1",
  "status": "completed",
  "label": "manual-full-test",
  "agent_id": "default",
  "model_name": "claude-sonnet-4-6",
  "source": "api",
  "metadata": {
    "biz_id": "test-001",
    "trace_id": "trace-abc-123",
    "operator": "manual"
  },
  "created_at_ms": 1710000000000,
  "finished_at_ms": 1710000001234,
  "duration_ms": 1234,
  "result": "..."
}
```

## 手工模拟 webhook 回调

subagent 完成后会向你配置的 `webhook.url` 发送 POST 请求。

下面的 curl 模拟了 picoclaw 发出的**完整 webhook 回调请求**，你可以用它来测试你的回调接收端。

### completed（成功完成）

```bash
curl -X POST "http://127.0.0.1:9000/callback" \
  -H "Content-Type: application/json" \
  -H "User-Agent: picoclaw-webhook/1.0" \
  -H "X-PicoClaw-Event: completed" \
  -H "X-PicoClaw-Task-ID: subagent-1" \
  -H "X-Test: 1" \
  -H "X-Request-ID: req-123456" \
  -H "Authorization: Bearer your-token" \
  -d '{
    "event": "completed",
    "status": "completed",
    "task_id": "subagent-1",
    "label": "manual-full-test",
    "agent_id": "default",
    "model_name": "claude-sonnet-4-6",
    "source": "api",
    "metadata": {
      "biz_id": "test-001",
      "trace_id": "trace-abc-123",
      "operator": "manual"
    },
    "result": "分析完成：spawn 通过 SubagentManager 统一调度，支持 tool spawn 和 API spawn 两种入口。",
    "error": "",
    "created_at_ms": 1710000000000,
    "finished_at_ms": 1710000005234,
    "duration_ms": 5234,
    "result_size": 85
  }'
```

### failed（执行失败）

```bash
curl -X POST "http://127.0.0.1:9000/callback" \
  -H "Content-Type: application/json" \
  -H "User-Agent: picoclaw-webhook/1.0" \
  -H "X-PicoClaw-Event: failed" \
  -H "X-PicoClaw-Task-ID: subagent-2" \
  -H "X-Test: 1" \
  -H "X-Request-ID: req-123456" \
  -H "Authorization: Bearer your-token" \
  -d '{
    "event": "failed",
    "status": "failed",
    "task_id": "subagent-2",
    "label": "manual-full-test",
    "agent_id": "default",
    "model_name": "claude-sonnet-4-6",
    "source": "api",
    "metadata": {
      "biz_id": "test-001",
      "trace_id": "trace-abc-123",
      "operator": "manual"
    },
    "result": "",
    "error": "Error: LLM call failed: connection timeout",
    "created_at_ms": 1710000000000,
    "finished_at_ms": 1710000003000,
    "duration_ms": 3000,
    "result_size": 0
  }'
```

### canceled（任务取消）

```bash
curl -X POST "http://127.0.0.1:9000/callback" \
  -H "Content-Type: application/json" \
  -H "User-Agent: picoclaw-webhook/1.0" \
  -H "X-PicoClaw-Event: canceled" \
  -H "X-PicoClaw-Task-ID: subagent-3" \
  -H "X-Test: 1" \
  -H "X-Request-ID: req-123456" \
  -H "Authorization: Bearer your-token" \
  -d '{
    "event": "canceled",
    "status": "canceled",
    "task_id": "subagent-3",
    "label": "manual-full-test",
    "agent_id": "default",
    "model_name": "claude-sonnet-4-6",
    "source": "api",
    "metadata": {
      "biz_id": "test-001",
      "trace_id": "trace-abc-123",
      "operator": "manual"
    },
    "result": "",
    "error": "Task canceled during execution",
    "created_at_ms": 1710000000000,
    "finished_at_ms": 1710000001500,
    "duration_ms": 1500,
    "result_size": 0
  }'
```

### result 超长时的截断场景

当 `result` 超过 `max_payload_bytes` 限制时，会被截断并附加标记：

```bash
curl -X POST "http://127.0.0.1:9000/callback" \
  -H "Content-Type: application/json" \
  -H "User-Agent: picoclaw-webhook/1.0" \
  -H "X-PicoClaw-Event: completed" \
  -H "X-PicoClaw-Task-ID: subagent-4" \
  -H "X-Test: 1" \
  -d '{
    "event": "completed",
    "status": "completed",
    "task_id": "subagent-4",
    "label": "large-result-test",
    "agent_id": "default",
    "model_name": "claude-sonnet-4-6",
    "source": "api",
    "metadata": {},
    "result": "这是一段被截断的很长的结果...",
    "error": "",
    "created_at_ms": 1710000000000,
    "finished_at_ms": 1710000010000,
    "duration_ms": 10000,
    "result_truncated": true,
    "result_size": 131072
  }'
```

### webhook 请求头说明

picoclaw 发出的 webhook 请求会带以下固定 header：

| Header | 说明 |
|--------|------|
| `Content-Type` | 固定 `application/json` |
| `User-Agent` | 固定 `picoclaw-webhook/1.0` |
| `X-PicoClaw-Event` | 事件类型：`completed` / `failed` / `canceled` |
| `X-PicoClaw-Task-ID` | 任务 ID |

加上你在 `webhook.headers` 里自定义的 header。

### webhook payload 字段说明

| 字段 | 类型 | 说明 |
|------|------|------|
| `event` | string | 事件类型：`completed` / `failed` / `canceled` |
| `status` | string | 与 event 相同 |
| `task_id` | string | 任务 ID |
| `label` | string | 任务标签（可为空） |
| `agent_id` | string | 目标 agent ID（可为空） |
| `model_name` | string | 使用的模型名 |
| `source` | string | 来源：`api` 或 `tool` |
| `metadata` | object | 创建时传入的 metadata，原样透传 |
| `result` | string | 执行结果（失败/取消时为空） |
| `error` | string | 错误信息（成功时为空） |
| `created_at_ms` | int64 | 任务创建时间（毫秒时间戳） |
| `finished_at_ms` | int64 | 任务完成时间（毫秒时间戳） |
| `duration_ms` | int64 | 执行耗时（毫秒） |
| `result_truncated` | bool | result 是否被截断（仅超长时出现） |
| `result_size` | int | result 原始字节数 |

---

## 说明

- `task`、`model_name` 是必填字段
- `agent_id` 可选，不传时使用默认 agent
- `metadata` 会透传到查询结果和 webhook payload
- Spawn API 创建的 subagent **不会发送 system 消息**
- 手工测试主要看：
  1. `POST /api/spawn` 是否返回 `202 Accepted`
  2. `GET /api/spawn/{task_id}` 的状态变化
  3. webhook 是否收到回调
