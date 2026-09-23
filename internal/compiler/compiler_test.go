package compiler

import (
	"math"
	"testing"
	"time"

	"github.com/xingjianluo/quantum-control-emulation-go/internal/calibration"
	"github.com/xingjianluo/quantum-control-emulation-go/internal/ir"
)

func newTestStore(t *testing.T, at time.Time) (*calibration.Store, int) {
	t.Helper()
	s := calibration.NewStore()
	d := s.BeginDraft(0, "init", at)
	d.GateParams[calibration.MakeGateKey(ir.GateX, 0)] = map[string]float64{"amp": 0.5, "freq": 5.1}
	d.GateParams[calibration.MakeGateKey(ir.GateX, 1)] = map[string]float64{"amp": 0.5, "freq": 5.2}
	d.GateParams[calibration.MakeGateKey(ir.GateX90, 0)] = map[string]float64{"amp": 0.25, "freq": 5.1}
	d.GateParams[calibration.MakeGateKey(ir.GateRZ, 0)] = map[string]float64{}
	d.GateParams[calibration.MakeGateKey(ir.GateCNOT, 0, 1)] = map[string]float64{"amp": 0.7, "freq": 4.8}
	d.ReadoutFidelity[0] = 0.97
	d.ReadoutFidelity[1] = 0.96
	v, err := s.Commit(d, calibration.Tolerance{ReadoutMin: 0, ReadoutMax: 1}, at)
	if err != nil {
		t.Fatal(err)
	}
	return s, v
}

func TestCompileHappyPath(t *testing.T) {
	at := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	store, v := newTestStore(t, at)
	c := New(store, Config{TTL: time.Hour})
	tasks := []ir.GateTask{
		{ID: "g1", Kind: ir.GateX, Qubits: []int{0}},
		{ID: "g2", Kind: ir.GateCNOT, Qubits: []int{0, 1}, DependsOn: []string{"g1"}},
		{ID: "m1", Kind: ir.GateMeas, Qubits: []int{1}, DependsOn: []string{"g2"}},
	}
	prog, err := c.Compile(tasks, at.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if prog.CalibVersion != v {
		t.Fatalf("绑定版本=%d want=%d", prog.CalibVersion, v)
	}
	if len(prog.Ops) != 3 {
		t.Fatalf("脉冲数=%d want 3", len(prog.Ops))
	}
	if prog.Ops[1].ID != "p-g2" || len(prog.Ops[1].Deps) != 1 || prog.Ops[1].Deps[0] != "p-g1" {
		t.Fatalf("依赖映射错误: %+v", prog.Ops[1].Deps)
	}
	t.Logf("输入任务=%+v -> 编译 %d 条脉冲，绑定校准 v%d，依赖链 %s->%s->%s",
		tasks, len(prog.Ops), v, prog.Ops[0].ID, prog.Ops[1].ID, prog.Ops[2].ID)
}

func TestCompileRejectReasons(t *testing.T) {
	at := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	store, _ := newTestStore(t, at)
	c := New(store, Config{TTL: time.Hour})

	cases := []struct {
		name   string
		tasks  []ir.GateTask
		reason FailReason
	}{
		{"未知门", []ir.GateTask{{ID: "g", Kind: "H", Qubits: []int{0}}}, ReasonUnknownGate},
		{"CNOT同比特", []ir.GateTask{{ID: "g", Kind: ir.GateCNOT, Qubits: []int{0, 0}}}, ReasonIllegalParam},
		{"RZ角度非有限", []ir.GateTask{{ID: "g", Kind: ir.GateRZ, Qubits: []int{0}, Angle: math.NaN()}}, ReasonIllegalParam},
		{"重复ID", []ir.GateTask{
			{ID: "g", Kind: ir.GateX, Qubits: []int{0}},
			{ID: "g", Kind: ir.GateX, Qubits: []int{1}},
		}, ReasonDuplicateID},
		{"依赖缺失", []ir.GateTask{{ID: "g", Kind: ir.GateX, Qubits: []int{0}, DependsOn: []string{"nope"}}}, ReasonUnresolvedDep},
		{"缺少校准参数(qubit-2未校准)", []ir.GateTask{{ID: "g", Kind: ir.GateX, Qubits: []int{2}}}, ReasonMissingCalibration},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.Compile(tc.tasks, at.Add(time.Minute))
			if !IsError(err, tc.reason) {
				t.Fatalf("输入=%+v 期望原因 %s，得到 %v", tc.tasks, tc.reason, err)
			}
			t.Logf("输入=%+v -> 判定拒绝: %s (%v)", tc.tasks, tc.reason, err)
		})
	}
}

func TestCompileRejectsExpiredCalibration(t *testing.T) {
	at := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	store, _ := newTestStore(t, at)
	c := New(store, Config{TTL: time.Minute})
	_, err := c.Compile([]ir.GateTask{{ID: "g", Kind: ir.GateX, Qubits: []int{0}}}, at.Add(2*time.Minute))
	if !calibration.IsError(err, calibration.ReasonExpired) {
		t.Fatalf("过期校准必须拒绝编译，得到 %v", err)
	}
	t.Logf("判定: 编译期检测到校准过期 -> %v（不静默沿用旧值）", err)
}
