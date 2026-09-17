# minipi

用 Go 从零实现的迷你 Agent 运行时（教学项目，day1 → day8 递进）。
每层递进一个能力：Provider 抽象 → Agent 循环 → 工具注册 → Context 管道 → 事件总线 → 会话持久化 → steering 插话 → 链式工作流 / 子 Agent / 人在环。

## 目录结构

```text
minipi/
├── cmd/minipi/
│   ├── main.go            # 组合根：环境变量 → Model / Store / EventBus / 工具集 → REPL
│   ├── events.go          # 事件渲染（流式正文、思考、工具调用、压缩）
│   └── day1/main-day1.go  # day1 历史快照（独立 main 包）
└── internal/
    ├── llm/               # LLM Provider 抽象与适配（openai / anthropic / opencode / mock）
    ├── agent/             # 核心循环 + 状态 + Context 管道 + 事件总线 + 工具 + 子 Agent
    ├── session/           # 会话持久化（JSONStore / SQLiteStore）
    └── workflow/          # 链式工作流（Stage / Gate / Retry）、人在环门、翻译链
```

## 运行

```bash
export MINIPI_PROVIDER=opencode     # openai | anthropic | opencode（默认）
export OPENCODE_API_KEY=...         # 按 provider 取用对应 key
go run ./cmd/minipi
```

环境变量：`MINIPI_PROVIDER`、`MINIPI_MODEL`、`MINIPI_BASE_URL`、`MINIPI_SESSION_ID`（设置即恢复该会话）、`MINIPI_SESSION_DB`（默认 `minipi_sessions.db`）、`OPENAI_API_KEY` / `ANTHROPIC_API_KEY` / `OPENCODE_API_KEY`。

## CLI 交互

| 输入 | 行为 |
| --- | --- |
| 普通文本 | 发起一轮对话 |
| 运行中任意文本 | steer：插话，在下一轮调模型前生效 |
| `/new` | 清空历史并切换 Provider 会话 |
| `/info` | 当前会话 ID / 消息数 / 轮数 / 累计 token |
| `/list` | 列出已保存的会话 |
| `/chain <主题>` | 演示链式工作流（提纲 → 门 → 正文） |
| `/translate <英文>` | 演示翻译三段链（翻译→长度门→审校→残留门→润色，润色环节由人工终门把关） |
| `/quit` | 退出 |

## 测试

```bash
go test ./...                                        # 全部离线（Mock + httptest，不花 API 费用）
go test -race ./internal/agent/ ./internal/workflow/  # 并发敏感包
```

架构细节、设计不变量与扩展指南见 [AGENTS.md](./AGENTS.md)。
