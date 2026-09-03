mini-pi/
├── cmd/cli/              # 命令行入口
├── internal/
│   ├── llm/              # LLM Provider 抽象
│   ├── agent/            # 核心循环 + 状态管理
│   ├── tool/             # 工具注册与执行
│   ├── context/          # Context 管道
│   ├── event/            # 事件总线
│   ├── session/          # 会话持久化
│   └── orchestrator/     # 多 Agent 编排

|day|学习板块|功能目标|核心交付|
