package sim

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xingjianluo/quantum-control-emulation-go/internal/calibration"
	"github.com/xingjianluo/quantum-control-emulation-go/internal/compile"
	"github.com/xingjianluo/quantum-control-emulation-go/internal/ir"
)

// cancelAfterHook 返回一个 hook：第 n 次进入时触发 cancel（模拟执行到一半
// 被取消）。返回的计数函数用于断言没有重复执行。
func cancelAfterHook(n int, cancel context.CancelFunc) (Hook, func() int) {
	var mu sync.Mutex
	entries := 0
	return func(ctx context.Context) error {
			mu.Lock()
			entries++
			cur := entries
			mu.Unlock()
			if cur == n {
				cancel()
			}
			return nil
		}, func() int {
			mu.Lock()
			defer mu.Unlock()
			return entries
		}
}

// countEvents 统计指定 kind 的事件数；gate 为空串时统计所有门。
func countEvents(r *JobResult, kind, gate string) int {
	n := 0
	for _, ev := range r.Events {
		if ev.Kind != kind {
			continue
		}
		if gate != "" && ev.GateID != gate {
			continue
		}
		n++
	}
	return n
}

// resumeDecision 取出续跑决策事件文本，供日志与断言。
func resumeDecision(r *JobResult) string {
	for _, ev := range r.Events {
		if ev.Kind == "resume-decision" {
			return ev.Note
		}
	}
	return ""
}

// allFinal 检查断点中所有 shot 都已完成（续跑到底后不应再有未完成的）。
func allFinal(cp *Checkpoint) bool {
	for _, rs := range cp.Records {
		for _, r := range rs {
			if !r.Final {
				return false
			}
		}
	}
	return true
}

// TestResumeSequentialCancelMiddle 验证：顺序模式执行到一半被取消后，从断点
// 续跑的最终结论与“一开始不被中断、一次性跑完”完全一致。
func TestResumeSequentialCancelMiddle(t *testing.T) {
	now := time.Date(2026, 9, 24, 10, 30, 0, 0, time.UTC)
	s := newStore(t, time.Hour, 3)
	e := NewExecutor(s, DefaultNoise())
	sched := demoSchedule(t)

	// 基线：不中断一次性跑完。
	base := e.RunSequential(context.Background(), Job{Schedule: sched, Shots: 10, Seed: 7, Now: now})
	logResult(t, "baseline-uninterrupted", base)

	// 第一次运行：g1 第 4 次进入 hook 时取消整个作业（执行中间）。
	ctx, cancel := context.WithCancel(context.Background())
	hook, _ := cancelAfterHook(4, cancel)
	j := Job{Schedule: sched, Shots: 10, Seed: 7, Now: now, Hooks: map[string]Hook{"g1": hook}}
	logJob(t, "interrupt-middle", j)
	partial := e.RunSequential(ctx, j)
	logResult(t, "partial", partial)
	cp := partial.Checkpoint
	if cp == nil {
		t.Fatal("interrupted run must carry a checkpoint")
	}
	t.Logf("checkpoint: calVersion=%s final-shots=%d/%d", cp.CalVersion, cp.finalShots(), 4*10)
	if cp.finalShots() == 0 || cp.finalShots() == 40 {
		t.Fatalf("expected a partially completed checkpoint, got final=%d", cp.finalShots())
	}

	// 续跑：新 ctx，不重头再来。
	resumed, err := e.ResumeSequential(context.Background(), j, cp, ResumeStrict)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	logResult(t, "resumed", resumed)
	t.Logf("resume decision: %s", resumeDecision(resumed))
	t.Logf("verdict basis: reused=%d recomputed-events=%d",
		countEvents(resumed, "reuse", ""), len(resumed.Events)-countEvents(resumed, "reuse", "")-1)

	if !resumed.Equivalent(base) {
		t.Fatalf("resumed result differs from uninterrupted baseline:\nresumed=%+v\nbase=%+v",
			resumed.Reports, base.Reports)
	}
	if got := countEvents(resumed, "reuse", ""); got != cp.finalShots() {
		t.Fatalf("reuse events=%d, want exactly the %d final shots from checkpoint", got, cp.finalShots())
	}
	if !allFinal(resumed.Checkpoint) {
		t.Fatal("after resume all shots must be final")
	}
	t.Log("verdict: resumed ≡ uninterrupted baseline (per-shot status/bit, aggregate, counts)")
}

// TestResumeConcurrentCancelMiddle 验证并发模式同样的续跑等价性。
func TestResumeConcurrentCancelMiddle(t *testing.T) {
	now := time.Date(2026, 9, 24, 10, 30, 0, 0, time.UTC)
	s := newStore(t, time.Hour, 3)
	e := NewExecutor(s, DefaultNoise())
	sched := demoSchedule(t)

	base := e.RunConcurrent(context.Background(), Job{Schedule: sched, Shots: 10, Seed: 7, Now: now})
	logResult(t, "baseline-uninterrupted", base)

	ctx, cancel := context.WithCancel(context.Background())
	hook, _ := cancelAfterHook(4, cancel)
	j := Job{Schedule: sched, Shots: 10, Seed: 7, Now: now, Hooks: map[string]Hook{"g1": hook}}
	logJob(t, "interrupt-middle-concurrent", j)
	partial := e.RunConcurrent(ctx, j)
	logResult(t, "partial", partial)
	cp := partial.Checkpoint
	t.Logf("checkpoint: calVersion=%s final-shots=%d/%d", cp.CalVersion, cp.finalShots(), 40)

	resumed, err := e.ResumeConcurrent(context.Background(), j, cp, ResumeStrict)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	logResult(t, "resumed", resumed)
	t.Logf("resume decision: %s", resumeDecision(resumed))

	if !resumed.Equivalent(base) {
		t.Fatalf("concurrent resumed differs from baseline:\nresumed=%+v\nbase=%+v",
			resumed.Reports, base.Reports)
	}
	if !allFinal(resumed.Checkpoint) {
		t.Fatal("after resume all shots must be final")
	}
	t.Log("verdict: concurrent resumed ≡ uninterrupted baseline")
}

// TestResumeCancelAtStart 验证作业刚开始（尚未产出任何结果）就被取消时的续跑。
func TestResumeCancelAtStart(t *testing.T) {
	now := time.Date(2026, 9, 24, 10, 30, 0, 0, time.UTC)
	s := newStore(t, time.Hour, 3)
	e := NewExecutor(s, DefaultNoise())
	sched := demoSchedule(t)
	j := Job{Schedule: sched, Shots: 6, Seed: 3, Now: now}
	logJob(t, "interrupt-at-start", j)

	base := e.RunSequential(context.Background(), j)

	// ctx 在运行前已取消：所有 shot 都是 not-started/cancelled。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	partial := e.RunSequential(ctx, j)
	logResult(t, "partial-at-start", partial)
	if got := partial.Checkpoint.finalShots(); got != 0 {
		t.Fatalf("nothing should be final when cancelled before start, got %d", got)
	}

	resumed, err := e.ResumeSequential(context.Background(), j, partial.Checkpoint, ResumeStrict)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	t.Logf("resume decision: %s", resumeDecision(resumed))
	if !resumed.Equivalent(base) {
		t.Fatal("resume from empty checkpoint must equal uninterrupted baseline")
	}
	if got := countEvents(resumed, "reuse", ""); got != 0 {
		t.Fatalf("nothing to reuse, got %d reuse events", got)
	}
	t.Log("verdict: cancel-at-start resume ≡ baseline, zero reuse")
}

// TestResumeCancelNearEnd 验证接近结束时被取消的续跑：只差最后一点。
func TestResumeCancelNearEnd(t *testing.T) {
	now := time.Date(2026, 9, 24, 10, 30, 0, 0, time.UTC)
	s := newStore(t, time.Hour, 3)
	e := NewExecutor(s, DefaultNoise())
	sched := demoSchedule(t)

	base := e.RunSequential(context.Background(), Job{Schedule: sched, Shots: 10, Seed: 11, Now: now})

	// g1 第 10 次进入（最后一个 shot）时取消：只剩 shot 9 的尾部未完成。
	ctx, cancel := context.WithCancel(context.Background())
	hook, _ := cancelAfterHook(10, cancel)
	j := Job{Schedule: sched, Shots: 10, Seed: 11, Now: now, Hooks: map[string]Hook{"g1": hook}}
	logJob(t, "interrupt-near-end", j)
	partial := e.RunSequential(ctx, j)
	logResult(t, "partial-near-end", partial)
	cp := partial.Checkpoint
	t.Logf("checkpoint: final-shots=%d/40", cp.finalShots())
	if cp.finalShots() >= 40 {
		t.Fatal("expected the tail to be unfinished")
	}

	resumed, err := e.ResumeSequential(context.Background(), j, cp, ResumeStrict)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	t.Logf("resume decision: %s", resumeDecision(resumed))
	if !resumed.Equivalent(base) {
		t.Fatal("near-end resume must equal uninterrupted baseline")
	}
	if got, want := countEvents(resumed, "reuse", ""), cp.finalShots(); got != want {
		t.Fatalf("reuse=%d want %d", got, want)
	}
	t.Logf("verdict: near-end resume ≡ baseline, reused=%d recomputed=%d",
		countEvents(resumed, "reuse", ""), 40-cp.finalShots())
}

// TestResumeAfterTimeout 验证单门超时打断后的续跑：超时的 shot 与因因果
// 不可达而取消的下游 shot 都被重算，结论与不超时的一次性运行一致。
func TestResumeAfterTimeout(t *testing.T) {
	now := time.Date(2026, 9, 24, 10, 30, 0, 0, time.UTC)
	s := newStore(t, time.Hour, 3)
	e := NewExecutor(s, DefaultNoise())
	sched := demoSchedule(t)

	// 第一次运行：g2 的 hook 一直阻塞直到超时（模拟脉冲卡死）。
	var mu sync.Mutex
	hanging := true
	hooks := map[string]Hook{
		"g2": func(ctx context.Context) error {
			mu.Lock()
			h := hanging
			mu.Unlock()
			if !h {
				return nil
			}
			<-ctx.Done()
			return ctx.Err()
		},
	}
	j := Job{Schedule: sched, Shots: 5, Seed: 9, Now: now, PerTaskTimeout: 30 * time.Millisecond, Hooks: hooks}
	logJob(t, "interrupt-timeout", j)
	partial := e.RunConcurrent(context.Background(), j)
	logResult(t, "partial-timeout", partial)
	if got := partial.Counts()["g2"][StatusTimedOut]; got == 0 {
		t.Fatalf("expected g2 timed-out shots, got %v", partial.Counts()["g2"])
	}
	cp := partial.Checkpoint
	t.Logf("checkpoint: final-shots=%d/20 (g2 超时与下游取消都未完成)", cp.finalShots())

	// 续跑：故障已清除（hook 立即返回），其余条件不变。
	mu.Lock()
	hanging = false
	mu.Unlock()
	resumed, err := e.ResumeConcurrent(context.Background(), j, cp, ResumeStrict)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	logResult(t, "resumed-after-timeout", resumed)
	t.Logf("resume decision: %s", resumeDecision(resumed))

	// 基线：同样的作业但 hook 从不阻塞（从未发生超时的一次性运行）。
	baseHooks := map[string]Hook{"g2": func(ctx context.Context) error { return nil }}
	baseJ := Job{Schedule: sched, Shots: 5, Seed: 9, Now: now, PerTaskTimeout: 30 * time.Millisecond, Hooks: baseHooks}
	base := e.RunConcurrent(context.Background(), baseJ)
	if !resumed.Equivalent(base) {
		t.Fatalf("resume-after-timeout differs from no-fault baseline:\nresumed=%+v\nbase=%+v",
			resumed.Reports, base.Reports)
	}
	if !allFinal(resumed.Checkpoint) {
		t.Fatal("after resume all shots must be final")
	}
	t.Log("verdict: timeout-interrupted resume ≡ no-fault baseline")
}

// TestResumeChain 验证续跑本身再次被中断后，还能从新的断点继续。
func TestResumeChain(t *testing.T) {
	now := time.Date(2026, 9, 24, 10, 30, 0, 0, time.UTC)
	s := newStore(t, time.Hour, 3)
	e := NewExecutor(s, DefaultNoise())
	sched := demoSchedule(t)

	base := e.RunSequential(context.Background(), Job{Schedule: sched, Shots: 9, Seed: 5, Now: now})

	// 第一次中断：g1 第 3 次进入时取消。
	ctx1, cancel1 := context.WithCancel(context.Background())
	h1, _ := cancelAfterHook(3, cancel1)
	j := Job{Schedule: sched, Shots: 9, Seed: 5, Now: now, Hooks: map[string]Hook{"g1": h1}}
	p1 := e.RunSequential(ctx1, j)
	t.Logf("first interrupt: final=%d/36", p1.Checkpoint.finalShots())

	// 第一次续跑又被中断：g2 第 2 次进入时取消。
	ctx2, cancel2 := context.WithCancel(context.Background())
	h2, _ := cancelAfterHook(2, cancel2)
	j.Hooks = map[string]Hook{"g2": h2}
	p2, err := e.ResumeSequential(ctx2, j, p1.Checkpoint, ResumeStrict)
	if err != nil {
		t.Fatalf("first resume: %v", err)
	}
	t.Logf("second interrupt: final=%d/36 decision=%s", p2.Checkpoint.finalShots(), resumeDecision(p2))

	// 第二次续跑跑完。
	j.Hooks = nil
	p3, err := e.ResumeSequential(context.Background(), j, p2.Checkpoint, ResumeStrict)
	if err != nil {
		t.Fatalf("second resume: %v", err)
	}
	t.Logf("final resume decision: %s", resumeDecision(p3))
	if !p3.Equivalent(base) {
		t.Fatal("chained resume must equal uninterrupted baseline")
	}
	if !allFinal(p3.Checkpoint) {
		t.Fatal("chained resume must finish all shots")
	}
	t.Log("verdict: interrupt -> resume -> interrupt -> resume ≡ baseline")
}

// TestResumeMismatchRejected 验证拿错作业/状态续跑被明确挡掉。
func TestResumeMismatchRejected(t *testing.T) {
	now := time.Date(2026, 9, 24, 10, 30, 0, 0, time.UTC)
	s := newStore(t, time.Hour, 3)
	e := NewExecutor(s, DefaultNoise())
	sched := demoSchedule(t)
	j := Job{Schedule: sched, Shots: 4, Seed: 1, Now: now}
	partial := e.RunSequential(context.Background(), j)

	cases := []struct {
		name string
		job  Job
		cp   *Checkpoint
	}{
		{"nil-checkpoint", j, nil},
		{"different-seed", Job{Schedule: sched, Shots: 4, Seed: 2, Now: now}, partial.Checkpoint},
		{"different-shots", Job{Schedule: sched, Shots: 5, Seed: 1, Now: now}, partial.Checkpoint},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := e.ResumeSequential(context.Background(), tc.job, tc.cp, ResumeStrict)
			if err == nil {
				t.Fatal("mismatched resume must be rejected")
			}
			re, ok := err.(*ResumeError)
			if !ok {
				t.Fatalf("error must be *ResumeError, got %T", err)
			}
			t.Logf("input: %s -> rejected reason=%s msg=%s", tc.name, re.Reason, re.Message)
			if re.Reason != ResumeErrMismatch {
				t.Fatalf("reason=%s want checkpoint-mismatch", re.Reason)
			}
		})
	}
	// 另一个作业的断点不能嫁接到本作业。
	other := e.RunSequential(context.Background(), Job{Schedule: sched, Shots: 4, Seed: 99, Now: now})
	_, err := e.ResumeSequential(context.Background(), j, other.Checkpoint, ResumeStrict)
	re := err.(*ResumeError)
	t.Logf("input: foreign-checkpoint -> rejected reason=%s", re.Reason)
	if re.Reason != ResumeErrMismatch {
		t.Fatalf("foreign checkpoint reason=%s want checkpoint-mismatch", re.Reason)
	}
	if !strings.Contains(re.Message, "does not match") {
		t.Fatalf("message should explain mismatch: %s", re.Message)
	}
}

// diamondSchedule 构造复杂依赖拓扑：分支、汇合、以及一个结果同时喂给多个
// 后续门（fan-out）。
//
//	a ──┬── b ──┐
//	    ├── c ──┼── d ── m0
//	    └── m1  ┘
//	c ── m2
func diamondSchedule(t *testing.T) *ir.Schedule {
	t.Helper()
	c, err := compile.NewCompiler(compile.DefaultProtocol(3))
	if err != nil {
		t.Fatal(err)
	}
	gates := []ir.GateTask{
		{ID: "a", Kind: ir.GateX, Qubits: []int{0}},
		{ID: "b", Kind: ir.GateX, Qubits: []int{1}, DependsOn: []string{"a"}},
		{ID: "c", Kind: ir.GateX, Qubits: []int{2}, DependsOn: []string{"a"}},
		{ID: "d", Kind: ir.GateCNOT, Qubits: []int{0, 1}, DependsOn: []string{"b", "c"}},
		{ID: "m0", Kind: ir.GateMeasure, Qubits: []int{0}, DependsOn: []string{"d"}},
		{ID: "m1", Kind: ir.GateMeasure, Qubits: []int{1}, DependsOn: []string{"a"}},
		{ID: "m2", Kind: ir.GateMeasure, Qubits: []int{2}, DependsOn: []string{"c"}},
	}
	s, err := c.Compile(gates)
	if err != nil {
		t.Fatalf("compile diamond: %v", err)
	}
	return s
}

// TestResumeComplexTopology 验证分支/汇合/fan-out 拓扑下：顺序与并发都能
// 正常结束（不卡住）、不重不漏，且中断续跑后的结论与一次性跑完完全一致。
func TestResumeComplexTopology(t *testing.T) {
	now := time.Date(2026, 9, 24, 10, 30, 0, 0, time.UTC)
	s := newStore(t, time.Hour, 3)
	e := NewExecutor(s, DefaultNoise())
	sched := diamondSchedule(t)
	const shots = 12
	j := Job{Schedule: sched, Shots: shots, Seed: 21, Now: now}
	logJob(t, "diamond", j)

	// 基线：两种模式一次性跑完，二者必须等价（复杂拓扑下顺序≡并发）。
	baseSeq := e.RunSequential(context.Background(), j)
	baseCon := e.RunConcurrent(context.Background(), j)
	if !baseSeq.Equivalent(baseCon) {
		t.Fatal("sequential and concurrent baselines differ on diamond topology")
	}
	for _, rep := range baseSeq.Reports {
		if rep.Attempts != shots {
			t.Fatalf("gate %s attempts=%d want %d: no omission/duplication", rep.GateID, rep.Attempts, shots)
		}
	}
	t.Logf("baseline: 7 gates x %d shots all accounted, seq≡con", shots)

	// 并发中断：b 第 5 次进入时取消。分支/汇合下取消会沿多条边级联，
	// 断点必须如实记录每条边已完成的 shot。
	ctx, cancel := context.WithCancel(context.Background())
	hook, _ := cancelAfterHook(5, cancel)
	jj := j
	jj.Hooks = map[string]Hook{"b": hook}
	partial := e.RunConcurrent(ctx, jj)
	logResult(t, "diamond-partial", partial)
	cp := partial.Checkpoint
	t.Logf("checkpoint: final-shots=%d/%d", cp.finalShots(), 7*shots)
	if cp.finalShots() == 0 || cp.finalShots() == 7*shots {
		t.Fatalf("expected partial completion, got %d", cp.finalShots())
	}

	// 并发续跑：必须正常结束且与基线一致。
	resCon, err := e.ResumeConcurrent(context.Background(), jj, cp, ResumeStrict)
	if err != nil {
		t.Fatalf("concurrent resume: %v", err)
	}
	t.Logf("concurrent resume decision: %s", resumeDecision(resCon))
	if !resCon.Equivalent(baseSeq) {
		t.Fatalf("concurrent resume on diamond differs from baseline:\nres=%+v\nbase=%+v",
			resCon.Reports, baseSeq.Reports)
	}

	// 顺序续跑同一个断点：结论也必须一致（续跑与模式无关）。
	resSeq, err := e.ResumeSequential(context.Background(), jj, cp, ResumeStrict)
	if err != nil {
		t.Fatalf("sequential resume: %v", err)
	}
	if !resSeq.Equivalent(baseSeq) {
		t.Fatal("sequential resume on diamond differs from baseline")
	}
	if !resSeq.Equivalent(resCon) {
		t.Fatal("sequential and concurrent resumes of the same checkpoint differ")
	}
	for _, rep := range resSeq.Reports {
		if rep.Attempts != shots {
			t.Fatalf("resumed gate %s attempts=%d want %d", rep.GateID, rep.Attempts, shots)
		}
	}
	if !allFinal(resCon.Checkpoint) || !allFinal(resSeq.Checkpoint) {
		t.Fatal("diamond resume must finish every shot of every gate")
	}
	t.Log("verdict: diamond topology resumes terminate, no stuck/omission/duplication, both modes ≡ baseline")
}

// interruptedMiddle 构造一个“执行到一半被取消”的断点，供校准兼容性测试复用。
func interruptedMiddle(t *testing.T, e *Executor, j Job) *Checkpoint {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	hook, _ := cancelAfterHook(3, cancel)
	jj := j
	jj.Hooks = map[string]Hook{"g1": hook}
	partial := e.RunSequential(ctx, jj)
	logResult(t, "partial-for-cal", partial)
	cp := partial.Checkpoint
	if cp == nil || cp.finalShots() == 0 {
		t.Fatal("expected a partially completed checkpoint")
	}
	return cp
}

// TestResumeCalibrationRollbackSameStable 验证：中断期间只是“提交了新版本又
// 回滚到同一个稳定版本”时，head 与断点版本一致，续跑被接受且结论与最早那次
// 一次性运行完全一致。
func TestResumeCalibrationRollbackSameStable(t *testing.T) {
	now := time.Date(2026, 9, 24, 10, 30, 0, 0, time.UTC)
	s := newStore(t, time.Hour, 3)
	e := NewExecutor(s, DefaultNoise())
	sched := demoSchedule(t)
	j := Job{Schedule: sched, Shots: 8, Seed: 13, Now: now}
	logJob(t, "rollback-same-stable", j)

	base := e.RunSequential(context.Background(), j)
	cp := interruptedMiddle(t, e, j)
	t.Logf("checkpoint calVersion=%s final=%d/32", cp.CalVersion, cp.finalShots())

	// 中断期间：提交容差内的新版本（head 移走），随后回滚到同一个稳定版本。
	v1, err := s.Commit(calibration.Candidate{
		Parameters: map[string]map[string]float64{
			calibration.GlobalLayer: {"amp": 0.51, "frequency": 5001},
		},
		ReadoutFid: map[int]float64{0: 0.9, 1: 0.9, 2: 0.9},
		Note:       "refresh during interruption",
	}, now)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	t.Logf("during interruption: committed %s, head=%s", v1.ID, s.CurrentID())
	rb, err := s.Rollback(now)
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	t.Logf("during interruption: rolled back to %s (head=%s)", rb.ID, s.CurrentID())

	resumed, err := e.ResumeSequential(context.Background(), j, cp, ResumeStrict)
	if err != nil {
		t.Fatalf("resume after rollback to same stable must be accepted: %v", err)
	}
	t.Logf("resume decision: %s", resumeDecision(resumed))
	if !resumed.Equivalent(base) {
		t.Fatal("resume after rollback-to-same-stable must equal the original uninterrupted run")
	}
	t.Log("verdict: rollback to same stable version => resume accepted, result ≡ original")
}

// TestResumeCalibrationNewStableStrictRejected 验证：中断期间出现了新的稳定
// 版本时，严格模式拒绝续跑，原因可区分（calibration-changed），绝不把旧校准
// 结果与新校准结果悄悄拼在一起。
func TestResumeCalibrationNewStableStrictRejected(t *testing.T) {
	now := time.Date(2026, 9, 24, 10, 30, 0, 0, time.UTC)
	s := newStore(t, time.Hour, 3)
	e := NewExecutor(s, DefaultNoise())
	j := Job{Schedule: demoSchedule(t), Shots: 8, Seed: 13, Now: now}
	logJob(t, "new-stable-strict", j)

	cp := interruptedMiddle(t, e, j)
	v1, err := s.Commit(calibration.Candidate{
		Parameters: map[string]map[string]float64{
			calibration.GlobalLayer: {"amp": 0.52, "frequency": 5002},
		},
		ReadoutFid: map[int]float64{0: 0.9, 1: 0.9, 2: 0.9},
		Note:       "new stable during interruption",
	}, now)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := s.MarkStable(now); err != nil {
		t.Fatalf("mark stable: %v", err)
	}
	t.Logf("during interruption: new stable %s, head moved %s -> %s", v1.ID, cp.CalVersion, s.CurrentID())

	_, err = e.ResumeSequential(context.Background(), j, cp, ResumeStrict)
	if err == nil {
		t.Fatal("strict resume must be rejected when calibration moved to a new stable version")
	}
	re, ok := err.(*ResumeError)
	if !ok {
		t.Fatalf("error must be *ResumeError, got %T", err)
	}
	t.Logf("rejected: reason=%s msg=%s", re.Reason, re.Message)
	if re.Reason != ResumeErrCalChanged {
		t.Fatalf("reason=%s want calibration-changed", re.Reason)
	}
	t.Log("verdict: strict policy refuses to mix old/new calibration results")
}

// TestResumeCalibrationRecomputeAffected 验证：校准变化选择重算模式时，只有
// 受影响的门被重算，未受影响的门复用旧结果；最终结论与“在新校准下一次性
// 跑完”完全一致。
func TestResumeCalibrationRecomputeAffected(t *testing.T) {
	now := time.Date(2026, 9, 24, 10, 30, 0, 0, time.UTC)
	s := newStore(t, time.Hour, 3)
	e := NewExecutor(s, DefaultNoise())
	sched := demoSchedule(t)
	j := Job{Schedule: sched, Shots: 10, Seed: 17, Now: now}
	logJob(t, "recompute-affected", j)

	// 旧校准下的基线（仅用于观察 m0 比特确实被新校准改变）。
	baseOld := e.RunSequential(context.Background(), j)
	cp := interruptedMiddle(t, e, j)
	t.Logf("checkpoint calVersion=%s final=%d/40", cp.CalVersion, cp.finalShots())

	// 中断期间：新稳定版本把比特 0 的读出保真度 0.9 -> 0.1（参数不变）。
	// 只有测量比特 0 的 m0 受影响。
	v1, err := s.Commit(calibration.Candidate{
		Parameters: map[string]map[string]float64{
			calibration.GlobalLayer: {"amp": 0.5, "frequency": 5000},
		},
		ReadoutFid: map[int]float64{0: 0.1, 1: 0.9, 2: 0.9},
		Note:       "readout fid q0 flipped",
	}, now)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := s.MarkStable(now); err != nil {
		t.Fatalf("mark stable: %v", err)
	}
	t.Logf("during interruption: new stable %s with readout fid q0 0.9->0.1", v1.ID)

	resumed, err := e.ResumeSequential(context.Background(), j, cp, ResumeRecomputeAffected)
	if err != nil {
		t.Fatalf("recompute-affected resume: %v", err)
	}
	logResult(t, "resumed-recompute", resumed)
	decision := resumeDecision(resumed)
	t.Logf("resume decision: %s", decision)
	if !strings.Contains(decision, "affected-gates=[m0]") {
		t.Fatalf("decision must identify m0 as the only affected gate: %s", decision)
	}

	// 新校准下的一次性基线：续跑结论必须与之逐项一致。
	baseNew := e.RunSequential(context.Background(), j)
	logResult(t, "baseline-new-cal", baseNew)
	if !resumed.Equivalent(baseNew) {
		t.Fatal("recompute-affected resume must equal an uninterrupted run under the new calibration")
	}
	// 未受影响门复用旧结果：g1/g2/g3 有 reuse 事件，m0 没有。
	for _, g := range []string{"g1", "g2", "g3"} {
		if countEvents(resumed, "reuse", g) == 0 {
			t.Fatalf("unaffected gate %s should reuse checkpoint results", g)
		}
	}
	if got := countEvents(resumed, "reuse", "m0"); got != 0 {
		t.Fatalf("affected gate m0 must be fully recomputed, got %d reuse events", got)
	}
	// 旧基线与新结论的 m0 多数票比特应当不同（保真度翻转确实生效）。
	var oldBit, newBit int
	for _, rep := range baseOld.Reports {
		if rep.GateID == "m0" {
			oldBit = rep.Bit
		}
	}
	for _, rep := range resumed.Reports {
		if rep.GateID == "m0" {
			newBit = rep.Bit
		}
	}
	t.Logf("m0 majority bit: old-cal=%d resumed(new-cal)=%d", oldBit, newBit)
	if oldBit == newBit {
		t.Fatal("readout fidelity flip must change m0 verdict, otherwise the test proves nothing")
	}
	t.Log("verdict: affected gate recomputed under new calibration, unaffected gates reused, result ≡ new-cal baseline")
}

// TestResumeCalibrationExpiredRejected 验证：中断期间校准过期时续跑被拒绝，
// 原因可区分（calibration-expired），不会产出“旧结果+校准失败”的拼接结论。
func TestResumeCalibrationExpiredRejected(t *testing.T) {
	start := time.Date(2026, 9, 24, 10, 30, 0, 0, time.UTC)
	s := newStore(t, time.Hour, 3)
	e := NewExecutor(s, DefaultNoise())
	sched := demoSchedule(t)
	j := Job{Schedule: sched, Shots: 6, Seed: 19, Now: start}
	logJob(t, "expired-at-resume", j)

	cp := interruptedMiddle(t, e, j)
	t.Logf("checkpoint calVersion=%s final=%d/24", cp.CalVersion, cp.finalShots())

	// 续跑时刻已越过 TTL（版本未变，但已过期）。
	later := start.Add(2 * time.Hour)
	jLater := j
	jLater.Now = later
	_, err := e.ResumeSequential(context.Background(), jLater, cp, ResumeStrict)
	if err == nil {
		t.Fatal("resume with expired calibration must be rejected")
	}
	re := err.(*ResumeError)
	t.Logf("rejected: reason=%s msg=%s", re.Reason, re.Message)
	if re.Reason != ResumeErrCalExpired {
		t.Fatalf("reason=%s want calibration-expired", re.Reason)
	}
	// 重算模式同样被拒绝：过期不是“受影响范围”问题。
	_, err = e.ResumeSequential(context.Background(), jLater, cp, ResumeRecomputeAffected)
	re2 := err.(*ResumeError)
	t.Logf("recompute policy also rejected: reason=%s", re2.Reason)
	if re2.Reason != ResumeErrCalExpired {
		t.Fatalf("recompute policy reason=%s want calibration-expired", re2.Reason)
	}
	t.Log("verdict: expired calibration at resume time => rejected under both policies")
}
