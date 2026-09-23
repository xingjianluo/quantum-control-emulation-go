package compile

import (
	"reflect"
	"testing"

	"github.com/xingjianluo/quantum-control-emulation-go/internal/ir"
)

func logSchedule(t *testing.T, name string, gates []ir.GateTask, s *ir.Schedule, err error) {
	t.Helper()
	t.Logf("[%s] input gates:", name)
	for _, g := range gates {
		t.Logf("  gate id=%s kind=%s qubits=%v deps=%v", g.ID, g.Kind, g.Qubits, g.DependsOn)
	}
	if err != nil {
		t.Logf("[%s] verdict: REJECTED reason=%s detail=%v", name, err.(*Error).Reason, err)
		return
	}
	t.Logf("[%s] scheduling decision (makespan=%d idle=%d):", name, s.MakespanNS(), s.TotalIdleNS())
	for _, p := range s.Pulses {
		t.Logf("  pulse %-8s gate=%-4s res=%-12s [%3d,%3d) params=%v",
			p.ID, p.GateID, p.Resource, p.StartNS, p.EndNS(), p.Params)
	}
	t.Logf("[%s] verdict: ACCEPTED", name)
}

func TestCompileBasicParallelAndDep(t *testing.T) {
	c, _ := NewCompiler(DefaultProtocol(3))
	gates := []ir.GateTask{
		{ID: "g1", Kind: ir.GateX, Qubits: []int{0}},
		{ID: "g2", Kind: ir.GateX, Qubits: []int{1}},
		{ID: "g3", Kind: ir.GateX, Qubits: []int{0}, DependsOn: []string{"g1"}},
	}
	s, err := c.Compile(gates)
	logSchedule(t, "basic", gates, s, err)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	// g1/g2 在不同控制资源上应并行(同为 0 起点)。
	start := map[string]int64{}
	for _, p := range s.Pulses {
		start[p.ID] = p.StartNS
	}
	if start["g1:p0"] != 0 || start["g2:p0"] != 0 {
		t.Fatalf("g1,g2 should start at 0, got %d,%d", start["g1:p0"], start["g2:p0"])
	}
	if start["g3:p0"] != 20 {
		t.Fatalf("g3 must start after g1 ends(20), got %d", start["g3:p0"])
	}
}

func TestCompileMeasureBusConflict(t *testing.T) {
	c, _ := NewCompiler(DefaultProtocol(3))
	// 两个独立测量共享 measure.bus：自由编译会自动串行化，不会冲突。
	gates := []ir.GateTask{
		{ID: "m1", Kind: ir.GateMeasure, Qubits: []int{0}},
		{ID: "m2", Kind: ir.GateMeasure, Qubits: []int{1}},
	}
	s, err := c.Compile(gates)
	logSchedule(t, "measure-bus", gates, s, err)
	if err != nil {
		t.Fatalf("free compile should serialize measures, got %v", err)
	}
	var a, b ir.Pulse
	for _, p := range s.Pulses {
		if p.ID == "m1:m0" {
			a = p
		}
		if p.ID == "m2:m0" {
			b = p
		}
	}
	if a.Overlaps(b) {
		t.Fatalf("measures must not overlap: [%d,%d) vs [%d,%d)", a.StartNS, a.EndNS(), b.StartNS, b.EndNS())
	}
}

func TestBindFixedConflictRejected(t *testing.T) {
	c, _ := NewCompiler(DefaultProtocol(3))
	gates := []ir.GateTask{
		{ID: "g1", Kind: ir.GateX, Qubits: []int{0}},
		{ID: "g2", Kind: ir.GateX, Qubits: []int{0}},
	}
	// 显式把两个无依赖门绑在同一资源同一时段 -> 冲突。
	pulses := []ir.Pulse{
		{ID: "g1:p0", GateID: "g1", Kind: ir.PulseControl, Resource: "q0.ctrl", StartNS: 0, DurationNS: 20, Params: map[string]float64{"amp": 0.5}},
		{ID: "g2:p0", GateID: "g2", Kind: ir.PulseControl, Resource: "q0.ctrl", StartNS: 10, DurationNS: 20, Params: map[string]float64{"amp": 0.5}},
	}
	_, err := c.BindFixed(gates, pulses)
	logSchedule(t, "fixed-conflict", gates, nil, err)
	if err == nil || err.(*Error).Reason != ReasonConflict {
		t.Fatalf("expected ReasonConflict, got %v", err)
	}
}

func TestBindFixedDepViolationRejected(t *testing.T) {
	c, _ := NewCompiler(DefaultProtocol(3))
	gates := []ir.GateTask{
		{ID: "g1", Kind: ir.GateX, Qubits: []int{0}},
		{ID: "g2", Kind: ir.GateX, Qubits: []int{0}, DependsOn: []string{"g1"}},
	}
	// 资源不重叠，但后继先于前驱结束开始 -> 违反完成-开始依赖。
	pulses := []ir.Pulse{
		{ID: "g1:p0", GateID: "g1", Kind: ir.PulseControl, Resource: "q0.ctrl", StartNS: 0, DurationNS: 20, Params: map[string]float64{"amp": 0.5}},
		{ID: "g2:p0", GateID: "g2", Kind: ir.PulseControl, Resource: "q0.ctrl", StartNS: 10, DurationNS: 20, Params: map[string]float64{"amp": 0.5}},
	}
	_, err := c.BindFixed(gates, pulses)
	logSchedule(t, "dep-violation", gates, nil, err)
	if err == nil || err.(*Error).Reason != ReasonConflict {
		// [0,20) 与 [10,30) 在同资源上首先构成冲突；冲突优先于依赖检查。
		t.Fatalf("expected ReasonConflict (overlap), got %v", err)
	}

	// 改成不同资源、不重叠，但仍违反完成-开始：需要第二个控制资源，
	// 但 g2 的形状固定为 q0.ctrl，因此通过“依赖但时间倒序”的场景更直接：
	gates2 := []ir.GateTask{
		{ID: "g1", Kind: ir.GateMeasure, Qubits: []int{0}},
		{ID: "g2", Kind: ir.GateMeasure, Qubits: []int{1}, DependsOn: []string{"g1"}},
	}
	pulses2 := []ir.Pulse{
		{ID: "g1:m0", GateID: "g1", Kind: ir.PulseMeasure, Resource: "measure.bus", StartNS: 40, DurationNS: 40, Params: map[string]float64{"amp": 1, "qubit": 0}},
		{ID: "g2:m0", GateID: "g2", Kind: ir.PulseMeasure, Resource: "measure.bus", StartNS: 0, DurationNS: 40, Params: map[string]float64{"amp": 1, "qubit": 1}},
	}
	_, err2 := c.BindFixed(gates2, pulses2)
	logSchedule(t, "dep-order", gates2, nil, err2)
	if err2 == nil || err2.(*Error).Reason != ReasonUnsatisfiableDep {
		t.Fatalf("expected ReasonUnsatisfiableDep, got %v", err2)
	}
}

func TestIllegalParamsRejected(t *testing.T) {
	c, _ := NewCompiler(DefaultProtocol(2))
	cases := []struct {
		name  string
		gates []ir.GateTask
	}{
		{"qubit-oob", []ir.GateTask{{ID: "g", Kind: ir.GateX, Qubits: []int{9}}}},
		{"arity", []ir.GateTask{{ID: "g", Kind: ir.GateCNOT, Qubits: []int{0}}}},
		{"unknown-dep", []ir.GateTask{{ID: "g", Kind: ir.GateX, Qubits: []int{0}, DependsOn: []string{"nope"}}}},
		{"cycle", []ir.GateTask{
			{ID: "a", Kind: ir.GateX, Qubits: []int{0}, DependsOn: []string{"b"}},
			{ID: "b", Kind: ir.GateX, Qubits: []int{1}, DependsOn: []string{"a"}},
		}},
		{"dup-id", []ir.GateTask{
			{ID: "g", Kind: ir.GateX, Qubits: []int{0}},
			{ID: "g", Kind: ir.GateX, Qubits: []int{1}},
		}},
	}
	for _, tc := range cases {
		_, err := c.Compile(tc.gates)
		logSchedule(t, "illegal:"+tc.name, tc.gates, nil, err)
		if err == nil {
			t.Fatalf("%s: expected rejection", tc.name)
		}
		if err.(*Error).Reason != ReasonIllegalParam && err.(*Error).Reason != ReasonUnsatisfiableDep {
			t.Fatalf("%s: expected classified reason, got %s", tc.name, err.(*Error).Reason)
		}
	}
}

func TestReorderStableAndSemanticPreserving(t *testing.T) {
	c, _ := NewCompiler(DefaultProtocol(4))
	// 手工构造一个带空洞的计划：q0 上 g3 被放到很晚，尽管 g1 早就结束。
	gates := []ir.GateTask{
		{ID: "g1", Kind: ir.GateX, Qubits: []int{0}},
		{ID: "g2", Kind: ir.GateX, Qubits: []int{1}},
		{ID: "g3", Kind: ir.GateX, Qubits: []int{0}, DependsOn: []string{"g1"}},
	}
	loose, err := c.BindFixed(gates, []ir.Pulse{
		{ID: "g1:p0", GateID: "g1", Kind: ir.PulseControl, Resource: "q0.ctrl", StartNS: 0, DurationNS: 20, Params: map[string]float64{"amp": 0.5}},
		{ID: "g2:p0", GateID: "g2", Kind: ir.PulseControl, Resource: "q1.ctrl", StartNS: 0, DurationNS: 20, Params: map[string]float64{"amp": 0.5}},
		{ID: "g3:p0", GateID: "g3", Kind: ir.PulseControl, Resource: "q0.ctrl", StartNS: 100, DurationNS: 20, Params: map[string]float64{"amp": 0.5}},
	})
	if err != nil {
		t.Fatal(err)
	}
	logSchedule(t, "before-reorder", gates, loose, nil)
	beforeIdle := loose.TotalIdleNS()
	r1 := Reorder(loose)
	r2 := Reorder(loose)
	logSchedule(t, "after-reorder", gates, r1, nil)
	if !SameSemantics(loose, r1) {
		t.Fatal("reorder must preserve semantics")
	}
	if r1.TotalIdleNS() > beforeIdle {
		t.Fatalf("reorder must not increase idle: %d -> %d", beforeIdle, r1.TotalIdleNS())
	}
	// 稳定可复现：两次重排结果完全一致。
	if r1.MakespanNS() != r2.MakespanNS() || r1.TotalIdleNS() != r2.TotalIdleNS() {
		t.Fatal("reorder not deterministic")
	}
	for i := range r1.Pulses {
		if !reflect.DeepEqual(r1.Pulses[i], r2.Pulses[i]) {
			t.Fatalf("reorder differs at pulse %d", i)
		}
	}
	// g3 应被 ASAP 拉到 g1 结束处(20)，空洞被消除。
	for _, p := range r1.Pulses {
		if p.ID == "g3:p0" && p.StartNS != 20 {
			t.Fatalf("g3 should start at 20 after ASAP, got %d", p.StartNS)
		}
	}
}

func TestDeterministicCompile(t *testing.T) {
	c, _ := NewCompiler(DefaultProtocol(4))
	gates := []ir.GateTask{
		{ID: "a", Kind: ir.GateX, Qubits: []int{0}},
		{ID: "b", Kind: ir.GateH, Qubits: []int{1}},
		{ID: "cc", Kind: ir.GateCNOT, Qubits: []int{0, 1}, DependsOn: []string{"a", "b"}},
		{ID: "m", Kind: ir.GateMeasure, Qubits: []int{0}, DependsOn: []string{"cc"}},
	}
	s1, err := c.Compile(gates)
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 5; n++ {
		s2, _ := c.Compile(gates)
		if len(s1.Pulses) != len(s2.Pulses) {
			t.Fatal("nondeterministic pulse count")
		}
		for i := range s1.Pulses {
			if !reflect.DeepEqual(s1.Pulses[i], s2.Pulses[i]) {
				t.Fatalf("nondeterministic compile at run %d pulse %d", n, i)
			}
		}
	}
	logSchedule(t, "deterministic", gates, s1, nil)
}
