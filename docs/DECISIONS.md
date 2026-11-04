# 技术决策记录

格式：编号、日期、决定、理由、备选方案。新决定追加到末尾。

## D001 使用 Go
- 日期：2026-11-04
- 决定：用 Go 实现
- 理由：后端岗位认可度高；goroutine 和 channel 适合事件循环模型；自带基准测试、模糊测试、pprof
- 备选：Rust（性能更高，但学习成本高）、TypeScript（已熟悉，但不适合展示底层性能）

## D002 价格和数量用整数
- 日期：2026-11-04
- 决定：价格以最小报价单位存为 int64，数量同样用 int64
- 理由：浮点数有精度误差，会导致比较和累加出错；真实交易系统普遍使用定点整数
- 备选：decimal 库（正确但慢）

## D003 单线程撮合
- 日期：2026-11-04
- 决定：每个交易对一个 goroutine 串行处理命令
- 理由：撮合天然要求顺序；无锁设计更快更简单，结果可确定性重放
- 备选：细粒度锁（复杂且容易出错）

## D004 价格档位有序结构
- 日期：待定
- 决定：红黑树或跳表，M1 开始时对比后决定

## D005 静态检查统一用 golangci-lint
- 日期：2026-11-04
- 决定：用 golangci-lint v2（版本固定为 v2.5.0）统一做静态检查，staticcheck 作为其中一个检查器启用，不再单独安装；配置在 `.golangci.yml`
- 理由：一个工具、一份配置、本地和 CI 版本一致；staticcheck 的规则一条不少。另外用 depguard 禁止 `internal/book` 的非测试代码导入 `time`、`math/rand`、`crypto/rand`、`net`、`os`，把"撮合核心不依赖时间、随机数和网络"这条规则交给 CI 自动检查
- 备选：单独安装 staticcheck（多一个工具要维护版本，且没有导入限制）

## D006 核心类型只含定长整数，事件追加到调用方的缓冲区
- 日期：2026-11-04
- 决定：Command（40 字节）、Event（64 字节）、Order（64 字节）、Trade、LevelUpdate 只由定长整数字段组成，可以用 `==` 比较。所有事件种类共用一个 Event 结构体，用 Kind 区分，字段用法见 Event 的注释。M1 的接口定为 `Book.Apply(cmd Command, out []Event) []Event`，调用方写 `buf = b.Apply(cmd, buf[:0])`；下一次调用会覆盖 buf，交给其他 goroutine 前要先复制。`contract_test.go` 固定这些类型的大小，并检查字段只含整数
- 理由：缓冲区长大后，发出事件不再分配内存，GC 也不用扫描事件切片。草稿基准（Xeon 2.1GHz）：追加 3 个事件约 27 ns，0 次分配。表驱动测试和重放测试可以直接用 `slices.Equal` 比较整段事件
- 备选：每种事件一个结构体再装进接口（每个事件分配一次，到处要 type switch）；回调函数（不分配，但每个测试都要写记录器）

## D007 OrderID 和 Timestamp 由引擎在写日志前填入 Command
- 日期：2026-11-04
- 决定：OrderID 由引擎分配，在整个日志里跨交易对唯一，0 无效；book 只能发现与当前挂单重复的 ID。Timestamp 是调用方给的 int64（引擎用 Unix 纳秒），book 原样复制，从不比较。时间优先就是命令被应用的先后顺序。symbol 和 client_id 不进入 Command，由引擎和 API 层处理
- 理由：日志自包含，重放不依赖时钟或 book 内部计数器；`DELETE /v1/orders/{id}` 不带 symbol，所以 ID 必须跨交易对唯一；时钟回拨不影响撮合
- 备选：由 book 分配 ID（跨交易对会冲突）；按时间戳排序（优先级受时钟质量影响）

## D008 枚举零值无效，数值显式写出并由测试固定
- 日期：2026-11-04
- 决定：Side、OrderType、TIF、CommandKind、EventKind 的零值无效，Reason 的零值是 ReasonNone。所有常量写出显式数字（如 `Buy Side = 1`），`TestWireValuesArePinned` 固定它们，以后只追加不改号。每个枚举有一张名字表，String 和 Parse 函数共用。拼写统一用美式 canceled
- 理由：漏填字段或 JSON 缺字段会被拒绝，不会悄悄变成 buy 或 limit。这些数字写进命令日志，重排常量会破坏旧日志，测试能拦住
- 备选：iota 从 0 开始（漏填字段等于一个合法值）；用 UnmarshalText 直接解码 JSON（拼错时错误码要再映射一次）

## D009 Command.Validate 是唯一的无状态校验
- 日期：2026-11-04
- 决定：`Validate()` 返回 Reason，合法时为 ReasonNone，检查顺序固定。市价单必须是 IOC 或 FOK 且 Price 为 0；限价单 Price 在 [1, MaxPrice]；撤单只能带 OrderID 和 Timestamp。API 用它直接回 400，book 对每条命令再查一次
- 理由：两层共用一个函数，结论不会不一致；顺序固定保证多处出错时原因确定，重放结果稳定；每种意图只有一种合法表示，未使用的字段必须为 0，以后改单等功能可以启用这些字段而不影响旧日志
- 备选：返回 error（会分配，调用方还要做类型断言）；市价单忽略 TIF 和 Price（日志不规范）

## D010 FOK 不能全部成交时整单拒绝
- 日期：2026-11-04
- 决定：FOK 先累加对手方可成交的档位总量，不够就只发一个 `Rejected(fok_unfillable)`，没有 Accepted 也没有 Trade，订单簿不变。IOC 和市价单的剩余部分是 Accepted ... `Canceled(ioc_remainder)`
- 理由：SPEC 第 6 节第 4 条写的是"不够就整单拒绝，不产生任何成交"，比第 3 节的"全部成交否则撤销"更具体；这样"Rejected 表示命令什么都没改变"始终成立，数量守恒检查不需要特殊情况
- 备选：Accepted 加 `Canceled(fok_unfillable)`（与第 6 节字面冲突）

## D011 事件序列、档位总量和 Seq
- 日期：2026-11-04
- 决定：新单的事件序列是 Rejected，或 Accepted、若干 Trade、最后 Rested 或 Canceled；撤单是 Rejected 或 Canceled。改变挂单的事件（Trade、Rested、撤掉挂单的 Canceled）带上被改变档位的方向和事件之后的绝对总量。Seq 每个 book 从 1 开始，每个事件加 1，拒单也计入；Trade 的 Seq 就是成交序号。WebSocket 每个频道用引擎自己的连续计数器
- 理由：只看事件流就能检查数量守恒；只有 book 能廉价地拿到档位总量，用绝对值时引擎按 (方向, 价格) 保留最后一个值就能生成 book_delta，不需要另建一份订单簿；频道只看到部分事件，直接用 Seq 会让客户端分不清真断档和被过滤的事件
- 备选：单独的 BookDelta 事件（事件更多，Qty 字段一个名字两种含义）；事件不带档位信息（引擎要维护盘口镜像）

## D012 Price 和 Qty 是基于 int64 的具名类型，上限都是 1e9
- 日期：2026-11-04
- 决定：`type Price int64`、`type Qty int64`、`type OrderID uint64`。Validate 要求价格不超过 1e9 tick、数量不超过 1e9 lot
- 理由：仍然是 D002 的 int64 整数，编译器还能阻止把价格当数量用。单笔成交金额最多 1e18，小于 int64 上限约 9.2e18；900 万张满额挂单的总量仍小于 2^53，在 JavaScript 客户端里是精确整数。`TestBoundsCannotOverflow` 把这些论证写成了测试。以后提高上限不影响旧日志，降低会
- 备选：裸 int64（价格和数量变量互换也能编译通过）；不设上限（模糊测试很快会让档位总量溢出）

## D013 book 只保存挂单，Order 记录 RestedSeq
- 日期：2026-11-04
- 决定：book 只保存挂单（目前只有限价 GTC），成交完或撤掉的订单直接移除，历史和状态由引擎根据事件维护。Order 记录 RestedSeq，即让它获得当前排队位置的那个 Rested 事件的 Seq
- 理由：每个价位里 RestedSeq 从队头到队尾严格递增，CheckInvariants 可以借此检查时间优先；以后改单减量时保留它，改价时换新值，"保留优先级"就能测试；快照恢复也能保持排队顺序
- 备选：book 保留已结束的订单（内存无限增长）
