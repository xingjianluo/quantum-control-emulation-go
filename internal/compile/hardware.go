package compile

import (
	"fmt"

	"github.com/xingjianluo/quantum-control-emulation-go/internal/ir"
)

// GateSpec 描述一种门在硬件上的脉冲展开规则。
//
// Build 接收门任务，产出该门的脉冲（不含时序，StartNS 由编译器决定）。
// 脉冲的 Resource 与 Params 在此确定，DurationNS 也在此固定——本仿真中
// 门时长由硬件规格决定，校准参数只影响仿真噪声，不改变编译后的时序，
// 从而保证“编译结论”不依赖校准状态即可复现。
type GateSpec struct {
	Kind       ir.GateKind
	Arity      int
	DurationNS int64
	Build      func(task ir.GateTask) []ir.Pulse
}

// Protocol 描述硬件资源与门规格。
//
// 每个量子比特有独立的控制资源（形如 q0.ctrl），所有比特共享一个测量资源
// （measure.bus），因此两个测量门即使作用于不同比特也不能并发——这是
// 冲突拒绝规则的重要来源。
type Protocol struct {
	Qubits           int
	ControlResources map[int]string
	MeasureResource  string
	GateSpecs        map[ir.GateKind]GateSpec
}

// Validate 校验硬件协议本身是否合法。
func (p *Protocol) Validate() error {
	if p.Qubits <= 0 {
		return fmt.Errorf("protocol: qubit count must be positive, got %d", p.Qubits)
	}
	if p.MeasureResource == "" {
		return fmt.Errorf("protocol: measure resource is empty")
	}
	for q := 0; q < p.Qubits; q++ {
		if p.ControlResources[q] == "" {
			return fmt.Errorf("protocol: qubit %d has no control resource", q)
		}
	}
	for k, spec := range p.GateSpecs {
		if spec.Kind != k {
			return fmt.Errorf("protocol: gate spec key %s mismatches kind %s", k, spec.Kind)
		}
		if spec.Arity <= 0 {
			return fmt.Errorf("protocol: gate %s has non-positive arity", k)
		}
		if spec.DurationNS <= 0 {
			return fmt.Errorf("protocol: gate %s has non-positive duration", k)
		}
		if spec.Build == nil {
			return fmt.Errorf("protocol: gate %s has no pulse builder", k)
		}
	}
	return nil
}

// DefaultProtocol 返回一个内置的确定性硬件协议。
func DefaultProtocol(qubits int) *Protocol {
	p := &Protocol{
		Qubits:           qubits,
		ControlResources: make(map[int]string, qubits),
		MeasureResource:  "measure.bus",
		GateSpecs:        make(map[ir.GateKind]GateSpec),
	}
	for q := 0; q < qubits; q++ {
		p.ControlResources[q] = fmt.Sprintf("q%d.ctrl", q)
	}
	ctrl := func(task ir.GateTask) []ir.Pulse {
		res := p.ControlResources[task.Qubits[0]]
		return []ir.Pulse{{
			ID: task.ID + ":p0", GateID: task.ID, Kind: ir.PulseControl,
			Resource: res, Params: map[string]float64{"amp": 0.5},
		}}
	}
	p.GateSpecs[ir.GateX] = GateSpec{Kind: ir.GateX, Arity: 1, DurationNS: 20, Build: ctrl}
	p.GateSpecs[ir.GateY] = GateSpec{Kind: ir.GateY, Arity: 1, DurationNS: 20, Build: ctrl}
	p.GateSpecs[ir.GateZ] = GateSpec{Kind: ir.GateZ, Arity: 1, DurationNS: 10, Build: ctrl}
	p.GateSpecs[ir.GateH] = GateSpec{Kind: ir.GateH, Arity: 1, DurationNS: 30, Build: ctrl}
	p.GateSpecs[ir.GateCNOT] = GateSpec{
		Kind: ir.GateCNOT, Arity: 2, DurationNS: 60,
		Build: func(task ir.GateTask) []ir.Pulse {
			return []ir.Pulse{{
				ID: task.ID + ":p0", GateID: task.ID, Kind: ir.PulseControl,
				// 两比特门同时占用控制端与靶端资源。
				Resource: p.ControlResources[task.Qubits[0]],
				Params:   map[string]float64{"amp": 0.7},
			}, {
				ID: task.ID + ":p1", GateID: task.ID, Kind: ir.PulseControl,
				Resource: p.ControlResources[task.Qubits[1]],
				Params:   map[string]float64{"amp": 0.7},
			}}
		},
	}
	p.GateSpecs[ir.GateMeasure] = GateSpec{
		Kind: ir.GateMeasure, Arity: 1, DurationNS: 40,
		Build: func(task ir.GateTask) []ir.Pulse {
			return []ir.Pulse{{
				ID: task.ID + ":m0", GateID: task.ID, Kind: ir.PulseMeasure,
				Resource: p.MeasureResource,
				// qubit 随脉冲携带，仿真执行时据此取该比特的读出保真度。
				Params: map[string]float64{"amp": 1.0, "qubit": float64(task.Qubits[0])},
			}}
		},
	}
	return p
}
