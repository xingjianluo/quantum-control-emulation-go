// Command demo 端到端演示本地量子控制编译、调度与校准仿真流水线：
//
//	校准提交 -> 高层门编译 -> 确定性 ASAP 调度 -> 顺序/并发仿真对比
//	-> 校准过期拒绝 -> 重新校准 -> 回滚到历史版本并复现历史结论。
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/xingjianluo/quantum-control-emulation-go/internal/calibration"
	"github.com/xingjianluo/quantum-control-emulation-go/internal/compiler"
	"github.com/xingjianluo/quantum-control-emulation-go/internal/ir"
	"github.com/xingjianluo/quantum-control-emulation-go/internal/scheduler"
	"github.com/xingjianluo/quantum-control-emulation-go/internal/simulator"
)

func main() {
	at := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	ttl := time.Hour

	// 1) 校准仓库：提交稳定版本 v1
	store := calibration.NewStore()
	draft := store.BeginDraft(0, "daily calibration", at)
	draft.GateParams[calibration.MakeGateKey(ir.GateX, 0)] = map[string]float64{"amp": 0.50, "freq": 5.10}
	draft.GateParams[calibration.MakeGateKey(ir.GateX, 1)] = map[string]float64{"amp": 0.51, "freq": 5.11}
	draft.GateParams[calibration.MakeGateKey(ir.GateCNOT, 0, 1)] = map[string]float64{"amp": 0.70, "freq": 4.80}
	draft.ReadoutFidelity[0] = 0.98
	draft.ReadoutFidelity[1] = 0.97
	tol := calibration.Tolerance{ParamDelta: map[string]float64{"amp": 0.05, "freq": 0.5}, ReadoutMin: 0.9, ReadoutMax: 1}
	v1, err := store.Commit(draft, tol, at)
	must(err)
	fmt.Printf("[calib] 提交稳定校准 v%d @ %s\n", v1, at)

	// 2) 高层门任务
	tasks := []ir.GateTask{
		{ID: "init-0", Kind: ir.GateX, Qubits: []int{0}},
		{ID: "init-1", Kind: ir.GateX, Qubits: []int{1}},
		{ID: "entangle", Kind: ir.GateCNOT, Qubits: []int{0, 1}, DependsOn: []string{"init-0", "init-1"}},
		{ID: "read-0", Kind: ir.GateMeas, Qubits: []int{0}, DependsOn: []string{"entangle"}},
		{ID: "read-1", Kind: ir.GateMeas, Qubits: []int{1}, DependsOn: []string{"entangle"}},
	}

	// 3) 编译 + 4) 调度
	cp := compiler.New(store, compiler.Config{TTL: ttl})
	prog, err := cp.Compile(tasks, at)
	must(err)
	sched := scheduler.New()
	must(sched.Assign(prog))
	fmt.Printf("[compile+schedule] %d 条脉冲, makespan=%s, 绑定校准 v%d\n",
		len(prog.Ops), prog.Makespan(), prog.CalibVersion)
	for _, d := range sched.Decisions() {
		fmt.Printf("    决策 %-10s 开始于 %-8s (%s)\n", d.OpID, d.Start, d.Why)
	}

	// 5) 顺序 vs 并发仿真（确定性、结论等价）
	eng := simulator.NewEngine(store)
	run := func(mode simulator.Mode) *simulator.Result {
		r, err := eng.Run(context.Background(), prog,
			simulator.Config{Mode: mode, Seed: 42, TTL: ttl, At: at.Add(time.Minute),
				Waiter: simulator.ImmediateWaiter{}})
		must(err)
		return r
	}
	seq := run(simulator.ModeSequential)
	con := run(simulator.ModeConcurrent)
	fmt.Printf("[simulate] 顺序: completed=%d failed=%d | 并发: completed=%d failed=%d\n",
		seq.Completed, seq.Failed, con.Completed, con.Failed)
	for _, o := range seq.Outcomes {
		fmt.Printf("    %-10s ok=%-5v measured=%d reason=%q\n", o.OpID, o.OK, o.Measured, o.FailReason)
	}

	// 6) 过期校准必须被拒绝
	if _, err := eng.Run(context.Background(), prog,
		simulator.Config{Mode: simulator.ModeSequential, TTL: ttl, At: at.Add(2 * time.Hour),
			Waiter: simulator.ImmediateWaiter{}}); err != nil {
		fmt.Printf("[calib-expiry] 2 小时后执行旧计划被拒绝: %v\n", err)
	}

	// 7) 重新校准 v2 -> 仿真 -> 回滚 v1 -> 结论与 v1 历史一致
	d2 := store.BeginDraft(v1, "recal", at.Add(90*time.Minute))
	d2.GateParams[calibration.MakeGateKey(ir.GateX, 0)]["amp"] = 0.52
	d2.ReadoutFidelity[0] = 0.99
	v2, err := store.Commit(d2, tol, at.Add(90*time.Minute))
	must(err)
	fmt.Printf("[calib] 提交新稳定校准 v%d\n", v2)

	prog2, err := cp.Compile(tasks, at.Add(91*time.Minute))
	must(err)
	must(sched.Assign(prog2))
	resV2, err := eng.Run(context.Background(), prog2, simulator.Config{
		Mode: simulator.ModeConcurrent, Seed: 42, TTL: ttl, At: at.Add(91 * time.Minute),
		Waiter: simulator.ImmediateWaiter{}})
	must(err)
	fmt.Printf("[simulate-v2] 基于 v%d: completed=%d failed=%d\n", v2, resV2.Completed, resV2.Failed)

	rb, err := store.Rollback(at.Add(95 * time.Minute))
	must(err)
	fmt.Printf("[rollback] 回到稳定版本 v%d（注意：它相对当前墙钟时间仍已过期，系统不会静默沿用）\n", rb)
	histSnap, ok := store.Get(rb)
	must2(ok)
	prog1b, err := cp.Compile(tasks, histSnap.CreatedAt)
	must(err)
	must(sched.Assign(prog1b))
	// 显式"历史重放"：以快照自身的历史时刻执行，才能合法复现历史结论。
	resV1b, err := eng.Run(context.Background(), prog1b, simulator.Config{
		Mode: simulator.ModeConcurrent, Seed: 42, TTL: ttl, At: histSnap.CreatedAt.Add(time.Minute),
		Waiter: simulator.ImmediateWaiter{}})
	must(err)
	same := outcomesEqual(seq.Outcomes, resV1b.Outcomes)
	fmt.Printf("[rollback-verify] 回滚后结论与 v1 历史记录一致: %v\n", same)
	if !same {
		panic("回滚后仿真结论与历史不一致")
	}
}

func outcomesEqual(a, b []simulator.OpOutcome) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func must2(ok bool) {
	if !ok {
		panic("expected value missing")
	}
}
