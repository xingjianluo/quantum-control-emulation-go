# 本地量子控制编译、调度与校准仿真 — 设计说明

本项目是一个**纯本地、零外部依赖**的量子控制平面仿真框架：把高层门任务编译为带时序与
依赖的脉冲计划，以分层版本管理校准数据，用确定性噪声模型对顺序/并发执行做可复现的
结果聚合。所有"失败"都带有可程序化判别的**原因码**，不会被静默吞掉。

```
高层门任务 []ir.GateTask
      │  compiler（参数/依赖/校准校验，绑定校准版本）
      ▼
ir.PulseProgram（脉冲操作 + 依赖，未排时）
      │  scheduler（成环/缺依赖/资源冲突校验；确定性 ASAP）
      ▼
带时序的 ir.PulseProgram（Start/Duration/资源窗口）
      │  simulator（虚拟时刻提交屏障 + 确定性哈希噪声）
      ▼
simulator.Result（逐操作结论 + 规范化因果事件流 + 计数）
```

包结构：

| 包 | 职责 |
|---|---|
| `internal/ir` | 门任务、资源、脉冲操作、脉冲计划等共享中间表示 |
| `internal/calibration` | 校准快照的草稿/提交/失败/回滚、容差与过期校验、审计日志 |
| `internal/compiler` | 高层门 → 脉冲；参数合法性、依赖可解析性、校准齐备性校验 |
| `internal/scheduler` | 依赖图校验、资源冲突检测、确定性 ASAP 排程与重排 |
| `internal/simulator` | 顺序/并发执行、确定性噪声、取消/超时/部分失败聚合、因果事件 |
| `cmd/demo` | 端到端演示入口 |
| `internal/integration` | 跨包端到端测试 |

---

## 1. 编译与调度模型

### 1.1 资源与占用窗口

每条脉冲声明占用若干**硬件资源**：

- `qubit-N`：驱动通道（X / X90 / RZ）
- `coupler-C-T`：耦合器通道（CNOT 同时占用 coupler、control、target 三类资源）
- `readout-N`：读出通道（MEASURE，不同比特的读出可并行）

占用区间为**半开窗口 `[Start, Start+Duration)`**：`[a,b)` 与 `[b,c)` 首尾相接不冲突，
任何严格重叠都被拒绝。CNOT 同时锁定两个比特，因此与作用在这两个比特上的其它操作
天然互斥。

### 1.2 编译器（compiler）

编译期依次做以下检查，任一不满足立即返回带 `FailReason` 的错误：

| 原因码 | 触发条件 |
|---|---|
| `unknown_gate` | 门类型不在 X/X90/RZ/CNOT/MEASURE 内 |
| `illegal_param` | 比特数不符、负编号、CNOT 同比特、RZ 角度为 NaN/Inf 等 |
| `duplicate_task_id` | 门任务 ID 重复 |
| `unresolved_dependency` | 依赖的任务不存在或自依赖 |
| `missing_calibration` | 当前稳定快照中缺少该门所需的 `amp/freq` 参数或读出保真度 |

编译通过后：

- 每个门映射为一条 `PulseOp`（ID 为 `p-<GateID>`，保留 `GateID` 便于追溯）；
- 门级依赖原样翻译为脉冲依赖；
- 整份计划绑定**当时的稳定校准版本号** `CalibVersion`；
- `Start` 留零，交由调度器排时。

### 1.3 调度器（scheduler）

`Assign` 采用确定性 ASAP（As-Soon-As-Possible）列表调度：

1. 先校验 ID 唯一、依赖存在、无自依赖、依赖图无环（Kahn 拓扑排序）；
2. 每轮从所有"依赖已完成"的就绪操作中，按键 `(依赖最大结束时间, "输入下标#ID")`
   取最小者——该全序键保证**相同输入永远得到相同计划**；
3. 在该操作涉及资源的已放置窗口集合中，找出不早于依赖下界的最早无冲突槽位。

`Validate` 对一份**已带时序**的计划独立做四类可区分拒绝：

- `missing_dependency` / `self_dependency` / `cyclic_dependencies`
- `resource_overlap`（携带冲突资源名与两条冲突操作）
- `dependency_after_start`（依赖结束晚于本操作开始）
- `duplicate_op_id` / `invalid_time`

### 1.4 重排（Replan）语义

`Replan` 深拷贝计划后重新 ASAP 压实。重排**只允许改变 `Start`**，以下语义要素逐字段
保持不变：操作 ID、来源门、操作类型、时长、参数、依赖集合、校准版本。测试
`TestReplanPreservesSemanticsAndReducesIdle` 同时验证：语义不变、makespan 不增加、
总空闲严格不增加、多次运行结果一致。

---

## 2. 校准与回滚规则

### 2.1 分层版本生命周期

```
BeginDraft ──► draft ──Commit(容差校验)──► stable（不可变，版本号递增，成为"当前稳定版"）
                 │
                 └──MarkFailed──────────► failed（留档、可审计、永不可用）
```

- 所有已提交快照**不可变**，访问器返回深拷贝；
- `Commit` 相对"上一稳定版本"做参数漂移校验：`Tolerance.ParamDelta` 逐参数给出绝对
  偏差上限，未列出的参数默认**零容差**；读出保真度必须在 `[0,1]` 且在
  `[ReadoutMin, ReadoutMax]` 内；
- 校验失败不会产生新版本、不推进当前指针（原因码 `param_out_of_tolerance` /
  `readout_out_of_range`）；
- 所有 begin/commit/fail/rollback 动作进入追加式 `AuditLog`。

### 2.2 不静默沿用旧值

编译与仿真在使用校准数据前都必须通过 `Store.ValidateAt(version, at, ttl)`：

| 原因码 | 含义 |
|---|---|
| `no_stable_version` | 仓库没有任何稳定版本 |
| `bad_snapshot` | 版本不存在 / 草稿态 / 失败态 |
| `calibration_expired` | 距快照提交时刻超过 TTL（`ttl<=0` 表示不过期） |
| `version_mismatch` | 计划绑定版本不是当前稳定版本（已有更新的稳定版） |

过期或版本错位时**直接拒绝**，绝不回落到旧参数继续跑。

### 2.3 回滚与历史复现

- `Rollback` 沿"稳定版本链"（失败快照不在链上）把当前指针前移一位；
  没有更早稳定版本时以 `no_previous_version` 拒绝；
- 回滚**不删除任何历史快照**，任意版本仍可通过 `Get(version)` 取回；
- **重要语义**：回滚只是切换指针，不会让"按墙钟已过期"的旧快照重新变新鲜。
  要复现历史结论，必须显式以该快照的历史时刻（`Snapshot.CreatedAt` 附近）进行
  "历史重放"。此时由于噪声是 `(seed, 版本, opID, 用途)` 的纯哈希函数，相同输入必然
  得到与历史记录逐条一致的结论。见集成测试与 demo 的 `rollback-verify` 步骤。

---

## 3. 仿真与并发语义

### 3.1 确定性噪声模型

不使用全局随机源。对每个判定，取 `FNV-1a(seed, calibVersion, opID, tag)` 的哈希映射
到 `[0,1)`：

- 门错误基线：单比特门 0.5%，两比特门 2%（`tag="gate"`）；
- 读出：以校准快照中的 `ReadoutFidelity[q]` 为正确概率，否则确定性翻转
  （`tag="readout"`）；
- 门错误与读出错误使用不同 `tag`，是相互独立的判定；
- 理想态采用最简经典玩具模型（仅用于产生可解释预期）：X/X90 翻转计算位、
  RZ 仅改相位、CNOT 在控制位为 1 时翻转目标位。

因此结论与 Go 运行时调度、机器、运行次数均无关；换 seed 才会换一条确定轨迹。

### 3.2 虚拟时刻提交屏障（并发等价的关键）

并发模式下每个操作一个 goroutine，但 goroutine **只负责等待**；所有改变共享状态的
动作（理想态演化、测量取值、结论落账、事件落账）都通过单一驱动循环经**提交屏障**
按全局全序生效：

```
提交顺序 = 按 (虚拟结束时刻 op.End(), opID) 排序
```

顺序模式不启动多个 goroutine，而是按确定性拓扑序逐条走**完全相同的等待→提交路径**。
因此：

- 两种模式的 `Outcomes` 逐字段等价，规范化后的 `Events` 等价；
- 并发内部无论 goroutine 如何交织（测试中重复 20 次 + `-race -count=N`），
  结论不漂移；
- 事件在最终结果中按 `(Virtual, OpID, Phase 序, Seq)` 稳定排序输出，因果关系可解释。

### 3.3 取消、超时与部分失败

- **依赖失败**：上游一旦失败/取消/超时，下游操作不执行，记 `skipped_dependency`
  （只产生 skip 事件，不产生 start/测量值，因果链不错位）；
- **超时**：按计划脉冲时长与 `Config.OpTimeout` 的**虚拟时间比较**确定性判定
  （`pulse.Duration > OpTimeout`），不使用墙钟 deadline，避免机器调度抖动导致
  不可复现；`DeterministicWaiter` 提供同样基于时长的注入；
- **取消（确定性虚拟取消点）**：`Config.CancelAt > 0` 时，取消点落在操作半开窗口
  `(Start, End]` 内的在途操作记为 `canceled`；它之前的操作不受影响，它之后的下游
  按依赖链 `skipped_dependency`。该规则纯由计划时序决定，因此顺序/并发模式
  结论逐字段等价（测试重复 20 次 + `-race` 验证）；
- **取消（外部 context）**：通过外层 `context.Context` 与 `BlockingWaiter` 注入——
  Waiter 在指定操作真正进入阻塞时经 `Entered` 通道通知，测试收到信号后再 cancel，
  不依赖固定 sleep。外部取消与 goroutine 完成天然存在竞态（某独立操作是否赶在
  cancel 前完成不可预测），因此外部取消测试只断言与交织无关的不变量：恰好一次、
  计数守恒、失败下游必 skip、因果事件不错位；需要逐比特可复现的结论时应使用
  `CancelAt`；
- **恰好一次**：每条操作在 `Outcomes` 中恰好一条记录，`Completed+Failed == 操作数`；
  聚合阶段还有防御性分支保证即使出现未登记操作也不会"遗漏"；
  所有共享状态仅在持锁的提交屏障内变更，`-race` 下零告警。

### 3.4 失败原因一览（simulator）

| 引擎级错误 | 含义 |
|---|---|
| `bad_program` | 计划未通过调度器校验（资源冲突/依赖非法等） |
| `calibration_invalid` | 校准版本不存在/草稿/失败态 |
| `calibration_expired` | 校准超过 TTL |
| `version_mismatch` | 绑定版本不是当前稳定版 |

| 单操作 `FailReason` | 含义 |
|---|---|
| `timeout` | 脉冲时长超过单操作超时上限 |
| `canceled` | 在途被上下文取消 |
| `skipped_dependency` | 上游失败导致的因果跳过 |
| `gate_error` | 确定性门噪声判定为错误（操作本身执行完成） |

---

## 4. 本地验证方法

要求 Go 1.25+（开发环境为 Go 1.26），无第三方依赖。

```bash
# 编译
go build ./...

# 全量单测（测试日志打印输入、调度决策与最终判定依据）
go test ./...

# 详细日志（以仿真包为例）
go test ./internal/simulator/ -v

# 并发安全 + 稳定性（重复执行、竞态检测）
go test -race -count=10 ./...

# 静态检查
go vet ./...

# 端到端演示：编译->调度->顺序/并发对比->过期拒绝->重校准->回滚历史复现
go run ./cmd/demo
```

测试覆盖矩阵：

| 场景 | 测试 |
|---|---|
| 参数/门非法、重复 ID、缺依赖 | `compiler.TestCompileRejectReasons` |
| 编译期校准过期拒绝 | `compiler.TestCompileRejectsExpiredCalibration` |
| 资源重叠 / 缺依赖 / 成环 / 依赖倒置 | `scheduler.TestValidateRejectReasons` |
| ASAP 无冲突、首尾相接允许 | `scheduler.TestAssignASAPAndNoOverlap` |
| 重排语义不变 + 空闲减少 + 可复现 | `scheduler.TestReplanPreservesSemanticsAndReducesIdle` |
| 容差拒绝、失败快照不可用、过期、回滚一致 | `calibration/*_test.go` |
| 顺序/并发等价、确定性 | `simulator.TestSequentialConcurrentEquivalence` 等 |
| 确定性超时 + 下游跳过 + 恰好一次 | `simulator.TestTimeoutDeterministic` |
| 确定性虚拟取消点 + 顺序/并发等价 | `simulator.TestVirtualCancelDeterministicAndEquivalent` |
| 外部 ctx 在途取消 + 恰好一次/因果不错位 | `simulator.TestExternalContextCancelExactlyOnce` |
| 超时场景跨模式等价 | `simulator.TestSequentialConcurrentEquivalenceUnderTimeout` |
| 端到端流水线与回滚历史复现 | `integration.TestPipelineCompileScheduleSimulateRollback` |

## 5. 范围与简化

这是**控制平面仿真**而非物理仿真：理想态演化用经典计算位玩具模型，噪声为离散的
哈希抽样，目的是让资源冲突、校准状态与并发因果**可解释、可区分、可复现**，
不代表真实量子振幅动力学。
