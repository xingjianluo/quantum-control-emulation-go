// Package integration 以端到端方式验证 编译->调度->仿真->校准过期->回滚复现 的完整流水线。
package integration

import (
	"context"
	"testing"
	"time"

	"github.com/xingjianluo/quantum-control-emulation-go/internal/calibration"
	"github.com/xingjianluo/quantum-control-emulation-go/internal/compiler"
	"github.com/xingjianluo/quantum-control-emulation-go/internal/ir"
	"github.com/xingjianluo/quantum-control-emulation-go/internal/scheduler"
	"github.com/xingjianluo/quantum-control-emulation-go/internal/simulator"
)

func buildStore(t *testing.T, at time.Time, amp0 float64) (*calibration.Store, int) {
	t.Helper()
	s := calibration.NewStore()
	d := s.BeginDraft(0, "c", at)
	d.GateParams[calibration.MakeGateKey(ir.GateX, 0)] = map[string]float64{"amp": amp0, "freq": 5.1}
	d.GateParams[calibration.MakeGateKey(ir.GateX, 1)] = map[string]float64{"amp": 0.5, "freq": 5.2}
	d.GateParams[calibration.MakeGateKey(ir.GateCNOT, 0, 1)] = map[string]float64{"amp": 0.7, "freq": 4.8}
	d.ReadoutFidelity[0] = 0.99
	d.ReadoutFidelity[1] = 0.99
	v, err := s.Commit(d, calibration.Tolerance{ParamDelta: map[string]float64{"amp": 1, "freq": 1}, ReadoutMin: 0, ReadoutMax: 1}, at)
	if err != nil {
		t.Fatal(err)
	}
	return s, v
}

func tasks() []ir.GateTask {
	return []ir.GateTask{
		{ID: "a", Kind: ir.GateX, Qubits: []int{0}},
		{ID: "b", Kind: ir.GateX, Qubits: []int{1}},
		{ID: "c", Kind: ir.GateCNOT, Qubits: []int{0, 1}, DependsOn: []string{"a", "b"}},
		{ID: "m0", Kind: ir.GateMeas, Qubits: []int{0}, DependsOn: []string{"c"}},
		{ID: "m1", Kind: ir.GateMeas, Qubits: []int{1}, DependsOn: []string{"c"}},
	}
}

func TestPipelineCompileScheduleSimulateRollback(t *testing.T) {
	at := time.Unix(2000, 0).UTC()
	ttl := time.Hour
	store, v1 := buildStore(t, at, 0.5)

	compileRun := func(runAt time.Time) (*ir.PulseProgram, *simulator.Result, simulator.Mode, error) {
		cp := compiler.New(store, compiler.Config{TTL: ttl})
		prog, err := cp.Compile(tasks(), runAt)
		if err != nil {
			return nil, nil, "", err
		}
		if err := scheduler.New().Assign(prog); err != nil {
			return nil, nil, "", err
		}
		eng := simulator.NewEngine(store)
		res, err := eng.Run(context.Background(), prog, simulator.Config{
			Mode: simulator.ModeConcurrent, Seed: 1234, TTL: ttl, At: runAt,
			Waiter: simulator.ImmediateWaiter{},
		})
		return prog, res, simulator.ModeConcurrent, err
	}

	_, hist, _, err := compileRun(at)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("历史(v%d): completed=%d failed=%d 测量=[%d,%d]", v1, hist.Completed, hist.Failed,
		hist.Outcomes[3].Measured, hist.Outcomes[4].Measured)

	// 过期：2 小时后直接执行必须被拒绝
	_, _, _, err = compileRun(at.Add(2 * time.Hour))
	if !calibration.IsError(err, calibration.ReasonExpired) {
		t.Fatalf("期望过期拒绝，得到 %v", err)
	}
	t.Logf("过期输入(2h>TTL) -> 最终判定依据: %v", err)

	// 重新校准 v2（漂移在宽容差内），结论可能随保真度/参数变化
	d2 := store.BeginDraft(v1, "recal", at.Add(30*time.Minute))
	d2.GateParams[calibration.MakeGateKey(ir.GateX, 0)]["amp"] = 0.55
	d2.ReadoutFidelity[0] = 0.995
	v2, err := store.Commit(d2, calibration.Tolerance{ParamDelta: map[string]float64{"amp": 0.1, "freq": 1}, ReadoutMin: 0, ReadoutMax: 1}, at.Add(30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	_, resV2, _, err := compileRun(at.Add(31 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("新校准(v%d): completed=%d failed=%d", v2, resV2.Completed, resV2.Failed)

	// 回滚到 v1，以历史时刻重放，结论必须与 v1 完全一致
	rb, err := store.Rollback(at.Add(40 * time.Minute))
	if err != nil || rb != v1 {
		t.Fatalf("回滚目标错误 rb=%d err=%v", rb, err)
	}
	histSnap, _ := store.Get(rb)
	_, replayed, _, err := compileRun(histSnap.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	for i := range hist.Outcomes {
		if hist.Outcomes[i] != replayed.Outcomes[i] {
			t.Fatalf("回滚后第 %d 条结论不一致: %+v vs %+v", i, hist.Outcomes[i], replayed.Outcomes[i])
		}
	}
	t.Logf("回滚到 v%d 后历史重放: %d 条结论与历史记录逐条一致", rb, len(hist.Outcomes))
}
