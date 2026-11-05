# 任务清单

完成一项就打勾。每个里程碑结束时所有测试必须通过。

## M0 项目初始化
- [x] 确认 go.mod 模块路径，加入 staticcheck 和 golangci-lint 配置
- [x] 定义核心类型：Order、Side、OrderType、TIF、Trade、Event、Command
- [x] 写一个 Makefile：test、bench、fuzz、lint、run

## M1 订单簿核心
- [x] 实现价格档位有序结构（红黑树或跳表），带独立单元测试
- [x] 实现 PriceLevel 的 FIFO 双向链表
- [x] 实现限价单 GTC 入簿与撮合
- [x] 实现撤单（O(1) 定位）
- [x] 实现 L2 盘口深度查询
- [x] 表驱动撮合场景测试，至少 20 个场景

## M2 完整订单类型与正确性
- [x] 市价单
- [x] IOC 和 FOK
- [x] 不变量检查函数 `book.CheckInvariants()`
- [x] 属性测试：10 万条随机命令，每步检查不变量
- [x] 模糊测试 `FuzzBook`

## M3 引擎层
- [x] 每个交易对一个 goroutine 的事件循环
- [ ] 命令日志（追加写文件）与重放
- [ ] 重放确定性测试
- [ ] 事件总线与订阅者扇出
- [ ] 有界通道背压

## M4 网络接口
- [ ] REST 接口与输入校验
- [ ] WebSocket 订阅与推送，消息序号
- [ ] 优雅关闭（停止接收、处理完队列、刷新日志）
- [ ] Prometheus 指标：订单数、成交数、队列长度、延迟直方图
- [ ] 集成测试

## M5 性能
- [ ] 撮合核心基准测试
- [ ] `cmd/loadgen` 可复现订单流生成器
- [ ] 端到端延迟测量
- [ ] pprof 分析并至少完成一项有数据支撑的优化（例如对象池减少分配）
- [ ] 写 `docs/BENCHMARKS.md`，附图

## M6 展示
- [ ] Dockerfile 和 docker compose
- [ ] README（只写英文版）：介绍、架构图、性能表、GIF、快速启动
- [ ] 选做：简单网页盘口可视化
- [ ] 整理 DECISIONS.md，确保每个决定都有理由
