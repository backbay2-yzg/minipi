# minipi

用 Go 从零实现的迷你 Agent 运行时（教学项目，day1 → day9 递进）。

每层递进一个能力，且**每一层都复用上一层的机制、不引入"魔法"**：

| 阶段 | 能力 | 关键机制 |
| --- | --- | --- |
| day1 | Provider 抽象 + 单轮问答 CLI | `llm.Model` 接口、`llm.New` 工厂 |
| day2 | Agent 循环（工具调用 / 结果回喂） | `Run` 循环、哨兵错误 `ErrMaxSteps`、Mock 离线测试 |
| day3 | 工具注册与执行绑定 | `Tool` 接口（`Spec` + `Run`）、`Registry`（保序） |
| day4 | Context 管道 | `TransformFunc` 四契约、`Hidden` 过滤、破坏性 `Compact` |
| day5 | 事件总线 | `EventBus` fan-out、流式 delta、`AgentID` 区分来源 |
| day6 | 会话持久化 | `Store` 接口、JSONStore / SQLiteStore、恢复语义 |
| day7 | steering 插话 + reasoning 展示 | `steerCh` + `drainSteer` 两个注入点 |
| day8 | 组合层 | `RunChain`（Stage / Gate / Retry）、`SubAgentTool`、`HumanGate` |
| day9 | 路由 | `Route` / `RunRoute` / `RunRouteWithFallback`（分类 → 派发，含兜底） |

一句话概括设计取向：**机制尽量少，组合尽量深**——路由派发只是一个 `switch`，一侧是单次 `Run`，另一侧是整条 `RunChain`，而链内环节又复用了同一套门机制。

---

## 快速开始

```bash
# 环境要求：Go 1.27+（go.mod 声明 go 1.27）；无 cgo 需求
go build ./...

# 默认 provider 为 opencode，需对应 key
set MINIPI_PROVIDER=opencode        # PowerShell: $env:MINIPI_PROVIDER="opencode"
set OPENCODE_API_KEY=sk-...         # bash: export OPENCODE_API_KEY=sk-...
go run ./cmd/minipi
```

### 环境变量

| 变量 | 作用 | 缺省 |
| --- | --- | --- |
| `MINIPI_PROVIDER` | `openai` / `anthropic` / `opencode` | `opencode` |
| `MINIPI_MODEL` | 模型名 | `gpt-4o-mini` / `claude-sonnet-4-5` / `glm-5.2` |
| `MINIPI_BASE_URL` | 兼容端点（代理 / 自建网关） | 各 provider 官方地址 |
| `MINIPI_SESSION_ID` | **设置即恢复该会话**；不设置则新建 | 空 |
| `MINIPI_SESSION_DB` | SQLite 会话库路径 | `minipi_sessions.db` |
| `OPENAI_API_KEY` / `ANTHROPIC_API_KEY` / `OPENCODE_API_KEY` | 鉴权（按 provider 取用） | 空 |

> ⚠️ API key 只从环境变量读取，**永不**写入代码、日志或会话文件。

未知的 `MINIPI_PROVIDER` 会直接报错退出，不会静默回落到某个默认值。

---

## CLI 交互

| 输入 | 行为 |
| --- | --- |
| 普通文本 | 发起一轮对话（`agent.Run` 在独立 goroutine 中跑，主循环继续接收输入） |
| **运行中任意文本** | steer：插话排队，在下一轮调模型前注入历史 |
| `/new` | 清空本地历史与 usage，并切换 Provider 侧会话（`llm.Sessioned`） |
| `/info` | 当前会话 ID / 消息数 / 轮数 / 累计 token |
| `/list` | 列出已保存会话（ID、消息数、首条 user 消息前 40 rune） |
| `/chain <主题>` | 演示链式工作流：提纲 →（编号要点门）→ 正文 |
| `/translate <英文>` | 演示翻译链：翻译 → 长度门 → 审校 → 残留门 → 润色（人工终门把关） |
| `/route <问题>` | 演示路由：分类（含置信度）→ 按分支执行（简单分支 / 链分支 / 兜底） |
| `/quit`、`/exit` | 退出 |

运行中以 `/` 开头的命令会被拒绝并提示等待本轮完成（命令语义与 steer 互斥）。

### 一轮对话里发生了什么

```text
用户输入 ──> agent.Run ──> turn_start ──> drainSteer（注入点 1）
                              │
                              ├─ buildView：Transforms 裁剪 → 过滤 Hidden（只产视图）
                              ├─ 调模型（流式 delta / reasoning delta 实时出事件）
                              ├─ assistant 消息入历史
                              ├─ 有 ToolCalls？── 否 ──> turn_end ──> 落盘 ──> 压缩检查
                              │             └─ 是 ──> 并行执行工具 ──> toolResult 入历史
                              └─ drainSteer（注入点 2：绝不拆散 toolCall/toolResult 配对）
```

每轮成功后写入 SQLite，并在轮数超过阈值（8 轮）时触发一次摘要压缩（保留最近 4 轮）。
单轮失败时，本地历史回滚到本轮起点——**本轮已注入的 steer 也一并作废**。

---

## 目录结构

```text
minipi/
├── cmd/minipi/
│   ├── main.go            # ★ 组合根：env → Model / Store / EventBus / 工具集 → REPL 主循环
│   ├── events.go          # 事件渲染：按 AgentID / Step / kind 分块打印
│   └── day1/main-day1.go  # day1 历史快照（独立 main 包，仅作教学对照）
└── internal/
    ├── llm/               # Provider 抽象与适配（openai / anthropic / opencode / mock）
    ├── agent/             # 核心：Run 循环、状态、Context 管道、事件总线、工具、子 Agent
    ├── session/           # 会话持久化（Store 接口 + JSONStore + SQLiteStore）
    └── workflow/          # 组合层：链式工作流、人在环门、翻译链、路由
```

依赖方向（严禁反向）：

```text
cmd/minipi（唯一知道「具体工具 / 具体 provider / 具体存储」的地方）
   ├──> internal/workflow ──> internal/agent ──> internal/llm
   └──> internal/session  ──> internal/agent ──> internal/llm
```

- `internal/agent` 只认识 `Tool` 接口与 `llm.Model` 抽象，不认识任何具体实现，因此可以纯 Mock 离线测试。
- 新增具体实现（工具 / provider / Store）时，实例化代码一律写在组合根，不要写进 `internal/agent`。

---

## 能力一览

### Provider 抽象（`internal/llm`）

接口最小化 + 可选能力探测，不给 `Model` 加方法，避免所有适配器和 Mock 跟着改：

| 接口 | 用途 |
| --- | --- |
| `Model` | 唯一必需能力：`Complete` |
| `StreamingModel` | 流式增量；契约：拼接 deltas == 返回的 `Text` |
| `ReasoningStreamingModel` | 思考流，仅展示、不入历史 |
| `Sessioned` | Provider 侧会话切换（`/new`） |
| `SessionForker` | 为并发的子 Agent 派生独立会话 |

已实现：OpenAI、Anthropic（仅非流式，Agent 自动回落）、OpenCode Zen Go（流式 + reasoning + 会话），以及测试用的 `MockModel` / `MockStreaming`。

### Agent 运行时（`internal/agent`）

- **循环**：`Run` = 生命周期壳（`agent_start` / `agent_end` 必各发一次）+ `run` = 核。默认最多 8 步，耗尽返回 `ErrMaxSteps` 并补一条 UI-only 注释。
- **Context 管道**：`PruneByTurns` / `PruneByTokens` / `TruncateToolResults`，始终以「轮」为单位操作，只裁视图不改历史。
- **压缩**：`Compact` 把老轮次换成一条摘要（成本计入 usage），不可逆，所以刻意**不是** `TransformFunc`。
- **工具**：`Registry` 保注册顺序；同轮多个调用用 `errgroup` 并行执行、串行写回；任何失败（error / panic / 取消 / 未知工具）都折叠成结果文本回喂，保证每条 `toolCall` 恰好一条 `toolResult`。
- **steering**：`Steer` 可从任意 goroutine 调用（队列满则阻塞背压，不丢消息）；CLI 主循环承担「运行中输入即插话」。
- **子 Agent**：`SubAgentTool` 把一整段子循环包装成普通工具 `delegate_task`，`AgentID` 为 `sub-N`。

事件序列契约：

```text
agent_start
  turn_start → message_start → message_update* (→ reasoning_update*) → message_end
             → [tool_execution_start → tool_execution_end]×M
             → turn_end
  …（多轮）
agent_end                     # 终态事件；丢失 = 订阅者 goroutine 泄漏
# 旁路（advisory）：compaction_start / compaction_end
```

### 持久化（`internal/session`）

- `Session{ID, CreatedAt, UpdatedAt, Config, State}`。
- `Config` 只存「重建行为所需的名字」（provider / model / baseURL / system / tool 名），**不存行为、永不存 key**；模型与 `Transforms` 由当前代码决定，所以升级模型或调整裁剪策略对老会话无缝生效。
- `JSONStore`：每会话一个文件，写 `.tmp` 再 `rename` 原子落盘，`Load` 防路径穿越，`List` 跳过损坏文件。
- `SQLiteStore`：单表 + 整体 JSON 正文，`INSERT ... ON CONFLICT DO UPDATE`，`modernc.org/sqlite` 驱动（无 cgo）。CLI 默认使用它。

恢复会话：

```bash
MINIPI_SESSION_ID=20260908-215058-6ceea8f4 go run ./cmd/minipi
```

会话由不同版本代码创建（出现未知工具名）时会显式报错，而不是静默跳过。

### 组合层（`internal/workflow`）

| 构件 | 说明 |
| --- | --- |
| `RunChain` | 严格串行，上一环节输出 = 下一环节输入；每次尝试都是全新 `AgentState`（`chain-N`），环节间只传字符串 |
| `Gate` | 环节出口的廉价检查点；返回 error 即拒绝，理由会拼进重试输入（任务 + 被拒产物 + 理由三件套） |
| `HumanGate` | 人在环终门：`y` 通过，`n` 追问一行理由并用它驱动重做 |
| `TranslateChain` | 翻译演示链：长度门管「量」、残留门管「质」、润色环节外挂人工门 |
| `RunRoute` / `RunRouteWithFallback` | 一次轻量分类（无工具、输出结构化 JSON）→ 按命中分支派发原始任务；支持置信度阈值与兜底分支 |

路由的失败分两类：**分类语义失败**（输出不可解析 / 分支未知 / 门重试耗尽）可落兜底；**模型级错误**（网络 / 取消）照常上抛——兜底分支用的是同一个模型，救不了也不该救。路由决策（`Route` / `Reason` / `Confidence`）与分类、执行的全部 token 都随结果返回。

---

## 测试

```bash
go test ./...                                         # 全量离线（Mock + httptest，零 API 费用）
go test -race ./internal/agent/ ./internal/workflow/   # 并发敏感包
go test ./internal/agent/ -run TestRunLoopWithToolCall -v
gofmt -l .                                            # 提交前格式检查
```

测试纪律：

- **全离线**：上层用 `MockModel` / `MockStreaming`，HTTP 层用 `httptest` 假 provider。禁止在测试里发真实请求。
- **goroutine 泄漏哨兵**：`internal/agent`、`internal/workflow` 的 `TestMain` 挂 `goleak.VerifyTestMain`；等终态事件必须带超时兜底，否则「事件丢失」会表现成永久挂起。
- **并发要证据不要 sleep**：用栅栏（`entered` / `release` 通道）证明「确实并发」或「确实隔离」。

---

## 边界与已知取舍

这些是刻意选择，不是 bug：

- 链与路由**同步执行**：运行期间主 goroutine 阻塞，不接受插话（`HumanGate` 除外）。异步化 + steering 贯通是待办。
- `HumanGate` 直接消费 `inputCh`，安全前提正是「同步执行」——任一时刻只有一个读者。
- 单进程单会话写入：`Store` 无跨进程文件锁；写冲突靠「同一会话同一时刻一个 writer」的约定规避。
- SQLite 无迁移机制：只用 `CREATE TABLE IF NOT EXISTS`，字段演进靠 JSON 兼容。
- 无 `/resume` 命令：恢复靠 `MINIPI_SESSION_ID`，`/list` 只列出不切换。
- token 估算是粗估（ASCII ≈ 4 字符/token、CJK ≈ 1 字符/token），且未计入 system 与工具声明。
- 并行工具的**事件相对顺序不保证**（同一工具 start→end 有序）；需要严格顺序的消费者应按 `ToolCall.ID` 归并。
- `cmd/minipi/day1/main-day1.go` 是历史快照，不随主线演进。

---

## 深入阅读

架构细节、11 条必须遵守的设计不变量、扩展指南（新增工具 / Provider / Transform / Gate / 持久化字段）与术语表见 **[AGENTS.md](./AGENTS.md)**。

改代码前请先读 AGENTS.md 第 6 节「必须遵守的不变量」——违反任何一条都会产生「测试可能不报、真实 Provider 上必炸」的问题。
