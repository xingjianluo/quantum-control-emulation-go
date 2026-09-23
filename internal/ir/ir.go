// Package ir 定义高层量子门任务与低层脉冲计划之间的中间表示。
//
// 整个仿真流程中，时间单位统一为纳秒(ns)。脉冲计划是不可变语义的载体：
// 编译产出后，重排只能改变时序与资源布局，不能改变门集合、门参数与依赖。
package ir

import "fmt"

// PulseKind 表示脉冲的硬件类型。
type PulseKind int

const (
	// PulseControl 是作用于量子比特的控制脉冲。
	PulseControl PulseKind = iota
	// PulseMeasure 是读出脉冲，占用共享的测量资源。
	PulseMeasure
)

// String 返回脉冲类型的可读名称。
func (k PulseKind) String() string {
	switch k {
	case PulseControl:
		return "control"
	case PulseMeasure:
		return "measure"
	default:
		return fmt.Sprintf("pulse-kind-%d", int(k))
	}
}

// GateKind 表示高层量子门种类。
type GateKind int

const (
	GateX GateKind = iota
	GateY
	GateZ
	GateH
	GateCNOT
	GateMeasure
)

// String 返回门种类的可读名称。
func (k GateKind) String() string {
	switch k {
	case GateX:
		return "X"
	case GateY:
		return "Y"
	case GateZ:
		return "Z"
	case GateH:
		return "H"
	case GateCNOT:
		return "CNOT"
	case GateMeasure:
		return "MEASURE"
	default:
		return fmt.Sprintf("gate-kind-%d", int(k))
	}
}

// GateTask 是一个高层量子门任务。
//
// DependsOn 声明语义依赖：被依赖的门必须完全结束后，本门的首个脉冲才能
// 开始。ID 必须在同一作业内唯一。
type GateTask struct {
	ID        string
	Kind      GateKind
	Qubits    []int
	DependsOn []string
}

// Pulse 是编译后计划中的一个脉冲。
//
// 同一 Resource 上 [StartNS, StartNS+DurationNS) 区间重叠的两个脉冲构成
// 资源冲突，除非它们所属门之间存在依赖关系（依赖天然决定先后）。
type Pulse struct {
	ID         string
	GateID     string
	Kind       PulseKind
	Resource   string
	StartNS    int64
	DurationNS int64
	// Params 为脉冲参数（amp 等），编译器按门规格填充。
	Params map[string]float64
}

// EndNS 返回脉冲的排他结束时刻（半开区间）。
func (p Pulse) EndNS() int64 { return p.StartNS + p.DurationNS }

// Overlaps 判断两个脉冲在同一资源上是否时间重叠（半开区间，端点相接不冲突）。
func (p Pulse) Overlaps(q Pulse) bool {
	return p.Resource == q.Resource && p.StartNS < q.EndNS() && q.StartNS < p.EndNS()
}

// Schedule 是一个带时序与依赖的脉冲计划。
type Schedule struct {
	GateIDs  []string
	Pulses   []Pulse
	GateDeps map[string][]string
	// gatePulses 记录每个门拥有的脉冲 ID 顺序，重排时用于保语义校验。
	gatePulseIDs map[string][]string
}

// GatePulseIDs 返回某门包含的脉冲 ID（拷贝）。
func (s *Schedule) GatePulseIDs(gateID string) []string {
	out := make([]string, len(s.gatePulseIDs[gateID]))
	copy(out, s.gatePulseIDs[gateID])
	return out
}

// SetGatePulseIDs 记录门与脉冲的归属关系（由编译器使用）。
func (s *Schedule) SetGatePulseIDs(gateID string, ids []string) {
	if s.gatePulseIDs == nil {
		s.gatePulseIDs = make(map[string][]string)
	}
	cp := make([]string, len(ids))
	copy(cp, ids)
	s.gatePulseIDs[gateID] = cp
}

// MakespanNS 返回计划总时长（最晚脉冲结束时刻）。
func (s *Schedule) MakespanNS() int64 {
	var end int64
	for _, p := range s.Pulses {
		if p.EndNS() > end {
			end = p.EndNS()
		}
	}
	return end
}

// TotalIdleNS 返回所有资源上的空闲等待总和，用于衡量重排收益。
func (s *Schedule) TotalIdleNS() int64 {
	byResource := make(map[string][]Pulse)
	for _, p := range s.Pulses {
		byResource[p.Resource] = append(byResource[p.Resource], p)
	}
	var idle int64
	for _, ps := range byResource {
		if len(ps) == 0 {
			continue
		}
		var first, last int64 = ps[0].StartNS, ps[0].EndNS()
		var busy int64
		for _, p := range ps {
			if p.StartNS < first {
				first = p.StartNS
			}
			if p.EndNS() > last {
				last = p.EndNS()
			}
			busy += p.DurationNS
		}
		idle += (last - first) - busy
	}
	return idle
}
