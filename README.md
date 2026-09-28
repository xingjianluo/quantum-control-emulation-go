# quantum-control-emulation-go

本地量子控制**编译 → 调度 → 校准 → 并发仿真 → 结果聚合**的确定性 Go 实现。
不依赖任何外部服务或硬件，所有结论都可解释、可复现。

## 目录结构

```
internal/ir           高层门任务 / 脉冲计划中间表示（时间单位：纳秒）
internal/compile      硬件协议、门→脉冲展开、ASAP 调度、冲突/参数/依赖拒绝、保语义重排
internal/calibration  校准参数与读出保真度的分层管理、TTL 过期、容差校验、回滚、历史结论
internal/sim          确定性噪声模型、顺序/并发执行、取消/超时/部分失败处理、断点续跑、结果聚合
cmd/qce               端到端演示命令
docs/                 设计与规则文档
```

## 一条命令体验

```bash
go run ./cmd/qce
```

它依次演示：编译调度、保语义重排压缩空闲、顺序/并发等价、校准过期拒绝、
回滚复现历史、容差内重新校准恢复、中断后断点续跑。

## 本地验证

```bash
# 全部单元测试（资源冲突 / 过期校准 / 回滚 / 并发取消 / 超时 / 部分失败）
go test ./...

# 带竞态检测、多次重复（并发正确性）
go test -race -count=5 ./internal/sim/

# 查看带输入、调度决策与判定依据的详细日志
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
  因果不可达判取消；事件带单调 `Seq` 的因果日志；一个结果喂给多个后继门
  （扇出）时经广播槽发布，分支/汇合/扇出拓扑都能正常结束。
- **断点续跑**：`RunResumable` 返回的检查点带走已完成的终态结果与未开始的
  部分（`cancelled`/`timed-out`，与真正失败 `failed` 严格区分），并快照中断时
  本作业实际用到的有效校准参数与读出保真度。`Resume` 先校验作业身份
  （`resume-mismatch-job`）；当前校准过期或不可用时，**无论 strict 还是
  recompute 都明确拒绝**（`resume-calibration-unavailable`），绝不拼出一份整体
  失败的“正常”结果；校准可用但参数已变时，strict 拒绝
  （`resume-calibration-changed`），recompute **只重算受影响的门**（全局参数
  变→全部门；某控制资源变→拥有该资源脉冲的门；某比特保真度变→该比特的测量
  门），其余已完成结果继续复用，合并结论与当前校准下一次不间断跑完逐项一致。
  回滚到同一稳定版本、换版本但有效参数相同、或只改作业不使用的资源时，全部
  复用。复用与重算的 shot 在事件日志中可逐条区分，拒绝原因互不重叠。
