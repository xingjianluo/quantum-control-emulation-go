package simulator

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/xingjianluo/quantum-control-emulation-go/internal/calibration"
	"github.com/xingjianluo/quantum-control-emulation-go/internal/compiler"
	"github.com/xingjianluo/quantum-control-emulation-go/internal/ir"
	"github.com/xingjianluo/quantum-control-emulation-go/internal/scheduler"
)

const t0Unix = 0

func testFixture(t *testing.T) (*calibration.Store, *ir.PulseProgram) {
	t.Helper()
	at := time.Unix(1000, 0).UTC()
	store := calibration.NewStore()
	d := store.BeginDraft(0, "init", at)
	d.GateParams[calibration.MakeGateKey(ir.GateX, 0)] = map[string]float64{"amp": 0.5, "freq": 5.1}
	d.GateParams[calibration.MakeGateKey(ir.GateX, 1)] = map[string]float64{"amp": 0.5, "freq": 5.2}
	d.GateParams[calibration.MakeGateKey(ir.GateCNOT, 0, 1)] = map[string]float64{"amp": 0.7, "freq": 4.8}
	d.ReadoutFidelity[0] = 0.99
	d.ReadoutFidelity[1] = 0.99
	if _, err := store.Commit(d, calibration.Tolerance{ReadoutMin: 0, ReadoutMax: 1}, at); err != nil {
		t.Fatal(err)
	}
	cp := compiler.New(store, compiler.Config{})
	tasks := []ir.GateTask{
		{ID: "x0", Kind: ir.GateX, Qubits: []int{0}},
		{ID: "x1", Kind: ir.GateX, Qubits: []int{1}},
		{ID: "cx", Kind: ir.GateCNOT, Qubits: []int{0, 1}, DependsOn: []string{"x0", "x1"}},
		{ID: "m0", Kind: ir.GateMeas, Qubits: []int{0}, DependsOn: []string{"cx"}},
		{ID: "m1", Kind: ir.GateMeas, Qubits: []int{1}, DependsOn: []string{"cx"}},
	}
	prog, err := cp.Compile(tasks, at)
	if err != nil {
		t.Fatal(err)
	}
	if err := scheduler.New().Assign(prog); err != nil {
		t.Fatal(err)
	}
	return store, prog
}

func cmpResult(a, b *Result) bool {
	if a.CalibVersion != b.CalibVersion ||
		a.Completed != b.Completed || a.Failed != b.Failed {
		return false
	}
	// Mode 预期不同（顺序 vs 并发），其余要素须逐字段一致。
	return reflect.DeepEqual(a.Outcomes, b.Outcomes) && reflect.DeepEqual(canonicalEvents(a.Events), canonicalEvents(b.Events))
}

func canonicalEvents(evs []Event) []struct {
	OpID    string
	Phase   Phase
	Virtual time.Duration
} {
	out := make([]struct {
		OpID    string
		Phase   Phase
		Virtual time.Duration
	}, len(evs))
	for i, e := range evs {
		out[i] = struct {
			OpID    string
			Phase   Phase
			Virtual time.Duration
		}{e.OpID, e.Phase, e.Virtual}
	}
	return out
}

func TestSequentialConcurrentEquivalence(t *testing.T) {
	store, prog := testFixture(t)
	eng := NewEngine(store)
	base := func(mode Mode) Config {
		return Config{Mode: mode, Seed: 42, At: time.Unix(1000, 0).UTC(), Waiter: ImmediateWaiter{}}
	}
	seqRes, err := eng.Run(context.Background(), prog, base(ModeSequential))
	if err != nil {
		t.Fatal(err)
	}
	// 并发多跑几轮，任何调度交织下结论都必须一致。
	var conRes *Result
	for i := 0; i < 20; i++ {
		r, err := eng.Run(context.Background(), prog, base(ModeConcurrent))
		if err != nil {
			t.Fatal(err)
		}
		if conRes != nil && !reflect.DeepEqual(conRes.Outcomes, r.Outcomes) {
			t.Fatalf("并发运行间不确定：第 %d 轮结论漂移", i)
		}
		conRes = r
	}
	if !cmpResult(seqRes, conRes) {
		t.Fatalf("顺序与并发结论不等价\nseq=%+v\ncon=%+v", seqRes.Outcomes, conRes.Outcomes)
	}
	t.Logf("输入: %d 条脉冲(seed=42)；调度决策后两种模式 completed=%d failed=%d",
		len(prog.Ops), seqRes.Completed, seqRes.Failed)
	for _, o := range seqRes.Outcomes {
		t.Logf("  判定 %s ok=%v measured=%d reason=%q", o.OpID, o.OK, o.Measured, o.FailReason)
	}
	for _, e := range seqRes.Events {
		t.Logf("  事件 t=%-6s %-7s %s %s", e.Virtual, e.Phase, e.OpID, e.Detail)
	}
}

func TestDeterminismAcrossSeeds(t *testing.T) {
	store, prog := testFixture(t)
	eng := NewEngine(store)
	r1, _ := eng.Run(context.Background(), prog, Config{Mode: ModeConcurrent, Seed: 7, At: time.Unix(1000, 0).UTC(), Waiter: ImmediateWaiter{}})
	r2, _ := eng.Run(context.Background(), prog, Config{Mode: ModeSequential, Seed: 7, At: time.Unix(1000, 0).UTC(), Waiter: ImmediateWaiter{}})
	if !reflect.DeepEqual(r1.Outcomes, r2.Outcomes) {
		t.Fatalf("相同 seed 下跨模式结论必须一致")
	}
}

func TestRunRejectsExpiredAndMismatch(t *testing.T) {
	at := time.Unix(1000, 0).UTC()
	store, prog := testFixture(t)

	// 1) 过期：TTL=1m，执行时刻在 2 分钟后
	_, err := NewEngine(store).Run(context.Background(), prog,
		Config{Mode: ModeConcurrent, At: at.Add(2 * time.Minute), TTL: time.Minute, Waiter: ImmediateWaiter{}})
	if !IsError(err, ReasonCalibExpired) {
		t.Fatalf("期望 calibration_expired，得到 %v", err)
	}
	t.Logf("过期校准执行 -> 拒绝: %v", err)

	// 2) 版本不匹配：在不回滚当前指针的前提下提交新版本，旧计划即失效
	d := store.BeginDraft(0, "v2", at.Add(30*time.Second))
	if _, err := store.Commit(d, calibration.Tolerance{ParamDelta: map[string]float64{"amp": 1, "freq": 1}, ReadoutMin: 0, ReadoutMax: 1}, at.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err = NewEngine(store).Run(context.Background(), prog,
		Config{Mode: ModeSequential, At: at.Add(31 * time.Second), Waiter: ImmediateWaiter{}})
	if !IsError(err, ReasonCalibMismatch) {
		t.Fatalf("期望 version_mismatch，得到 %v", err)
	}
	t.Logf("计划绑定 v%d 但当前为更新版本 -> 拒绝: %v", prog.CalibVersion, err)
}

func TestRunRejectsBadProgram(t *testing.T) {
	at := time.Unix(1000, 0).UTC()
	store, prog := testFixture(t)
	prog.Ops[0].Start = 100 * time.Hour // 人为制造依赖倒置
	_, err := NewEngine(store).Run(context.Background(), prog,
		Config{At: at, Waiter: ImmediateWaiter{}})
	if !IsError(err, ReasonBadProgram) {
		t.Fatalf("期望 bad_program，得到 %v", err)
	}
	t.Logf("非法计划 -> 拒绝: %v", err)
}

func TestTimeoutDeterministic(t *testing.T) {
	at := time.Unix(1000, 0).UTC()
	store, prog := testFixture(t)
	eng := NewEngine(store)
	// cx 脉冲时长 80ns，限 50ns -> cx 超时，下游 m0/m1 必须跳过且不产生测量值
	cfg := Config{Mode: ModeConcurrent, At: at, OpTimeout: 50 * time.Nanosecond,
		Waiter: DeterministicWaiter{MaxDuration: 50 * time.Nanosecond}}
	res, err := eng.Run(context.Background(), prog, cfg)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]OpOutcome{}
	for _, o := range res.Outcomes {
		byID[o.OpID] = o
	}
	if byID["p-cx"].FailReason != string(ReasonTimeout) {
		t.Fatalf("期望 p-cx timeout，得到 %+v", byID["p-cx"])
	}
	for _, m := range []string{"p-m0", "p-m1"} {
		if byID[m].FailReason != string(ReasonSkippedDep) || byID[m].Measured != -1 {
			t.Fatalf("下游 %s 应跳过且无测量值，得到 %+v", m, byID[m])
		}
	}
	if res.Completed+res.Failed != len(prog.Ops) {
		t.Fatalf("每条操作必须恰好计数一次: completed=%d failed=%d total=%d",
			res.Completed, res.Failed, len(prog.Ops))
	}
	t.Logf("输入: OpTimeout=50ns, p-cx 时长=80ns -> 判定 p-cx=timeout；下游 m0/m1=skipped_dependency；合计 %d 条无遗漏", len(prog.Ops))
	for _, e := range res.Events {
		t.Logf("  事件 t=%-6s %-7s %s", e.Virtual, e.Phase, e.OpID)
	}
}

func TestVirtualCancelDeterministicAndEquivalent(t *testing.T) {
	at := time.Unix(1000, 0).UTC()
	store, prog := testFixture(t)
	eng := NewEngine(store)

	// 时序：x0/x1 @[0,30ns)；cx @[30,110ns)；m0/m1 @[110,310ns)
	// 取消点 40ns 落在 cx 窗口内 -> cx 在途取消；x0/x1 已完成，m0/m1 因果跳过。
	mk := func(mode Mode) Config {
		return Config{Mode: mode, Seed: 5, At: at, CancelAt: 40 * time.Nanosecond,
			Waiter: ImmediateWaiter{}}
	}
	seq, err := eng.Run(context.Background(), prog, mk(ModeSequential))
	if err != nil {
		t.Fatal(err)
	}
	var con *Result
	for i := 0; i < 20; i++ {
		r, err := eng.Run(context.Background(), prog, mk(ModeConcurrent))
		if err != nil {
			t.Fatal(err)
		}
		if con != nil && !reflect.DeepEqual(con.Outcomes, r.Outcomes) {
			t.Fatalf("取消场景并发第 %d 轮结论漂移", i)
		}
		con = r
	}
	if !reflect.DeepEqual(seq.Outcomes, con.Outcomes) {
		t.Fatalf("虚拟取消点下顺序/并发结论不等价\nseq=%+v\ncon=%+v", seq.Outcomes, con.Outcomes)
	}
	byID := map[string]OpOutcome{}
	for _, o := range seq.Outcomes {
		byID[o.OpID] = o
	}
	if !byID["p-x0"].OK || !byID["p-x1"].OK {
		t.Fatalf("取消点之前的独立操作必须完成: x0=%+v x1=%+v", byID["p-x0"], byID["p-x1"])
	}
	if byID["p-cx"].FailReason != string(ReasonCanceled) {
		t.Fatalf("p-cx 应为在途取消，得到 %+v", byID["p-cx"])
	}
	for _, m := range []string{"p-m0", "p-m1"} {
		if byID[m].FailReason != string(ReasonSkippedDep) || byID[m].Measured != -1 {
			t.Fatalf("下游 %s 应跳过且无测量值，得到 %+v", m, byID[m])
		}
	}
	if seq.Completed+seq.Failed != len(prog.Ops) {
		t.Fatalf("计数不平: %d+%d != %d", seq.Completed, seq.Failed, len(prog.Ops))
	}
	t.Logf("输入: CancelAt=40ns 落在 cx[30,110ns) 窗口内 -> 判定 x0/x1=ok, cx=canceled, m0/m1=skipped；顺序与并发 20 轮一致")
	for _, e := range seq.Events {
		t.Logf("  事件 t=%-6s %-7s %s %s", e.Virtual, e.Phase, e.OpID, e.Detail)
	}
}

func TestExternalContextCancelExactlyOnce(t *testing.T) {
	at := time.Unix(1000, 0).UTC()
	store, prog := testFixture(t)
	eng := NewEngine(store)

	// 外部 ctx 取消与 goroutine 完成天然存在竞态，因此本测试只断言与交织无关的不变量：
	// 每条操作恰好一条结论、计数守恒、被取消操作的下游必为 skip、因果事件不错位。
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan string, 4)
	cfg := Config{Mode: ModeConcurrent, At: at,
		Waiter: BlockingWaiter{Block: map[string]bool{"p-x0": true}, Entered: entered}}
	done := make(chan *Result)
	go func() {
		r, err := eng.Run(ctx, prog, cfg)
		if err != nil {
			t.Errorf("取消是正常结果而非引擎错误: %v", err)
		}
		done <- r
	}()
	select {
	case id := <-entered:
		if id != "p-x0" {
			t.Fatalf("期望进入阻塞的是 p-x0，得到 %s", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("p-x0 未进入在途等待")
	}
	cancel()
	res := <-done

	counts := map[string]int{}
	for _, o := range res.Outcomes {
		counts[o.OpID]++
	}
	for id, n := range counts {
		if n != 1 {
			t.Fatalf("操作 %s 出现 %d 次，违反恰好一次", id, n)
		}
	}
	byID := map[string]OpOutcome{}
	for _, o := range res.Outcomes {
		byID[o.OpID] = o
	}
	if byID["p-x0"].FailReason != string(ReasonCanceled) {
		t.Fatalf("p-x0 应为 canceled，得到 %+v", byID["p-x0"])
	}
	if byID["p-cx"].FailReason != string(ReasonSkippedDep) {
		t.Fatalf("p-cx 应因上游取消而跳过，得到 %+v", byID["p-cx"])
	}
	if res.Completed+res.Failed != len(prog.Ops) {
		t.Fatalf("计数不平: completed=%d failed=%d total=%d", res.Completed, res.Failed, len(prog.Ops))
	}
	var cancelEnd time.Duration
	for _, e := range res.Events {
		if e.OpID == "p-x0" && e.Phase == PhaseCancel {
			cancelEnd = e.Virtual
		}
		if e.OpID == "p-cx" && e.Phase == PhaseSkip && e.Virtual < cancelEnd {
			t.Fatalf("因果记录错位: p-cx skip 早于 p-x0 cancel")
		}
	}
	t.Logf("外部 ctx 在途取消: p-x0=canceled, p-cx/m0/m1 因果处理；completed=%d failed=%d（恰好一次、无错位）",
		res.Completed, res.Failed)
}

func TestSequentialConcurrentEquivalenceUnderTimeout(t *testing.T) {
	at := time.Unix(1000, 0).UTC()
	store, prog := testFixture(t)
	eng := NewEngine(store)
	cfg := func(m Mode) Config {
		return Config{Mode: m, Seed: 99, At: at, OpTimeout: 50 * time.Nanosecond,
			Waiter: DeterministicWaiter{MaxDuration: 50 * time.Nanosecond}}
	}
	seq, err := eng.Run(context.Background(), prog, cfg(ModeSequential))
	if err != nil {
		t.Fatal(err)
	}
	con, err := eng.Run(context.Background(), prog, cfg(ModeConcurrent))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(seq.Outcomes, con.Outcomes) {
		t.Fatalf("超时场景下顺序/并发结论不等价\n%+v\n%+v", seq.Outcomes, con.Outcomes)
	}
	t.Log("超时边界场景下顺序与并发聚合结论逐字段一致")
}
