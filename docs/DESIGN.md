# 设计说明：编译调度模型、校准与回滚规则、并发语义、本地验证

本文档配合 README.md，描述本项目的四条主线：编译与调度、校准与回滚、
并发执行与聚合、以及可解释性与验证方法。所有时间单位统一为纳秒(ns)。

## 1. 编译与调度模型

### 1.1 层级

- **GateTask**（`internal/ir`）：高层任务，含 `ID / Kind / Qubits / DependsOn`。
- **Pulse**：低层脉冲，含 `ID / GateID / Kind(control|measure) / Resource /
  StartNS / DurationNS / Params`。占用区间为半开 `[StartNS, EndNS)`。
- **Schedule**：脉冲列表 + 门集合 + 依赖边集合。
- **Protocol**（`internal/compile`）：硬件描述，包括每比特独立控制资源
  `qN.ctrl`、共享测量资源 `measure.bus`、以及 GateKind→GateSpec 的展开规则。
  门时长由硬件规格固定；**校准只影响仿真噪声，不改变编译后时序**，因此编译
  结论不依赖校准状态即可复现。

### 1.2 拒绝规则与原因分类

编译入口 `Compile`（自由编译）与 `BindFixed`（校验用户显式绑定）返回的
`*compile.Error` 带明确 `Reason`：

| Reason | 触发条件 |
|---|---|
| `illegal-param` | 门 ID 空/重复、Kind 不支持、元数不符、比特越界/重复、脉冲 ID 重复、amp∉(0,1]、显式绑定时脉冲形状(资源/时长/参数)与规格不符、脉冲数不符 |
| `unsatisfiable-dependency` | 依赖引用不存在的门、自依赖、依赖成环；显式绑定时违反完成-开始契约（前驱最晚结束 > 后继最早开始） |
| `resource-conflict` | **仅 BindFixed**：同一资源上两个脉冲半开区间重叠，即使两门有依赖也拒绝（因为依赖是完成-开始关系，重叠本身非法） |

自由编译 `Compile` 不会产生资源冲突：调度器总会绕开已占用槽位（测量总线互斥
因此自动串行化）。冲突是对"我指定的这个布局行不行"的回答，故只在 `BindFixed`
中出现。端点相接 `[a,b)` 与 `[b,c)` 不算冲突。

### 1.3 确定性 ASAP 列表调度

1. 校验参数与依赖，Kahn 拓扑排序；就绪集合按**输入下标**破紧。
2. 按 GateSpec 展开每个门的脉冲（门内脉冲按 ID 排序，CNOT 两端脉冲可同时
   放置在各自资源上）。
3. 对拓扑序中的每个门，起点下界 = max(前驱门最晚结束)；门内每个脉冲在其
   `lower` 与资源已占用区间之间找最早可行槽（`earliestSlot`）。
4. 输出按 `(StartNS, PulseID)` 排序。

破紧规则完全确定 → 相同输入在任何机器上产出逐字段相同的计划。

### 1.4 重排保语义且减少空闲

`Reorder(s)` 重建拓扑序后用同一个 ASAP 算法重新放置脉冲，**只改 StartNS**。

- 语义判定 `SameSemantics(a,b)`：门集合相同、依赖边集合相同、每个门脉冲的
  multiset（资源/类型/参数，忽略时序与顺序）相同。
- 收益：`Schedule.TotalIdleNS()` 与 `MakespanNS()` 不会增大；演示中
  被人为推迟的测量从 320ns 压回 120ns。
- 稳定：连续两次 `Reorder` 结果 DeepEqual，不依赖 map 遍历顺序或 goroutine。

## 2. 校准与回滚规则

### 2.1 分层

- 参数两层：全局层资源键 `"*"` 与资源层（如 `q0.ctrl`）。执行时
  `CurrentFor(resource, qubit, now)` 先拷贝全局层再用资源层覆盖，返回有效参数。
- 读出保真度按比特独立存放。
- Store 的所有读取返回**拷贝**，外部修改无法污染存储。

### 2.2 生命周期与"不静默沿用"

- `Seed` 写入工厂稳定版（自动成为回滚锚点）。
- 版本带 `CreatedAt`；`now - CreatedAt > TTL` 即过期。`Current/CurrentFor`
  返回 `FailExpired`，**不返回旧值**。执行层捕获后把该 shot 判为
  `failed` 并写 `calibration-halt` 事件，绝不产出看似正常的测量比特。
- `Commit(candidate, now)` 在加锁状态下依次校验，任一失败都**不产生新版本**：
  1. `bad-value`：NaN/Inf/负数、amp∉(0,1]、保真度∉[0,1]、比特越界；
  2. `missing-key`：缺少必需参数键；
  3. `out-of-tolerance`：与**最近稳定版**在"合并后的有效参数"上逐键比较，
     绝对漂移超过容差（容差按参数键配置，如 amp:0.1, frequency:50）。
- 提交成功的版本为**非稳定**；`MarkStable` 提升为锚点（过期版本禁止提升）。
- `Rollback(now)` 把 head 恢复到上一稳定版。若该稳定版也已过期，回滚仍完成
  但同时返回 `FailExpired`——状态改变与告警分开表达，不掩盖问题。

### 2.3 回滚复现

历史结论通过 `AttachHistory(versionID, summary)` 与版本绑定。回滚后：

- 有效参数/保真度回到稳定版（有测试断言 merged amp/fid 等于基线）；
- `HistoryAt` 读回同一结论字符串；
- 由于噪声 PRNG 只依赖 `(seed, gateID, shot)` 且有效参数一致，重跑仿真
  每个 shot 的状态与测量比特都与历史一致（见 sim 确定性说明）。

### 2.4 并发取消/超时测试中观察到的重要不变量

- 失败提交绝不移动 head（测试断言 head 仍是旧稳定版）。
- 过期后可以基于稳定基线 `Commit` 容差内的新候选完成重新校准并刷新 head。

## 3. 并发语义

### 3.1 确定性的来源

每个 `(seed, gateID, shot)` 经 FNV + 混合函数派生独立 PCG 流（`gateStream`）。
顺序与并发模式调用**同一个** `runGate`，故对同一个门与 shot：

- 抽到的控制脉冲去极化随机数相同 → 成功/失败相同；
- 抽到的读出随机数相同（理想比特 1，`draw > fidelity` 翻转）→ 测量比特相同。

结论与 goroutine 交错、map 遍历、调度时序无关。`JobResult.Equivalent` 逐门比较
PerShot 序列、Attempts、聚合状态与多数票比特。

### 3.2 调度模型在执行层的对应

- 每个门一个 goroutine，门内 shot **严格串行**（一个 shot 绝不执行两次）。
- shot 级屏障 `waitDeps`：等所有依赖门的**同一 shot** 完成（完成-开始），
  对应编译器的依赖契约。
- 每门独立 `context.WithTimeout`：超时只归因于本门（`timed-out`），父 ctx
  取消级联全部门（`cancelled`）。

### 3.3 取消、超时、部分失败的计数规则

| 情形 | 本门该 shot | 依赖它的门（同一 shot） |
|---|---|---|
| 父 ctx 取消 | 尚未执行的 shot → `cancelled` | 屏障未满足 → `cancelled` |
| 本门超时 | `timed-out` | 依赖结果为 aborted → `cancelled`（不伪装成超时） |
| Hook 返回普通错误 | `failed` | **不**阻断：脉冲语义上已发射，下游照常执行 |
| 确定性噪声抽中 | `failed` | 同上，照常执行 |
| 校准过期/非法驱动 | `failed` + `calibration-halt`/`illegal-drive` 事件 | 同上 |

关键实现点：`waitDeps` 先非阻塞探测依赖槽，避免"依赖已完成且 ctx 同时取消"
时 Go 的 `select` 随机选中取消分支而错杀结果。每个 (门, shot) 的结果写入
**广播槽**（`shotSlot`：先写结果再 `close` 就绪通道）而非消费型通道：
一个门的结果同时喂给多个后继门（扇出）时，每个后继都能读到同一份结果，
`close` 建立的 happens-before 保证读者看到完整结果。**每门每 shot 恰好一个
结果**：没有重复计数，也没有遗漏；分支/汇合/扇出拓扑都能正常结束。

### 3.5 可续跑执行（断点续跑）

长时间批跑可能跑到一半被取消或超时。`RunResumable` 除返回聚合结果外，还返回
一个 **Checkpoint**，把中断现场完整带走：

- **已完成的部分**：每个 (门, shot) 的终态结果（`ok`/`measured`/`failed`，
  含测量比特），标记 `Final=true`；
- **还没开始/没跑完的部分**：`cancelled`/`timed-out` 的 shot，标记
  `Final=false`——与真正的执行失败（`failed`，终态）严格区分；
- **身份指纹**：JobID、脉冲计划拓扑与参数指纹（`ScheduleFP`）、Shots、Seed、
  中断时生效校准版本的 ID 与有效参数指纹（`CalibrationFP`）。

`Resume(job, checkpoint, policy, concurrent)` 的续跑流程：

1. **作业身份校验**：JobID / ScheduleFP / Shots / Seed 任一不一致，返回
   `*ResumeError{Reason: resume-mismatch-job}`，绝不执行——拿错作业或对不上
   的状态会被明确挡掉，而不是算出一个看似正常的结果。
2. **校准兼容性校验**：当前校准版本必须未过期，且有效参数指纹与中断点一致
   （指纹按有效参数逐键计算，不含版本 ID）。因此：
   - 校准未变 → 兼容；
   - 中断期间**回滚到同一个稳定版本** → 有效参数逐键相同 → 兼容，续跑结论
     与最早那次一次性跑完的结果一致；
   - 过期、回滚到不同的稳定版、或提交了参数不同的新稳定版 → 不兼容。
3. **兼容**：复用全部 `Final=true` 的结果（每个复用的 shot 记一条 `reuse`
   事件），只为 `Final=false` 的 (门, shot) 调用执行核。由于随机数只依赖
   `(seed, gateID, shot)` 且有效参数一致，合并后的每 shot 状态、测量比特、
   聚合判定与计数与一次性不间断跑完**逐项一致**，不多算也不漏算。
4. **不兼容**：`ResumeStrict` 返回
   `*ResumeError{Reason: resume-incompatible-calibration}`，旧新结果绝不拼接；
   `ResumeRecompute` 丢弃检查点中的全部旧结果整体重跑（`Reused=0`），结论
   等于当前校准下的全新运行。两种处置都写入 `resume-decision` 事件，日志能
   看出是拒绝还是重算、以及原因（过期还是参数漂移）。

顺序与并发模式共用同一条复用通路：并发模式下复用结果同样通过广播槽发布，
下游屏障与扇出语义不变。`JobResult.Reused/Recomputed` 给出本次复用与重算的
(门, shot) 计数；`Resume` 返回的新检查点合并了全量状态，可再次用于续跑。

### 3.4 因果事件日志

`recorder` 用互斥锁 + 原子自增 Seq 记录全部事件（complete / pulse-fail /
measure / calibration-halt / illegal-drive / skip / not-started）。
并发模式下事件交错顺序可能变化，但因果先后（依赖门终结事件 Seq < 后继门
首个事件 Seq）由屏障保证，测试逐 shot 校验。

## 4. 可解释性与本地验证

所有测试都用 `t.Logf` 打印三段信息：

1. **输入**：门 ID/Kind/Qubits/依赖（或候选参数、now、TTL）；
2. **调度决策**：每个脉冲的资源与 `[start,end)`、makespan、idle；
3. **最终判定依据**：接受/拒绝、分类 Reason、各 shot 状态、计数、关键事件。

```bash
go test ./... -v            # 查看全部输入/决策/判定日志
go test -race -count=5 ./internal/sim/   # 并发不变量 + 竞态
go run ./cmd/qce            # 端到端演示
```

## 5. 测试覆盖矩阵

| 场景 | 测试 |
|---|---|
| 并行/依赖调度、测量总线串行 | `TestCompileBasicParallelAndDep`, `TestCompileMeasureBusConflict` |
| 资源重叠拒绝 | `TestBindFixedConflictRejected` |
| 违反完成-开始依赖拒绝 | `TestBindFixedDepViolationRejected` |
| 非法参数/坏依赖/环/重复 ID | `TestIllegalParamsRejected` |
| 重排保语义、减空闲、稳定可复现 | `TestReorderStableAndSemanticPreserving` |
| 编译确定性 | `TestDeterministicCompile` |
| 过期拒绝静默沿用 | `TestExpiredRejectsSilentUse` |
| 分层合并 | `TestLayeredMerge` |
| 校准失败分类且 head 不动 | `TestCommitFailuresAreClassified` |
| 回滚 + 历史复现 | `TestRollbackReproducesHistory` |
| 无稳定版回滚 | `TestRollbackNoStable` |
| 过期后重新校准 | `TestRecalibrateAfterExpiry` |
| 顺序/并发等价、不重不漏、因果序 | `TestSequentialConcurrentEquivalent` |
| 并发取消 | `TestCancellationNoDoubleCount` |
| 超时与取消可区分 | `TestTimeoutClassifiedDistinctly` |
| 部分失败不遗漏 | `TestPartialPulseFailureNoOmission` |
| 过期校准执行中止 | `TestExpiredCalibrationHaltsNotSilent` |
| 普通硬件故障归类 failed | `TestHookGenericFailureIsFailed` |
| 复杂拓扑（分支/汇合/扇出）顺序≡并发、正常结束 | `TestComplexTopologySequentialConcurrentEquivalent` |
| 开始/中间/接近结束时取消后的续跑（顺序+并发） | `TestResumeAfterCancelAtStartMiddleEnd` |
| 超时中断后续跑；failed 终态与 cancelled/timed-out 待重算可区分 | `TestResumeAfterTimeoutConcurrent` |
| 新稳定版/过期 → 续跑拒绝或整体重算；回滚到同一稳定版 → 兼容且结论一致 | `TestResumeRejectedOnCalibrationChange` |
| 拿错作业/对不上的状态续跑被明确拒绝 | `TestResumeRejectsMismatchedJob` |
