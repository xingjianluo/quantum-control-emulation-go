# quantum-control-emulation-go

本地量子控制**编译 → 调度 → 校准 → 并发仿真 → 结果聚合**的确定性 Go 实现。
不依赖任何外部服务或硬件，所有结论都可解释、可复现。

## 目录结构

```
internal/ir           高层门任务 / 脉冲计划中间表示（时间单位：纳秒）
internal/compile      硬件协议、门→脉冲展开、ASAP 调度、冲突/参数/依赖拒绝、保语义重排
internal/calibration  校准参数与读出保真度的分层管理、TTL 过期、容差校验、回滚、历史结论、版本差异比较
internal/sim          确定性噪声模型、顺序/并发执行、取消/超时/部分失败处理、断点续跑、结果聚合
cmd/qce               端到端演示命令
docs/                 设计与规则文档
```

## 一条命令体验

```bash
go run ./cmd/qce
```

它依次演示：编译调度、保语义重排压缩空闲、顺序/并发等价、校准过期拒绝、
回滚复现历史、容差内重新校准恢复、中断后从断点续跑且结论与一次性跑完一致。

## 本地验证

```bash
# 全部单元测试（资源冲突 / 过期校准 / 回滚 / 并发取消 / 超时 / 部分失败 / 断点续跑）
go test ./...

# 带竞态检测、多次重复（并发正确性）
go test -race -count=5 ./internal/sim/

# 查看带输入、续跑决策与判定依据的详细日志
go test ./... -v
```

测试均为确定性测试：相同输入在任何机器、任何运行次数下结论一致。

## 核心保证（详见 docs/DESIGN.md）

- **拒绝原因可区分**：编译错误分为 `resource-conflict` / `illegal-param` /
  `unsatisfiable-dependency`；校准错误分为 `calibration-expired` /
  `out-of-tolerance` / `bad-value` / `missing-key` / `no-stable-version`。
- **重排保语义**：`compile.Reorder` 只重排时序，门集合、依赖边、每个门脉冲的
  资源/时长/参数不变（`SameSemantics` 校验），且使用与编译相同的确定性 ASAP
  算法压缩空闲等待，结果跨机器稳定。
- **不静默沿用旧校准**：超过 TTL 后 `Current/CurrentFor` 直接返回错误，执行层
  把对应 shot 判为失败并记录 `calibration-halt` 事件。
- **回滚即复现**：回滚到上一稳定版本后，有效参数/保真度与该版本完全一致，
  与版本绑定的历史结论可读，仿真可逐 shot 复现。
- **顺序≡并发**：随机数只依赖 `(seed, gateID, shot)`，两种执行模式共用同一
  执行核，聚合报告（每 shot 状态、多数票比特、尝试次数）完全相同。
- **取消/超时不重不漏**：每门每 shot 恰好一个结果；超时按门归因、下游因
  因果不可达判取消；事件带单调 `Seq` 的因果日志。完成信号采用关闭-广播，
  分支/汇合/一对多（fan-out）的复杂依赖拓扑都能正常结束。
- **断点续跑**：作业被取消/超时打断后，`JobResult.Checkpoint` 带回已完成与
  未完成的部分；`ResumeSequential`/`ResumeConcurrent` 复用已完成的
  (门, shot) 结果（日志记 `reuse` 事件）、只重算未完成部分，最终结论与
  一次性跑完逐项一致。续跑前强制校验校准兼容性：版本未变（含回滚到同一
  稳定版）直接续跑；版本已变可严格拒绝（`calibration-changed`）或只重算
  受影响的门；校准过期、断点版本丢失、作业指纹不符分别以
  `calibration-expired` / `calibration-version-lost` / `checkpoint-mismatch`
  明确拒绝，绝不把新旧结果悄悄拼在一起（详见 docs/DESIGN.md §3.5）。
