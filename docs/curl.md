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

```返回值
{
  "event": "completed",
  "status": "completed",
  "task_id": "subagent-80802404",
  "agent_id": "worker",
  "model_name": "claw-haiku",
  "source": "api",
  "metadata": {
    "rz_task_id": "slow-002"
  },
  "result": "已完成分析。先说明一个关键信息：\n\n- 任务环境里写的是 `dbname: pms_core_211`\n- 但你给的 SQL 是：`select * from pms_core_214.ngt_process_detail`\n\n这条 SQL 已经显式指定了库名 `pms_core_214`，所以我按 **`pms_core_214.ngt_process_detail`** 实际分析了。\n\n---\n\n## SQL优化分析报告\n\n### 原始SQ
L\n```sql\nselect * from pms_core_214.ngt_process_detail\n```\n\n---\n\n### 一、结论\n\n这条 SQL 慢的根因非常直接：\n\n1. **没有 WHERE 条件**\n2. **没有 LIMIT**\n3. **用了 `select *`**\n4. 目标表大约 **72.8 万行**\n5. 执行计划是 **全表扫描（type=ALL）**\n\n也就是说，这不是“索引没建好导致慢”，而是这条 SQL 的
义本身就是：\n\n> **把整张表所有列、所有行全部读出来**\n\n对于这种 SQL，**全表扫描是正常且不可避免的执行方式**。  \n如果业务确实要拉全表，那它慢是符合预期的，优化空间很有限。\n\n---\n\n### 二、表结构摘要\n\n表：`pms_core_214.ngt_process_detail`\n\n主键：\n```sql\nPRIMARY KEY (id)\n```\n\n目前只有一个索引   
：\n\n- `PRIMARY(id)`\n\n表大致情况：\n\n- 估算行数：`728823`\n- 数据大小：约 `100MB`\n- `INDEX_LENGTH = 0`，说明除了主键外基本没有其他有效二级索引\n\n---\n\n### 三、执行计划分析\n\n执行计划结果：\n\n```sql\nEXPLAIN SELECT * FROM pms_core_214.ngt_process_detail;\n```\n\n核心结果：\n\n- `type = ALL`\n- `key 
= NULL`\n- `rows = 728823`\n\n说明：\n\n- MySQL 选择 **扫描整表**\n- 没有可用索引\n- 但即使加索引，对这条 SQL 也**没有本质帮助**\n\n因为你是：\n\n```sql\nselect *\nfrom ngt_process_detail\n```\n\n没有过滤条件、没有排序条件、没有关联条件。  \n这种情况下，数据库必须把整张表都读出来，索引反而通常不会比直接扫表
优。\n\n---\n\n### 四、性能问题分析\n\n#### 1. `SELECT *`\n会把所有字段都返回，包括这个大字段：\n\n```sql\nformat_parameter varchar(2048) NOT NULL\n```\n\n这会明显增加：\n\n- 磁盘读取量\n- 网络传输量\n- 客户端处理开销\n\n#### 2. 无过滤条件\n没有 `WHERE`，无法缩小扫描范围。\n\n#### 3. 无分页\n没有 `LIMIT`   
，一次性返回全部数据，容易导致：\n\n- SQL 执行时间长\n- 结果集过大\n- 应用内存占用高\n- 网络传输慢\n\n#### 4. 表本身不算小\n72 万多行、约 100MB，对于“全表全字段读取”来说，慢是很正常的。\n\n---\n\n## 五、是否需要加索引？\n\n### 结论：**针对这条 SQL，本身不建议单纯为了它去加索引**\n\n原因：\n\n```sql\nselect 
* from ngt_process_detail\n```\n\n这种 SQL 没有筛选条件，索引无法帮助“少读数据”。\n\n只有在你后续改成类似下面这种查询时，索引才有意义：\n\n```sql\nwhere process_id = ?\nwhere ngt_cd = ?\nwhere business_dt = ?\nwhere htl_cd = ? and business_dt = ?\norder by id desc limit 100\n```\n\n---\n\n## 六、优化建议\n\
n### 1. 最优先：不要查全表全字段\n如果只是排查数据、列表展示、任务巡检，建议只查需要的列。\n\n例如：\n\n```sql\nselect id, ngt_cd, htl_cd, process_id, business_dt, log_type, business_log_code, seq, _timestamp\nfrom pms_core_214.ngt_process_detail;\n```\n\n这样可以避免把 `format_parameter` 这种大字段全部带出
\n\n---\n\n### 2. 加 WHERE 条件\n如果业务并不是真的要全量导出，应该按常见查询维度过滤。\n\n比如按业务日期：\n\n```sql\nselect id, ngt_cd, htl_cd, process_id, business_dt, log_type, business_log_code, seq, _timestamp\nfrom pms_core_214.ngt_process_detail\nwhere business_dt = '2026-04-01';\n```\n\n如果经常   
这样查，可以考虑索引：\n\n```sql\ncreate index idx_business_dt on ngt_process_detail(business_dt);\n```\n\n---\n\n### 3. 加分页\n如果是页面查询或批量处理，必须分页。\n\n例如：\n\n```sql\nselect id, ngt_cd, htl_cd, process_id, business_dt, log_type, business_log_code, seq, _timestamp\nfrom pms_core_214.ngt_p
rocess_detail\norder by id\nlimit 1000;\n```\n\n继续翻页可用：\n\n```sql\nselect id, ngt_cd, htl_cd, process_id, business_dt, log_type, business_log_code, seq, _timestamp\nfrom pms_core_214.ngt_process_detail\nwhere id > ?\norder by id\nlimit 1000;\n```\n\n这种方式比 `limit offset` 更稳定。\n\n---\n\n### 4.
 如果是“全量导出”，接受它慢，改为分批导出\n如果业务场景就是：\n\n- 全表同步\n- 全量导出\n- 数据校验\n\n那这条 SQL 本质上就不可能很快。建议改成**按主键分批拉取**：\n\n```sql\nselect *\nfrom pms_core_214.ngt_process_detail\nwhere id > ? and id <= ?;\n```\n\n或者：\n\n```sql\nselect *\nfrom pms_core_214.ngt_pr
ocess_detail\nwhere id > ?\norder by id\nlimit 5000;\n```\n\n这样可以降低单次事务、网络和客户端压力。\n\n---\n\n### 5. 若常用查询条件明确，再补充索引\n根据字段语义，这张表未来可能常见的过滤条件有：\n\n- `process_id`\n- `business_dt`\n- `ngt_cd`\n- `htl_cd`\n- `log_type`\n- `business_log_code`\n\n如果你能提 
供真实慢 SQL，比如：\n\n```sql\nselect * from ngt_process_detail where process_id = 123;\n```\n\n或\n\n```sql\nselect * from ngt_process_detail where business_dt='2026-04-01' and htl_cd='xxx';\n```\n\n我可以继续给你精确判断该建什么索引、索引顺序怎么排。\n\n---\n\n## 七、可落地优化方案\n\n### 场景A：只是查看
近数据\n```sql\nselect id, ngt_cd, htl_cd, process_id, business_dt, log_type, business_log_code, seq, _timestamp\nfrom pms_core_214.ngt_process_detail\norder by id desc\nlimit 200;\n```\n\n可加索引（如果需要按倒序高频查）：\n- 主键已有，一般足够\n\n---\n\n### 场景B：按日期查\n```sql\nselect id, ngt_cd, ht t
l_cd, process_id, business_dt, log_type, business_log_code, seq, _timestamp\nfrom pms_core_214.ngt_process_detail\nwhere business_dt = '2026-04-01';\n```\n\n建议索引：\n```sql\ncreate index idx_business_dt on ngt_process_detail(business_dt);\n```\n\n---\n\n### 场景C：按流程ID查\n```sql\nselect *\nfrom pms_c
ore_214.ngt_process_detail\nwhere process_id = 123456;\n```\n\n建议索引：\n```sql\ncreate index idx_process_id on ngt_process_detail(process_id);\n```\n\n---\n\n### 场景D：按酒店+日期查\n```sql\nselect *\nfrom pms_core_214.ngt_process_detail\nwhere htl_cd = 'H001' and business_dt = '2026-04-01';\n```\n\n建 
议复合索引：\n```sql\ncreate index idx_htl_business_dt on ngt_process_detail(htl_cd, business_dt);\n```\n\n---\n\n## 八、预期收益\n\n如果仍然执行：\n\n```sql\nselect * from pms_core_214.ngt_process_detail\n```\n\n那优化收益非常有限，因为它天然就是全表读。\n\n如果改为：\n\n- 指定列\n- 增加过滤条件\n- 分页\n-
 分批读取\n\n通常可以把性能提升到 **一个数量级以上**，尤其是页面查询和接口查询场景。\n\n---\n\n## 九、最终判断\n\n### 这条 SQL 是否“异常慢”？\n**不一定异常。**  \n它慢是因为它在做一件本来就重的事：**读取整张表所有数据**。\n\n### 是否存在索引缺失问题？\n**针对当前这条 SQL，不是核心问题。**\n\n### 最佳优化方 
向\n不是“给这条 SQL 加索引”，而是：\n\n1. 不要 `select *`\n2. 不要无条件全表查\n3. 增加分页\n4. 改成按条件、按批次读取\n\n---\n\n如果你愿意，我下一步可以继续帮你做两件事中的任意一个：\n\n1. **根据这张表结构，直接给出一套“最可能需要”的索引方案**\n2. **如果你有真实业务 SQL（带 WHERE 条件的那种），我可以继续做确慢 SQL 优化**",
  "created_at_ms": 1775024533830,
  "finished_at_ms": 1775024749953,
  "duration_ms": 216123,
  "result_size": 7416
}
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
