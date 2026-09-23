package scheduler

import (
	"testing"
	"time"

	"github.com/xingjianluo/quantum-control-emulation-go/internal/ir"
)

func qubitRes(q int) ir.Resource {
	return ir.Resource{Kind: ir.ResourceQubit, Name: "qubit-" + itoa(q)}
}
func readoutRes(q int) ir.Resource {
	return ir.Resource{Kind: ir.ResourceReadout, Name: "readout-" + itoa(q)}
}

func op(id string, kind ir.OpKind, dur time.Duration, res []ir.Resource, deps ...string) *ir.PulseOp {
	return &ir.PulseOp{ID: id, GateID: id, Kind: kind, Resources: res, Duration: dur, Deps: deps}
}

func TestAssignASAPAndNoOverlap(t *testing.T) {
	s := New()
	prog := &ir.PulseProgram{Ops: []*ir.PulseOp{
		op("a", ir.OpPulse, 10*time.Nanosecond, []ir.Resource{qubitRes(0)}),
		op("b", ir.OpPulse, 10*time.Nanosecond, []ir.Resource{qubitRes(1)}),
		op("c", ir.OpPulse, 10*time.Nanosecond, []ir.Resource{qubitRes(0)}, "a"),
		op("m", ir.OpReadout, 20*time.Nanosecond, []ir.Resource{readoutRes(0)}, "c"),
	}}
	if err := s.Assign(prog); err != nil {
		t.Fatal(err)
	}
	starts := map[string]time.Duration{}
	for _, o := range prog.Ops {
		starts[o.ID] = o.Start
	}
	t.Logf("调度决策:")
	for _, d := range s.Decisions() {
		t.Logf("  %s @%s (%s)", d.OpID, d.Start, d.Why)
	}
	if starts["a"] != 0 || starts["b"] != 0 {
		t.Fatalf("无依赖操作应从 0 开始: %+v", starts)
	}
	if starts["c"] != 10*time.Nanosecond {
		t.Fatalf("c 必须在 a 释放 qubit-0 后开始，got %s", starts["c"])
	}
	if starts["m"] != 20*time.Nanosecond {
		t.Fatalf("m 应在 c 完成后开始，got %s", starts["m"])
	}
	if err := s.Validate(prog); err != nil {
		t.Fatalf("调度结果必须自洽: %v", err)
	}
}

func TestAssignDeterminism(t *testing.T) {
	build := func() *ir.PulseProgram {
		return &ir.PulseProgram{Ops: []*ir.PulseOp{
			op("z", ir.OpPulse, 10, []ir.Resource{qubitRes(0)}),
			op("a", ir.OpPulse, 10, []ir.Resource{qubitRes(1)}),
			op("m", ir.OpPulse, 10, []ir.Resource{qubitRes(2)}),
		}}
	}
	s := New()
	p1 := build()
	p2 := build()
	if err := s.Assign(p1); err != nil {
		t.Fatal(err)
	}
	if err := s.Assign(p2); err != nil {
		t.Fatal(err)
	}
	for i := range p1.Ops {
		if p1.Ops[i].Start != p2.Ops[i].Start {
			t.Fatalf("相同输入必须得到相同排程: %d vs %d", p1.Ops[i].Start, p2.Ops[i].Start)
		}
	}
}

func TestValidateRejectReasons(t *testing.T) {
	s := New()
	var err error

	// 1) 缺失依赖
	missing := &ir.PulseProgram{Ops: []*ir.PulseOp{
		op("a", ir.OpPulse, 10, []ir.Resource{qubitRes(0)}, "ghost"),
	}}
	err = s.Validate(missing)
	if !IsError(err, ReasonMissingDep) {
		t.Fatalf("期望 missing_dependency，得到 %v", err)
	}
	t.Logf("缺失依赖 -> 拒绝: %v", err)

	// 2) 成环
	cyclic := &ir.PulseProgram{Ops: []*ir.PulseOp{
		op("a", ir.OpPulse, 10, []ir.Resource{qubitRes(0)}, "b"),
		op("b", ir.OpPulse, 10, []ir.Resource{qubitRes(1)}, "a"),
	}}
	err = s.Validate(cyclic)
	if !IsError(err, ReasonCyclicDeps) {
		t.Fatalf("期望 cyclic_dependencies，得到 %v", err)
	}
	t.Logf("依赖成环 -> 拒绝: %v", err)

	// 3) 资源重叠
	overlap := &ir.PulseProgram{Ops: []*ir.PulseOp{
		op("a", ir.OpPulse, 10, []ir.Resource{qubitRes(0)}),
		op("b", ir.OpPulse, 10, []ir.Resource{qubitRes(0)}),
	}}
	overlap.Ops[0].Start = 0
	overlap.Ops[1].Start = 5
	err = s.Validate(overlap)
	if !IsError(err, ReasonResourceBusy) {
		t.Fatalf("期望 resource_overlap，得到 %v", err)
	}
	t.Logf("资源重叠 [0,10) vs [5,15) -> 拒绝: %v", err)

	// 4) 首尾相接允许（半开区间）
	adjacent := &ir.PulseProgram{Ops: []*ir.PulseOp{
		op("a", ir.OpPulse, 10, []ir.Resource{qubitRes(0)}),
		op("b", ir.OpPulse, 10, []ir.Resource{qubitRes(0)}, "a"),
	}}
	adjacent.Ops[0].Start = 0
	adjacent.Ops[1].Start = 10
	if err := s.Validate(adjacent); err != nil {
		t.Fatalf("首尾相接不应视为冲突: %v", err)
	}
	t.Log("窗口 [0,10) 与 [10,20) 首尾相接 -> 接受")

	// 5) 依赖倒置
	inv := &ir.PulseProgram{Ops: []*ir.PulseOp{
		op("a", ir.OpPulse, 10, []ir.Resource{qubitRes(0)}, "b"),
		op("b", ir.OpPulse, 10, []ir.Resource{qubitRes(1)}),
	}}
	inv.Ops[0].Start = 0
	inv.Ops[1].Start = 5
	err = s.Validate(inv)
	if !IsError(err, ReasonDepOrder) {
		t.Fatalf("期望 dependency_after_start，得到 %v", err)
	}
	t.Logf("依赖 b 晚于 a 开始 -> 拒绝: %v", err)
}

func TestReplanPreservesSemanticsAndReducesIdle(t *testing.T) {
	s := New()
	// 手工构造一个带空洞的合法计划：
	// a@[0,10) 用 qubit-0；b@[20,30) 用 qubit-1；c@[30,40) 用 qubit-0 且依赖 a。
	prog := &ir.PulseProgram{CalibVersion: 1, Ops: []*ir.PulseOp{
		op("a", ir.OpPulse, 10, []ir.Resource{qubitRes(0)}),
		op("b", ir.OpPulse, 10, []ir.Resource{qubitRes(1)}),
		op("c", ir.OpPulse, 10, []ir.Resource{qubitRes(0)}, "a"),
	}}
	prog.Ops[0].Start = 0
	prog.Ops[1].Start = 20
	prog.Ops[2].Start = 30
	if err := s.Validate(prog); err != nil {
		t.Fatal(err)
	}
	before := prog.Makespan()
	idleBefore := TotalIdle(prog)

	np, err := s.Replan(prog)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(np); err != nil {
		t.Fatalf("重排结果必须合法: %v", err)
	}
	if !semanticallyEqual(prog, np) {
		t.Fatal("重排改变了语义（操作/时长/参数/依赖不一致）")
	}
	if np.Makespan() > before {
		t.Fatalf("重排不得延长 makespan: before=%s after=%s", before, np.Makespan())
	}
	if TotalIdle(np) >= idleBefore {
		t.Fatalf("重排应压实空闲: before=%s after=%s", idleBefore, TotalIdle(np))
	}
	t.Logf("重排前 makespan=%s idle=%s；重排后 makespan=%s idle=%s；语义保持一致",
		before, idleBefore, np.Makespan(), TotalIdle(np))
	for _, d := range s.Decisions() {
		t.Logf("  重排决策 %s @%s (%s)", d.OpID, d.Start, d.Why)
	}
}

// semanticallyEqual 比较语义要素：ID、门来源、类型、时长、参数、依赖集合；允许 Start 不同。
func semanticallyEqual(a, b *ir.PulseProgram) bool {
	if len(a.Ops) != len(b.Ops) {
		return false
	}
	am := map[string]*ir.PulseOp{}
	for _, o := range a.Ops {
		am[o.ID] = o
	}
	for _, bo := range b.Ops {
		ao, ok := am[bo.ID]
		if !ok {
			return false
		}
		if ao.GateID != bo.GateID || ao.Kind != bo.Kind || ao.Duration != bo.Duration ||
			ao.CalibVersion != bo.CalibVersion {
			return false
		}
		if len(ao.Deps) != len(bo.Deps) {
			return false
		}
		dm := map[string]bool{}
		for _, d := range ao.Deps {
			dm[d] = true
		}
		for _, d := range bo.Deps {
			if !dm[d] {
				return false
			}
		}
		if len(ao.Params) != len(bo.Params) {
			return false
		}
		for k, v := range ao.Params {
			if bo.Params[k] != v {
				return false
			}
		}
	}
	return true
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
