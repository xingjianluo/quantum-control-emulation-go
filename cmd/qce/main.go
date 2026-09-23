// Command qce 运行一个端到端的本地量子控制编译、调度与校准仿真演示。
//
// 流程：
//  1. 构造硬件协议与高层门任务，编译为脉冲计划（ASAP 确定性调度）。
//  2. 展示保语义重排（压缩空闲等待）。
//  3. 建立分层校准存储（含 TTL）。
//  4. 分别以顺序与并发模式执行同一作业，校验聚合结论等价。
//  5. 演示校准过期拒绝与回滚复现。
package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/xingjianluo/quantum-control-emulation-go/internal/calibration"
	"github.com/xingjianluo/quantum-control-emulation-go/internal/compile"
	"github.com/xingjianluo/quantum-control-emulation-go/internal/ir"
	"github.com/xingjianluo/quantum-control-emulation-go/internal/sim"
)

func main() {
	ctx := context.Background()

	// 1) 编译与调度。
	comp, err := compile.NewCompiler(compile.DefaultProtocol(3))
	must(err)
	gates := []ir.GateTask{
		{ID: "x0", Kind: ir.GateX, Qubits: []int{0}},
		{ID: "x1", Kind: ir.GateX, Qubits: []int{1}},
		{ID: "cx", Kind: ir.GateCNOT, Qubits: []int{0, 1}, DependsOn: []string{"x0", "x1"}},
		{ID: "mz", Kind: ir.GateMeasure, Qubits: []int{0}, DependsOn: []string{"cx"}},
	}
	sched, err := comp.Compile(gates)
	must(err)
	fmt.Printf("== 编译结果: makespan=%dns idle=%dns\n", sched.MakespanNS(), sched.TotalIdleNS())
	for _, p := range sched.Pulses {
		fmt.Printf("  %-8s gate=%-3s res=%-12s [%3d,%3d)\n", p.ID, p.GateID, p.Resource, p.StartNS, p.EndNS())
	}

	// 2) 保语义重排（制造空洞后压缩）。
	loose, err := comp.BindFixed(gates, withDelayedMeasure(sched))
	must(err)
	rebased := compile.Reorder(loose)
	fmt.Printf("== 重排: makespan %dns -> %dns, 语义保持=%v\n",
		loose.MakespanNS(), rebased.MakespanNS(), compile.SameSemantics(loose, rebased))

	// 3) 分层校准。
	t0 := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	store := calibration.NewStore(time.Hour, 3,
		map[string]float64{"amp": 0.2, "frequency": 100},
		[]string{"amp"})
	must(store.Seed(calibration.Version{
		ID: "factory", CreatedAt: t0,
		Parameters: map[string]map[string]float64{
			calibration.GlobalLayer: {"amp": 0.5, "frequency": 5000},
			"q0.ctrl":               {"amp": 0.48},
		},
		ReadoutFid: map[int]float64{0: 0.97, 1: 0.95, 2: 0.93},
		Note:       "factory calibration",
	}))

	// 4) 顺序 vs 并发，结论必须等价。
	exec := sim.NewExecutor(store, sim.DefaultNoise())
	job := sim.Job{Schedule: sched, Shots: 200, Seed: 20260924, Now: t0.Add(30 * time.Minute)}
	seq := exec.RunSequential(ctx, job)
	con := exec.RunConcurrent(ctx, job)
	fmt.Printf("== 顺序/并发等价: %v\n", seq.Equivalent(con))
	printCounts("顺序", seq)
	printCounts("并发", con)

	// 5) TTL 内先提交未稳定的新版本，再回滚到工厂稳定版并复现历史结论。
	fresh := t0.Add(30 * time.Minute)
	cand := calibration.Candidate{
		Parameters: map[string]map[string]float64{
			calibration.GlobalLayer: {"amp": 0.51, "frequency": 5002},
			"q0.ctrl":               {"amp": 0.49},
		},
		ReadoutFid: map[int]float64{0: 0.97, 1: 0.95, 2: 0.93},
		Note:       "daily refresh",
	}
	v2, err := store.Commit(cand, fresh)
	must(err)
	must(store.AttachHistory("factory", calibration.HistorySummary{Summary: "baseline-bucket=0.97"}))
	fmt.Printf("== 提交新版本(未稳定): %s, head=%s\n", v2.ID, store.CurrentID())
	rb, err := store.Rollback(fresh)
	must(err)
	hist, err := store.HistoryAt(rb.ID)
	must(err)
	fmt.Printf("== 回滚至 %s, 历史结论复现: %q\n", rb.ID, hist.Summary)

	// 6) TTL 过期后：拒绝静默沿用，仿真整体失败；重新校准（容差内）恢复可用。
	if _, err := store.Current(t0.Add(2 * time.Hour)); err != nil {
		fmt.Printf("== 过期校准被拒绝: %v\n", err)
	}
	exp := exec.RunSequential(ctx, sim.Job{Schedule: sched, Shots: 10, Seed: 1, Now: t0.Add(2 * time.Hour)})
	printCounts("过期作业", exp)
	recal, err := store.Commit(cand, t0.Add(2*time.Hour))
	must(err)
	must(store.MarkStable(t0.Add(2 * time.Hour)))
	fmt.Printf("== 重新校准并标记稳定: %s\n", recal.ID)
	recovered := exec.RunSequential(ctx, sim.Job{Schedule: sched, Shots: 10, Seed: 1, Now: t0.Add(2 * time.Hour)})
	printCounts("恢复作业", recovered)

	_ = rebased
}

func withDelayedMeasure(s *ir.Schedule) []ir.Pulse {
	ps := make([]ir.Pulse, len(s.Pulses))
	copy(ps, s.Pulses)
	for i := range ps {
		if ps[i].Kind == ir.PulseMeasure {
			ps[i].StartNS += 200
		}
	}
	return ps
}

func printCounts(label string, r *sim.JobResult) {
	counts := r.Counts()
	ids := make([]string, 0, len(counts))
	for gate := range counts {
		ids = append(ids, gate)
	}
	sort.Strings(ids)
	for _, gate := range ids {
		fmt.Printf("  [%s] gate=%s counts=%v\n", label, gate, counts[gate])
	}
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}
