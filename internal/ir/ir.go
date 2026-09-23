// Package ir 定义量子控制编译/调度/仿真流水线中各阶段共享的中间表示：
// 高层门任务 (GateTask)、带时序和依赖的脉冲操作 (PulseOp) 以及脉冲计划 (PulseProgram)。
package ir

import "time"

// ResourceKind 标识硬件资源的类别。
type ResourceKind string

const (
	ResourceQubit   ResourceKind = "qubit"   // 量子比特（驱动/量子门通道）
	ResourceCoupler ResourceKind = "coupler" // 耦合器（两比特门通道）
	ResourceReadout ResourceKind = "readout" // 读出通道
)

// Resource 是调度器眼里的一个硬件资源。
type Resource struct {
	Kind ResourceKind
	Name string // 如 qubit-0、coupler-0-1、readout-0
}

// GateKind 是高层门任务的类型。
type GateKind string

const (
	GateX    GateKind = "X"
	GateX90  GateKind = "X90"
	GateRZ   GateKind = "RZ"
	GateCNOT GateKind = "CNOT"
	GateMeas GateKind = "MEASURE"
)

// GateTask 是用户提交的一层高层门任务。
type GateTask struct {
	ID        string
	Kind      GateKind
	Qubits    []int    // 作用比特；CNOT 为 [control, target]，MEASURE 为被测比特
	Angle     float64  // RZ 等参数门使用，单位 rad
	DependsOn []string // 依赖的 GateTask ID
}

// OpKind 是编译后底层脉冲操作的类型。
type OpKind string

const (
	OpPulse   OpKind = "PULSE"   // 在驱动/耦合通道上施加脉冲
	OpReadout OpKind = "READOUT" // 在读出通道上测量
	OpIdling  OpKind = "IDLE"    // 显式空闲等待（如缓冲/松弛）
)

// PulseOp 是脉冲计划中的一条操作。
type PulseOp struct {
	ID        string
	GateID    string // 来源高层门 ID（可解释性）
	Kind      OpKind
	Resources []Resource
	Duration  time.Duration
	// Params 保存脉冲形状参数（幅度、频率等），由编译期由校准数据填充。
	Params map[string]float64
	// Deps 以 PulseOp ID 表达偏序依赖；调度器必须保证依赖先完成。
	Deps []string
	// Start 由调度器填写，相对计划 t=0 的开始时间。
	Start time.Duration
	// CalibVersion 记录编译时绑定的校准快照版本，执行期据此检测过期。
	CalibVersion int
}

// End 返回 Start+Duration。
func (o *PulseOp) End() time.Duration { return o.Start + o.Duration }

// PulseProgram 是一份带完整时序的脉冲计划。
type PulseProgram struct {
	Ops []*PulseOp
	// CalibVersion 是整份计划编译时所绑定的校准快照版本。
	CalibVersion int
}

// Makespan 返回计划最晚结束时间；空计划为 0。
func (p *PulseProgram) Makespan() time.Duration {
	var end time.Duration
	for _, op := range p.Ops {
		if e := op.End(); e > end {
			end = e
		}
	}
	return end
}
