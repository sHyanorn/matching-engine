# CLAUDE.md

本仓库是 Aaron 的作品集项目：一个用 Go 写的限价订单簿撮合引擎。代码清晰度和可验证的性能数据同样重要。

## 开工前必读

1. `docs/SPEC.md`：需求和架构，遇到冲突以它为准
2. `docs/TASKS.md`：从第一个未勾选的里程碑开始
3. `docs/DECISIONS.md`：已做出的决定，不要擅自推翻；如需修改，先说明理由并征得同意

## 工作方式

- 每个里程碑开始前，先给出实现计划，等 Aaron 确认
- 完成一个任务就提交一次，提交信息用 Conventional Commits 英文格式
- 完成后在 TASKS.md 打勾；新的技术决定追加到 DECISIONS.md

## 常用命令

```bash
make help                           # 列出 Makefile 里的快捷命令
go test -race ./...                 # 全部测试
go test -short ./...                # 快速运行，属性测试只跑 1 万条命令
go test -bench=. -benchmem ./internal/book
go test -fuzz=FuzzBook -fuzztime=30s ./internal/book
golangci-lint run ./...             # 静态检查，包含 govet 和 staticcheck
go run ./cmd/engine                 # 启动服务
go run ./cmd/loadgen -rate 50000    # 压测
```

## 硬性规则

- 价格和数量一律用 `int64` 整数（价格单位为最小报价单位 tick），禁止使用浮点数
- 撮合核心 `internal/book` 不依赖任何网络、时间或随机数；时间戳由调用方传入，保证可重放、可测试
- 撮合核心尽量只用标准库；订单簿的树或跳表结构自己实现，这是作品的展示重点
- 每个公开函数都要有测试；修改撮合逻辑必须同时跑模糊测试和不变量测试
- 性能相关改动必须附带前后基准测试对比，结果写入 `bench-results/`
- 不要为了性能牺牲可读性，除非基准测试证明收益明显，并在 DECISIONS.md 记录
