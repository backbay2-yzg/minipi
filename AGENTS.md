# AGENTS.md — minipi 协作指南

> 本文件是给在本仓库工作的 AI 编码代理（以及人类协作者）看的操作手册。
> 内容基于 `main` 分支当前代码（commit `16992a9`）。**改代码前请先读完第 6 节「必须遵守的不变量」。**

## 1. 项目概览

**minipi** 是用 Go 从零实现的、教学性质的 Agent 运行时（"mini-pi"），配套 CLI 可直接与真实 LLM Provider 对话。仓库按 day1 → day8 递进提交，每层加一个能力：

| 阶段 | 能力 |
| --- | --- |
| day1 | `llm` Provider 抽象 + 单轮问答 CLI |
| day2 | Agent 循环（工具调用 / 结果回喂）、Mock 离线测试 |
| day3 | `Tool` 接口与 `Registry`（声明与执行合一） |
| day4 | Context 管道（Transforms → 过滤）、`Hidden`、`Compact` |
| day5 | 事件总线（流式 delta、生命周期事件） |
| day6 | 会话持久化（JSONStore / SQLiteStore、恢复语义） |
| day7 | steering（运行中插话）、reasoning 展示 |
| day8 | workflow 链（Stage / Gate / Retry）、SubAgent 并行委派、人在环门 |

- **工具链**：Go **1.27**（`go.mod` 的 `go 1.27`），module 名 `minipi`，主要开发环境为 Windows + PowerShell。
- **零魔法**：不引入 Web 框架 / DI / 日志库；只用标准库 + 3 个直接依赖。
- **依赖**：`golang.org/x/sync`（`errgroup` 并行执行工具）、`modernc.org/sqlite`（**无 cgo** 的 SQLite 持久化）、`go.uber.org/goleak`（测试期 goroutine 泄漏哨兵）。

**注释风格（请沿用）**：中文注释解释「为什么这样设计」，关键处用 `★` 标记组装点与易漏点，必要时引用课程「反模式编号」（如 46 / 55 / 56 / 57 / 59 / 61 / 62 / 70–76 / 84）。新增代码请保持同等密度的「决策 + 理由」注释，不要写「翻译代码」式的废话注释。

## 2. 快速命令与环境变量

```bash
go build ./...                                          # 编译全部包
go run ./cmd/minipi                                     # 启动 CLI（需要 API key）
go test ./...                                           # 全量测试（离线，Mock + httptest，零 API 费用）
go test -race ./internal/agent/ ./internal/workflow/     # 并发敏感包建议带 -race
go test ./internal/agent/ -run TestRunLoopWithToolCall -v
gofmt -l .                                              # 提交前格式检查
```

环境变量（缺省值见 `cmd/minipi/main.go`）：

| 变量 | 作用 | 缺省 |
| --- | --- | --- |
| `MINIPI_PROVIDER` | provider 选择：`openai` / `anthropic` / `opencode` | `opencode` |
| `MINIPI_MODEL` | 模型名 | 按 provider：`gpt-4o-mini` / `claude-sonnet-4-5` / `glm-5.2` |
| `MINIPI_BASE_URL` | 兼容端点（代理 / 自建网关） | 各 provider 官方地址 |
| `MINIPI_SESSION_ID` | **设置即恢复该会话**；不设置则新建 | 空 |
| `MINIPI_SESSION_DB` | SQLite 会话库路径 | `minipi_sessions.db` |
| `OPENAI_API_KEY` / `ANTHROPIC_API_KEY` / `OPENCODE_API_KEY` | 鉴权（按 provider 取用） | 空 |

⚠️ **API key 只从环境变量读取，永不写入代码、日志或会话文件**（见不变量 9）。

CLI 交互：回车发送；**运行中任意输入即 steer（插话）**；命令 `/new`（清空本地历史 + 切换 Provider 会话）、`/info`、`/list`、`/chain <主题>`、`/translate <英文原文>`、`/quit`。运行中 `/` 开头的命令会被拒绝并提示等待本轮完成。

## 3. 目录结构与依赖方向

```text
minipi/
├── cmd/minipi/
│   ├── main.go            # ★ 组合根：env → Model / Store / EventBus / 工具集 → REPL 主循环
│   ├── events.go          # 事件渲染：按 AgentID / Step / kind 分块打印到终端
│   └── day1/main-day1.go  # day1 历史快照（独立 package main，请勿改动）
└── internal/
    ├── llm/               # Provider 抽象与适配（openai / anthropic / opencode / mock）
    ├── agent/             # 核心：Run 循环、状态、Context 管道、事件总线、工具、子 Agent
    ├── session/           # 会话持久化（Store 接口 + JSONStore + SQLiteStore）
    └── workflow/          # 组合层：链式工作流（Stage / Gate / Retry）、人在环门、翻译链
```

依赖方向（严禁反向）：

```text
cmd/minipi（组合根：唯一知道「具体工具 / 具体 provider / 具体存储」的地方）
   ├──> internal/workflow ──> internal/agent ──> internal/llm
   └──> internal/session  ──> internal/agent ──> internal/llm
```

- `internal/agent` **不认识任何具体工具**（只认识 `Tool` 接口），也**不认识任何具体 provider**（只认识 `llm.Model`）。实例化一律发生在 `cmd/minipi`：`toolFactories()`、`buildTools()`、`llm.New(...)`。
- `internal/llm` 不 import 任何上层包，只依赖标准库。
- `internal/workflow` 复用 `agent.Run` 与 `agent.FinalText`，不复制循环逻辑。
- 新增具体实现（工具 / provider / Store）时，**不要把实例化代码写进 `internal/agent`** —— 依赖方向一破，`agent` 就无法再用 Mock 离线测试。

> 注：早期 `README.md` 曾描述 `cmd/cli/`、`internal/tool|context|event|orchestrator` 等目录，这些**并不存在**；真实结构以上表为准。

## 4. 核心数据流

### 4.1 一次 `agent.Run`

```text
Run(ctx, st, input)                      # agent.go：壳，保证 agent_start / agent_end 恰好各一次（含错误路径）
└── run(ctx, st, input)                  # 核
    ① st.Messages = append(..., UserMsg(input))
    ② 能力探测（整个循环只探一次）：ReasoningStreamingModel > StreamingModel > Model.Complete
    ③ loop step < maxSteps（默认 DefaultMaxStep = 8）：
       ├── ctx 检查 → turn_start 事件
       ├── drainSteer()                  # ★ 注入点 1：消费插话，成为新轮起点
       ├── buildView(ctx)：Transforms 依次裁剪 → convertToLlm（过滤 Hidden）  # 只产「视图」，不改历史
       ├── message_start → 调模型（流式 delta / reasoning delta 实时出事件）
       ├── TotalUsage 累加 → 追加 AssistantMsg(Text, ToolCalls) → message_end
       ├── 无 ToolCalls → turn_end → return（正常出口）
       ├── executeTools(ctx, st, calls, step)   # errgroup 并行执行，按调用顺序回喂 toolResult
       ├── drainSteer()                  # ★ 注入点 2：工具阶段之后，绝不拆散 toolCall/toolResult 配对
       └── turn_end
    ④ 兜底：追加 Hidden 的 UINote("agent stopped: max steps (N) exceeded") 并返回 ErrMaxSteps
```

关键语义：

- 返回值 `[]Message` **与 `st.Messages` 共享底层数组**：调用方只读，切勿 append / 修改。
- 出错时返回「当时的部分历史」——部分历史也是数据；是否回滚由调用方决定（CLI 在失败轮次执行 `st.Messages = st.Messages[:turnStart]`，连同本轮已注入的 steer 一起作废）。
- 单个工具失败**不中断循环**：错误文本作为 toolResult 回喂，模型自愈（换参 / 换工具 / 放弃）。
- `MaxSteps` 耗尽不算「程序崩溃」：`ErrMaxSteps` 是哨兵错误，调用方用 `errors.Is` 判断。

### 4.2 事件序列契约（`internal/agent/event.go`）

```text
agent_start
  turn_start → message_start → message_update* (→ reasoning_update*) → message_end
             → [tool_execution_start → tool_execution_end]×M
             → turn_end
  …（多轮）
agent_end            # 终态事件：Err == nil 表示正常结束；订阅者据此收尾（丢失 = goroutine 泄漏）
# 旁路（advisory，非生命周期）：compaction_start / compaction_end
```

`cmd/minipi` 用 `agent.NewEventBus(256)` + 单一 `printEvents` 订阅者渲染。`AgentID` 用于区分来源：主 Agent 为 `"main"`，子 Agent 为 `"sub-N"`，链环节为 `"chain-N"`（N 从 1 起）。

### 4.3 会话恢复路径（组合点）

```text
MINIPI_SESSION_ID 存在
  → store.Load(ctx, id)                       # 数据从磁盘来
  → agent.BuildRegistry(sess.Config.ToolNames, toolFactories())   # 行为从代码来
  → reg.Register(agent.NewSubAgentTool(m, baseTools, 4, bus))     # 委托工具在恢复时重新挂载
  → AgentState{ Model: 当前环境的模型, SystemPrompt/Messages/TotalUsage 来自会话, Transforms/Bus 来自代码 }
```

设计决策：**模型与策略（Transforms）不持久化** —— 升级模型/调整裁剪策略无缝生效；`Config` 只记录「重建行为所需的名字」。

## 5. 模块详解

### 5.1 `internal/llm` — Provider 抽象

接口最小化 + 可选能力探测（**不要**给 `Model` 加方法，否则所有适配器与 Mock 都要跟着改）：

| 接口 | 方法 | 用途 |
| --- | --- | --- |
| `Model` | `Complete(ctx, req) (*Completion, error)` | 唯一必需能力 |
| `StreamingModel` | `+ StreamComplete(ctx, req, onDelta)` | 流式；契约：`拼接(deltas) == 返回值.Text` |
| `ReasoningStreamingModel` | `+ StreamCompleteWithReasoning(ctx, req, onDelta, onReasoning)` | 思考流，**仅展示、不入历史** |
| `Sessioned` | `NewSession() string` | Provider 侧会话切换（`/new`） |
| `SessionForker` | `Fork() Model` | 为并发子 Agent 派生独立 Provider 会话 |

统一类型（`types.go`）：`Role`（`user` / `assistant` / `toolResult` / `system`）、`ToolCall{ID,Name,Input}`（`Input` 是 **JSON 字符串**）、`ToolSpec{Name,Description,InputSchema}`、`CompletionRequest{Messages,System,Tools,MaxTokens}`、`Completion{Text,ToolCalls,StopReason,Usage}`、`TokenUsage`、`StopReason`（`end_turn` / `tool_use` / `max_tokens`）、`APIError{Provider,StatusCode,Body}`。

适配器要点（改 wire 格式前务必看对应文件顶部注释）：

- `openai.go` / `openai_stream.go`：默认 `https://api.openai.com/v1`，`Client.Timeout: 120s`；`toolResult → role:"tool"`；SSE 工具调用按 `index` 累积、`sort` 后重组（乱序 = ID 配对错乱）。
- `anthropic.go`：默认 `https://api.anthropic.com`；`tool_use.input` 是 **JSON 对象**（不是字符串）；`tool_result` 走 `anBlock.ToolUseID`。
- `opencode.go` / `opencode_stream.go`：默认 `https://opencode.ai/zen/go/v1`；**必须**带 `x-opencode-session`（缺失 → 400 `MissingSessionID`）与 `User-Agent: minipi/0.1`；上游校验极严，**一个自造字段都不能加**；`do()` 只对「连接级失败」重试 3 次（拿到响应头后绝不重试，否则重复计费）；流式用空闲看门狗（`defaultOpenCodeSSEIdleTimeout = 90s`，可被测试缩短）而非总超时，`reasoning_content` 经 `onReasoning` 转发。
- `mock.go` / `mock_stream.go`：`MockModel{Responses, Calls, Err}`（队列 + 调用记录）、`MockStreaming{ChunkSize}`（rune 安全切片）。

**扩展点**：新增 provider → 实现 `Complete`（+ 按需实现可选接口）→ 在 `llm.New` 的 `switch cfg.Provider` 注册 → 在 `cmd/minipi/main.go` 的 key/model 分支加一条。

### 5.2 `internal/agent` — 核心运行时

**`agent.go`**

- `AgentState`：`AgentID`（空 → `main`）、`SystemPrompt`、`Model`、`Tools *Registry`、`MaxSteps`、`Transforms []TransformFunc`、`Bus *EventBus`、`Messages []Message`、`TotalUsage`，以及私有的 `steerMu` / `steerCh`。
- `Steer(ctx, msg)`：任何 goroutine 可调用，`steerCh`（容量 `SteerQueueCap = 16`）是**唯一**跨 goroutine 通道；队列满时阻塞（背压，不静默丢弃）。
- `drainSteer()`：只在 Run 的 goroutine 调用，把队列里的插话以 `UserMsg("[steer] " + msg)` 追加到历史。
- `emit()`：`Bus == nil` 的 nil 安全包装，并统一填充 `AgentID`；埋点处不再判空。
- `Run` = 壳（`agent_start` / `agent_end` 各发一次，错误路径不漏）+ `run` = 核。

**`message.go`**：`Message` 内嵌 `llm.Message`（JSON 扁平提升）+ `CreatedAt` + `Hidden`。构造函数 `UserMsg` / `AssistantMsg` / `ToolResultMsg` / `UINote`（唯一允许 `Hidden = true` 的入口）。`convertToLlm` 过滤 `Hidden`；`FinalText` 取末条非空 assistant 文本（`SubAgentTool`、`workflow` 都靠它取最终答案）。

**`context.go`**：`TransformFunc` + 三条现成变换 `PruneByTurns` / `PruneByTokens` / `TruncateToolResults`，以及轮切分 `splitTransactions` / `flatten` / `CountTransactions`、粗估器 `estimateTextTokens`（ASCII ≈ 4 字符/token，CJK ≈ 1 字符/token；`tokensPerMsgOverhead = 4`，`tokensPerToolCallOverhead = 8`）。契约见第 6 节不变量 1–4。

**`memory.go`**：`Compact(ctx, st, keepTx)` 破坏性压缩（不可逆，**不得**做成 `TransformFunc`）。摘要输入先 `TruncateToolResults(500)`，摘要以 `UserMsg("[earlier conversation summary]\n"+text)` 放在最前，成本计入 `TotalUsage`，事件 `compaction_start` / `compaction_end`。CLI 阈值：`compactThresholdTx = 8` 轮触发、`compactKeepTx = 4` 保留。

**`execute.go`**：`executeTools` 用 `errgroup` 并行执行同一轮的多个调用，`results[i]` 按索引写回后**在主线程序**一次性 append；`runOne` 用 `recover` + ctx 检查把 panic / 取消 / 未知工具 / 参数错误全部折叠成结果文本。

**`event.go`**：`EventKind` 常量、`Event{AgentID,Kind,Step,Delta,Text,ToolCall,Result,Err}`、`EventBus`（`Subscribe` 返回只读通道 + cancel；`Emit` 在 `RLock` 内阻塞投递，`ctx.Done()` 时放弃）。

**`tool.go`**：`Tool` 接口（`Spec()` 声明 + `Run()` 执行，`args` 用 `json.RawMessage`，解析权归工具自身）；`Registry` 以 `map` + `order` 保注册顺序；`Register` 对空名 / 重名直接 `panic`（启动期编程错误，快速失败）；`BuildRegistry(names, factories)` 供会话恢复；内置 `TimeTool`(`get_time`)、`CalcTool`(`calc`：`a`/`b`/`op`)。

**`subagent.go`**：`SubAgentTool{Model,Tools,MaxSteps,Bus}` 把一整段子 Agent 循环包装成普通工具 `delegate_task`。每次 `Run` 都：`Fork()` provider 会话（若支持）→ 建全新 `AgentState`（`AgentID = "sub-N"`，`subAgentSequence` 原子自增）→ `Run` → `FinalText` 回喂。三条并发契约见不变量 10。

### 5.3 `internal/session` — 持久化

- `Store` 接口四方法：`Save` / `Load` / `List` / `Delete`（`ctx` 预留，为 SQLite / Postgres 的取消语义而设）。`ErrNotFound` 是哨兵错误。
- `Session{ID,CreatedAt,UpdatedAt,Config,State}`；`Config{Provider,ModelName,BaseURL,SystemPrompt,ToolNames}`（**只有名字，没有行为，永不存 key**）；`State{Messages,TotalUsage}`；`Meta{ID,CreatedAt,UpdatedAt,Messages,Title}`（Title = 首条可见 user 消息前 40 rune）。
- `NewID()`：`20060102-150405-<4字节hex>`。
- `JSONStore`：`<dir>/<id>.json`，**原子写**（写 `.tmp` → `rename`），`Load` 做路径穿越防御（拒绝 `/`、`\`、`..`），`List` 跳过损坏文件。
- `SQLiteStore`：单表 `sessions(id PRIMARY KEY, updated_at, data_json)`，`INSERT ... ON CONFLICT(id) DO UPDATE`，正文整体 JSON 存 `data_json`；`modernc.org/sqlite` 驱动，**无需 cgo**；`:memory:` 时限制 `SetMaxOpenConns(1)`。`Close()` 刻意不进 `Store` 接口。
- 并发假设：同一会话同一时刻只有一个 writer（CLI 单进程单会话）；跨进程加锁属未做范围。

### 5.4 `internal/workflow` — 组合层

- `Stage{Name,SystemPrompt,Tools}`（每环节独立的 system 与工具集）、`Gate func(ctx, output) error`（nil 通过，error 拒绝）、`ChainStep{Stage,Gate,Retries}`、`ChainResult{Outputs,TotalUsage}`。
- `RunChain(ctx, model, bus, steps, input)`：严格串行，上一环节输出 = 下一环节输入；**每次尝试都新建 `AgentState`**（`AgentID = "chain-N"`）→ 环节间只经字符串传数据；`bus` 非 nil 时下传给每个环节（流式 delta 实时可见），nil 则全程静默；任何环节耗尽重试即整链中止，已完成产出随错误返回；失败环节的 token 也计入 `TotalUsage`。
- `runStep` 重试输入三件套：原任务 + 被拒产物 + 拒绝理由（"修正"的宾语必须在场）。
- `human.go`：`HumanGate(inputCh, say)` — `y/yes` 通过；`n/no` 追问一行理由并以 `用户否决：<理由>` 作为错误返回（驱动重做）；非法输入提示后继续等；`ctx` 取消立即让路。★ **通道所有权契约**：本门直接消费 `inputCh`，安全前提是链同步执行、主循环此刻阻塞在门内；若把链异步化，必须改为经主循环路由（否则 y/n 会被抢走）。
- `translate.go`：`LengthFloorGate(src)`（`words*4/5` 汉字下限，拦「灾难性丢内容」，**不做跨语言字符数直比**）、`ResidueGate(allowlist)`（`[A-Za-z]{4,}` 未翻译残留）、`TranslateChain(src)`（翻译 → 长度门 → 审校（原文烘焙进 system）→ 残留门 → 润色，润色环节默认无门无重试，由 CLI 外挂 `HumanGate` 并设 `Retries = 1` 让否决理由驱动重做）。

## 6. 必须遵守的不变量

> 动代码前对照这 11 条。违反任何一条都会产生「测试可能不报、真实 Provider 上必炸」的 bug。

### 不变量 1 — 视图 ≠ 历史
管道（`buildView` → `Transforms`）只**裁剪出本次请求的视图**，绝不改写 `st.Messages`。
- `PruneByTurns` / `PruneByTokens` 会 `flatten` 出全新数组；`TruncateToolResults` 慢路径**必须先 `make + copy` 再改**（直接 `msgs[i].Content = ...` 写的是 `st.Messages` 的底层数组 = 永久改写历史）。
- 破坏性操作只能显式做（`Compact`），且**不得**伪装成 `TransformFunc` 挂进管道。
- **后果**：一旦越界，历史被静默篡改、落盘内容与用户所见不一致、重试/压缩基于坏数据。

### 不变量 2 — `TransformFunc` 四契约
1. **只裁视图不改历史**；2. **结构安全**：`assistant(ToolCalls)` 与其 `toolResult` 同进退（以轮为单位操作，永不拆散）；3. **前缀稳定**：相同历史前缀 → 相同视图前缀（否则打断 Provider 提示缓存）；4. **永不丢尾部**：最新一轮必须在输出里（哪怕它自身超预算）。
- **后果**：破 2 → Provider 400（`tool_call_id` 找不到对应调用）；破 3 → 缓存失效、成本翻倍；破 4 → 模型"失忆"，正在进行的对话消失。

### 不变量 3 — 轮（transaction）语义
`splitTransactions` 以**可见 user 消息**开新轮，`Hidden` 消息不开新轮、附着在当前轮内；首条消息开启第一轮（防御性）。所有裁剪 / 压缩 / 计数都基于轮。
- 已知约束：实现假设 `toolResult` 紧随其 `assistant(toolCalls)` —— 当前 `run` 的执行顺序保证了这一点；**若将来在工具阶段中途插入 user 消息，必须同步延长这个定义**。
- **后果**：以消息为单位裁剪会切断配对，直接 400。

### 不变量 4 — `Hidden` 只给 UI 注释
`Hidden = true` 只允许经 `UINote` 构造（如 MaxSteps 兜底注释、未来的内部提示）。协议消息（`assistant+ToolCalls`、`toolResult`）标记 `Hidden` 会破坏配对；`convertToLlm` 是唯一过滤点。
- **后果**：配对断裂 400；或相反——UI 注释泄漏给模型，污染对话。

### 不变量 5 — 每条 `toolCall` 恰好一条 `toolResult`
`runOne` 必须把**任何形态**的失败（工具返回 error / panic / ctx 取消 / 未知工具 / 空参数）折叠成结果文本返回，绝不跳过：这是与 Provider 配对的硬协议，也是 `executeTools` 里 `g.Go` 永远 `return nil` 的原因。
- 空参数要兜底成 `{}`（部分 Provider 对无参工具给空串）。
- **后果**：少一条 `toolResult` → 下一次请求 400，整段会话不可继续。

### 不变量 6 — 错误回喂，而非中断
单个工具失败、子 Agent 失败都是**给模型的信息**，不是程序故障：错误文本回喂让模型自愈（换参 / 换工具 / 放弃）。只有 ctx 取消、模型调用失败、`MaxSteps` 耗尽才结束本轮。
- **后果**：把工具错误当致命错误抛出，会让 Agent 失去自愈能力，也让"部分历史"这一设计失效。

### 不变量 7 — 成本必须可见
**所有** LLM 调用都计入 usage：主循环、`Compact` 摘要、链上每个环节的每次重试、子 Agent（子 state 自己的 usage 由调用方汇总）。`workflow` 连失败环节烧掉的 token 也入账。
- **后果**：隐藏成本 = 账目不可信；一旦有人用 usage 做预算/限流，就是错误数据。

### 不变量 8 — 事件总线纪律
- `Emit` 在 `RLock` 内**阻塞式**投递：保序、保证送达，代价是慢订阅者会拖住 Agent 主循环 → **订阅者必须及时消费**（`cmd/minipi` 的 `printEvents` goroutine 就是专职消费者）。
- 通道**只有 `Subscribe` 返回的 cancel 能 close**，`Emit` 永不 close（fan-out 下关闭权归发起方，避免 `send on closed channel`）。
- `agent_start` / `agent_end` 必须各发一次，**错误路径也不能漏发 `agent_end`**（终态事件丢失 = 订阅者 goroutine 泄漏，goleak 会点名）。
- 缓冲区必须 ≥ 单轮最大事件数（CLI 用 256）。
- 并行工具的事件**相对顺序不保证**（同一工具 start→end 有序）；需要严格顺序的消费者应按 `ToolCall.ID` 归并。
- `Bus == nil` 必须等价于"静默"（既有测试零改动的前提）。

### 不变量 9 — 持久化只存名字，不存行为，永不存 key
`session.Config` 只放「重建行为所需的名字」：`Provider` / `ModelName` / `BaseURL` / `SystemPrompt` / `ToolNames`。行为（工具实例、`Transforms`、`Bus`、模型实例）在恢复时由代码重建（`BuildRegistry` + `toolFactories()`）。**API key 只活在环境变量里**（课程反模式 46：密钥不进持久化）。未知工具名 = 配置漂移（会话由不同版本代码创建）→ 显式报错，不静默跳过。
- **后果**：存 key = 明文泄漏；存行为 = 反序列化出"幽灵状态"，与代码版本脱钩。

### 不变量 10 — 子 Agent 三契约
1. **ctx 必须从父派生**（`runOne` 传入的 `gctx`）→ 父取消能穿透到子的 HTTP 层；**绝不换成 `context.Background()`**。
2. **全新 `AgentState`**，父子零共享切片；任务上下文靠 `task` 字符串自包含（在 `Spec().Description` 里向父模型声明"看不到本次对话"）。
3. **`SubAgentTool.Tools` 必须不含任何 `SubAgentTool`**（防递归失控）—— 由组合根 `buildTools()` 保证（父级注册、子级不注册）；本类型无法自校验，注释即契约。
- 另外：带 Provider 会话状态的 Model 必须在**每次 `Run`** 时 `Fork()`（不能只在构造期 fork 一次），否则并发子代理共享同一会话 ID。
- **后果**：破 1 → Ctrl+C 杀不死子请求；破 2 → 对话串味；破 3 → 无限递归。

### 不变量 11 — 并发与 nil 安全
- `AgentState` 由 `Run` 的 goroutine **独占读写**，单 goroutine 下**不加锁**；唯一的跨 goroutine 通道是 `steerCh`（`steerMu` 只保护 `steerCh` 的懒创建）。
- **历史 `append` 只发生在主 goroutine**：`executeTools` 是"并行执行、串行写回"。
- `Tools == nil`（无工具）与 `Bus == nil`（静默）都是合法配置，必须 nil 安全。
- 不提前抽象：`Registry` 运行期只读、不加 `RWMutex`，等真的需要热插拔再加。
- **后果**：给 `AgentState` 随意加锁 = 把"无锁单线程模型"这一核心简化摧毁，并掩盖真实的竞态设计问题。

## 7. 扩展指南

### 7.1 新增一个工具（4 步）

1. **实现 `Tool`**（`internal/agent/tool.go` 或新文件）：`Spec()` 给出名字 / 描述 / JSON Schema（描述要写给模型看，说清"什么时候用"）；`Run(ctx, args)` 用 `json.RawMessage` 自己 Unmarshal，出错就 `return "", err`（错误会被回喂给模型）。
2. **登记工厂**：在 `cmd/minipi/main.go` 的 `toolFactories()` 加 `"<name>": func() agent.Tool { return agent.NewXxxTool() }` —— 会话恢复路径（`BuildRegistry`）靠它重建行为。
3. **决定归属**：在 `buildTools()` 里选择放进 `base`（子 Agent 也能用）还是只放 `parent`（仅父可用）。注意 `base` 里的工具也要显式注册进 `parent`。
4. **补测试**（`internal/agent/tools_test.go`）：用 `MockModel` 驱动一轮「声明 → 调用 → 回喂」，断言 `Spec()`、参数解析、错误回喂路径。

⚠️ 工具名一旦发布就是**持久化数据**（写入 `session.Config.ToolNames`）：改名/删除会让老会话恢复时报"unknown tool"（这是有意的显式失败，不要改成静默跳过）。工具内**不要**直接碰 `st.Messages`。

### 7.2 新增一个 Provider

1. **实现适配器**（新文件 `internal/llm/<name>.go`）：必需 `Complete`；需流式再加 `StreamComplete`（契约：`拼接(deltas) == 返回值.Text`）；需思考流加 `StreamCompleteWithReasoning`；需 Provider 侧会话加 `Sessioned` / `SessionForker`。
2. **注册**：在 `llm.New` 的 `switch cfg.Provider` 加分支。
3. **组合根**：在 `cmd/minipi/main.go` 的 provider `switch` 里补 `apiKey` 与 `defaultModel`。
4. **补测试**：`httptest` 假服务断言请求 wire 字段与响应解析（参考 `llm/opencode_test.go: TestOpenCodeComplete`），SSE 至少覆盖「分片工具调用按 index 重组」与「usage 在最后一个 chunk」。
5. **wire 纪律**：只发标准字段；暴露给严格端点时宁可少发也别多发（一个自造字段就是 400）。超时策略按「非流式用总超时 / 流式用空闲看门狗」区分。

### 7.3 新增一个 `TransformFunc`

- 严守第 6 节不变量 1、2（只裁视图、结构安全、前缀稳定、永不丢尾部）；慢路径改内容前先 `make + copy`（照抄 `TruncateToolResults` 的快/慢路径结构）。
- 单测必查三件事：**输入历史未被改写**、`toolCall`/`toolResult` 配对完整、**最新轮仍在视图里**（参考 `internal/agent/context_test.go`）。
- 挂载点：`AgentState.Transforms`（切片顺序 = 执行顺序），CLI 当前是 `PruneByTurns(2)`。
- 做预算类变换时注意：真实上下文还包含 system 与工具声明，当前 `estimateTextTokens` 只估消息体（已知简化，见第 9 节）。

### 7.4 新增一个 `Gate` 或工作流环节

- `Gate` 返回 `nil` = 通过；返回 error = 拒绝，**错误文本会拼进重试输入**（`Retries` 控制重试上限，`0` = 拒绝即整链中止）。
- 门要廉价：正则 / 长度 / 结构校验优先；要用 LLM 校验就把它当成一次**可计费调用**并明确记账。
- 门的分工要单一：翻译链里长度门管「量」、残留门管「质」，不要写一个"什么都管"的巨型门。
- 环节独立性：每个 `Stage` 自带 `SystemPrompt` 与 `Tools`；环节之间只传字符串。
- 需要人工输入时必须用 `workflow.HumanGate`，并遵守它的**通道所有权契约**（链同步执行时才能直接消费 `inputCh`）。

### 7.5 新增持久化字段

先自问：「这是数据，还是行为？」
- 数据（消息、usage、时间戳）→ `session.State`。
- 重建行为所需的名字 → `session.Config`。
- 实例 / 函数 / 密钥 → **不持久化**（恢复时由代码重建）。
- 加字段后**必须**同步更新 `internal/session/session_test.go` 的往返断言（`TestRoundTrip` 逐字段检查），否则字段会被静默丢弃。

## 8. 测试规范

- **全离线、零 API 费用**：上层逻辑用 `llm.MockModel` / `llm.MockStreaming`；HTTP 层用 `httptest` 起假 provider（见 `internal/llm/model_test.go`、`internal/llm/opencode_test.go`、`internal/agent/reasoning_test.go`）。**禁止**在测试里发真实请求。
- **goroutine 泄漏哨兵**：`internal/agent` 与 `internal/workflow` 的 `TestMain` 已挂 `goleak.VerifyTestMain(m)`。新增涉及 goroutine / 订阅者 / 泵的包请沿用同一纪律；测试里等终态事件要带超时兜底（参考 `drainEvents` 的 `time.After`），否则"事件丢失"会表现成永久挂起而不是失败。
- **命名与注释风格**：`TestXxx` + 中文「场景 N：……」说明剧本；断言消息写清"期望 vs 实际"。同一个测试文件里多个场景应当各自独立可读。
- **并发测试要证据，不要 sleep**：用栅栏（如 `subagent_test.go` 的 `forkProbeModel` 的 `entered` / `release`）证明"确实并发"或"确实隔离"；`-race` 是并发包的常规检查。
- **Mock 编排**：一个执行者一份队列（"谁的剧本谁的队列"）；共享一份队列再去交错消费是已知反模式（62），不要这样写新测试。
- **不要为了测试给生产代码开后门**（唯一例外是像 `OpenCode.sseIdleTimeout` 这种真实需要的可调参数，测试用它把 90s 缩短到毫秒级）。
- **不迁就实现改断言**：断言反映契约（事件序列、消息序列、usage 数值）。实现与断言冲突时，先判断是哪一侧错了。
- **隐私与卫生**：测试不写入真实 key、不读取仓库内会话库；会话/存储测试统一 `t.TempDir()`；`minipi_sessions.db` 与 `.idea` 个人文件已在 `.gitignore` 中。

## 9. 已知简化、刻意取舍与待办

**这些不是 bug，不要"顺手修"**（要动请先改设计并同步本文件）：

| 项 | 现状 | 说明 |
| --- | --- | --- |
| `cmd/minipi/day1/main-day1.go` | 保留 | day1 历史快照（独立 `package main`），作为教学对照存在 |
| 链同步执行 | 刻意 | `RunChain` 运行期间主 goroutine 阻塞，链内不接受插话（除 `HumanGate`）；异步化 + steering 贯通是待办 |
| `HumanGate` 直接消费 `inputCh` | 刻意 | 安全前提是"链同步执行"；异步化时必须改为经主循环路由 |
| `openai_stream.go` 的消息编码 | 技术债 | 与 `Complete` 有约 20 行重复（`toOAMessages` / `toOATools` 待抽取）；`opencode` 侧已抽成 `buildRequest` |
| `estimateTextTokens` | 粗估 | ASCII ≈ 4 字符/token、CJK ≈ 1 字符/token；预算未计入 system 与工具声明开销 |
| 单进程单会话写入 | 刻意 | 跨进程无文件锁；`JSONStore` 靠 `.tmp` + `rename` 保原子性 |
| SQLite 无迁移机制 | 刻意 | 只有 `CREATE TABLE IF NOT EXISTS`；正文整体存 `data_json`，字段演进靠 JSON 兼容 |
| 无 `/resume` 命令 | 未做 | 恢复靠环境变量 `MINIPI_SESSION_ID`；`/list` 只列出不切换 |
| Anthropic 无流式 | 未做 | 只实现 `Complete`，Agent 循环自动回落非流式路径（能力探测的好处） |
| 工具事件全局顺序 | 刻意 | 并行执行下不同工具的相对顺序不保证（见不变量 8） |
| 并行工作流 | 未做 | `RunChain` 严格串行；并发放后续阶段 |
| `runTranslateDemo` 的文档注释 | **已过期** | 注释称"链内 AgentState 未接事件总线"，但代码实际把 `bus` 传入 `RunChain`（`runStep` 里 `Bus: bus`）→ 以代码为准，注释待清理 |

**真正的待办线索**：代码里的「作业 N」「Day N」注释是原始课程的练习入口；搜索 `作业` / `TODO` 可找到它们所在位置。

## 10. 术语表

| 术语 | 出处 | 含义 |
| --- | --- | --- |
| transaction（轮） | `agent/context.go` | 从一条可见 user 消息开始、到下一条 user 消息之前的消息序列；裁剪/压缩/计数的最小单位 |
| view（视图） | `agent/agent.go: buildView` | 本次请求实际发给模型的消息切片；由历史经 Transforms + `convertToLlm` 得到，**不是**历史本体 |
| steer（插话） | `agent/agent.go: Steer` | Agent 运行期间由外部 goroutine 注入的用户消息；在 `drainSteer` 的注入点进入历史 |
| gate（门） | `workflow/chain.go` | 环节出口的廉价检查点；返回 error 即拒绝，理由被拼进重试输入 |
| retry | `workflow/chain.go: ChainStep.Retries` | 门拒绝后的重做次数；每次重做都是**全新** `AgentState` + 完整 token 成本 |
| stage / chain | `workflow/chain.go` | 工作流环节 / 环节串成的数据管道（上一环节输出 = 下一环节输入） |
| compaction | `agent/memory.go: Compact` | 破坏性压缩：老轮次 → 一条摘要消息；不可逆，非 Transform |
| fork | `llm.SessionForker` | 为并发子 Agent 派生独立的 Provider 会话（配置只读共享，会话 ID 独立） |
| hidden | `agent/message.go` | UI-only 标记：留在历史里但永不发给模型；只允许经 `UINote` 构造 |
| pairing（配对） | `agent/execute.go` | 每条 `toolCall` 必须有对应 `toolResult`（靠 `ToolCall.ID` / `ToolCallID`）；破坏即 400 |
| advisory vs lifecycle 事件 | `agent/event.go` | `compaction_*` 是旁路事件（失败不发、不影响收尾）；`agent_end` 是生命周期事件（必发） |
| backpressure（背压） | `agent/agent.go: Steer` | 队列满时 `Steer` 阻塞而不是丢弃插话 —— 用户意图不许静默丢 |
| drain | `agent/agent.go: drainSteer` | 一次性取空插话队列，转成 user 消息追加进历史 |
| wire format | `internal/llm/*` | 各 Provider 的 HTTP 请求/响应结构；统一消息在这里被"翻译"成 wire 消息 |
| `AgentID` | `agent/event.go` | 事件来源标识：`main` / `sub-N` / `chain-N`，用于区分并发流 |

---

**改动本文件时**：请同步更新「已知简化」表与相应章节，并在 commit message 里写明依据（契约变更 / 新增能力 / 修正文档漂移）。