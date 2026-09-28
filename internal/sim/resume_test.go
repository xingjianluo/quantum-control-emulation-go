package sim

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xingjianluo/quantum-control-emulation-go/internal/calibration"
	"github.com/xingjianluo/quantum-control-emulation-go/internal/compile"
	"github.com/xingjianluo/quantum-control-emulation-go/internal/ir"
)

// complexSchedule 构造含分支、汇合与扇出的复杂依赖拓扑：
//
//	a0 a1 a2        (三个根，分支)
//	 |  |  |
//	 +--+  b1       (b0 汇合 a0+a1)
//	 |     |
//	b0     |
//	|\     |
//	c0 c1  c2       (b0 扇出到 c0/c1)
//	|  |   |
//	d0 <---+        (d0 跨分支汇合 c0+c2)
//	|  |
//	m0 m1           (两个测量，共享测量总线)
func complexSchedule(t *testing.T) *ir.Schedule {
	t.Helper()
	c, err := compile.NewCompiler(compile.DefaultProtocol(3))
	if err != nil {
		t.Fatal(err)
	}
	gates := []ir.GateTask{
		{ID: "a0", Kind: ir.GateX, Qubits: []int{0}},
		{ID: "a1", Kind: ir.GateH, Qubits: []int{1}},
		{ID: "a2", Kind: ir.GateZ, Qubits: []int{2}},
		{ID: "b0", Kind: ir.GateCNOT, Qubits: []int{0, 1}, DependsOn: []string{"a0", "a1"}},
		{ID: "b1", Kind: ir.GateY, Qubits: []int{2}, DependsOn: []string{"a2"}},
		{ID: "c0", Kind: ir.GateZ, Qubits: []int{0}, DependsOn: []string{"b0"}},
		{ID: "c1", Kind: ir.GateX, Qubits: []int{1}, DependsOn: []string{"b0"}},
		{ID: "c2", Kind: ir.GateH, Qubits: []int{2}, DependsOn: []string{"b1"}},
		{ID: "d0", Kind: ir.GateCNOT, Qubits: []int{0, 2}, DependsOn: []string{"c0", "c2"}},
		{ID: "m0", Kind: ir.GateMeasure, Qubits: []int{0}, DependsOn: []string{"d0"}},
		{ID: "m1", Kind: ir.GateMeasure, Qubits: []int{1}, DependsOn: []string{"c1"}},
	}
	s, err := c.Compile(gates)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return s
}

// cancelHooks 为所有门安装共享计数 hook：第 cancelAt 次进入时取消整个作业。
func cancelHooks(gateIDs []string, cancelAt int64, cancel context.CancelFunc) (map[string]Hook, *atomic.Int64) {
	var entries atomic.Int64
	h := func(ctx context.Context) error {
		if entries.Add(1) == cancelAt {
			cancel()
		}
		return nil
	}
	out := make(map[string]Hook, len(gateIDs))
	for _, g := range gateIDs {
		out[g] = h
	}
	return out, &entries
}

func logResume(t *testing.T, name string, res *JobResult, cp *Checkpoint) {
	t.Helper()
	for _, ev := range res.Events {
		if ev.Kind == "resume-decision" {
			t.Logf("[%s] %s", name, ev.Note)
		}
	}
	reuseEv := 0
	for _, ev := range res.Events {
		if ev.Kind == "reuse" {
			reuseEv++
		}
	}
	t.Logf("[%s] reused=%d recomputed=%d reuse-events=%d", name, res.Reused, res.Recomputed, reuseEv)
	if cp != nil {
		final, pending := 0, 0
		for _, recs := range cp.Records {
			for _, r := range recs {
				if r.Final {
					final++
				} else {
					pending++
				}
			}
		}
		t.Logf("[%s] new checkpoint: final=%d pending=%d", name, final, pending)
	}
}

// assertEquivalentToReference 校验续跑合并结果与一次性不间断参考运行逐项一致：
// 聚合报告（每 shot 状态、多数票比特、尝试次数）与各类计数完全相等。
func assertEquivalentToReference(t *testing.T, name string, got, ref *JobResult) {
	t.Helper()
	if !got.Equivalent(ref) {
		t.Fatalf("[%s] resumed result differs from uninterrupted reference:\ngot=%+v\nref=%+v", name, got.Reports, ref.Reports)
	}
	if !reflect.DeepEqual(got.Counts(), ref.Counts()) {
		t.Fatalf("[%s] counts differ:\ngot=%v\nref=%v", name, got.Counts(), ref.Counts())
	}
	t.Logf("[%s] verdict basis: reports & per-status counts identical to uninterrupted run", name)
}

// assertCheckpointInvariant 校验检查点不变量：非终态记录只能是 cancelled/timed-out
// （被打断未跑完），终态记录只能是 ok/measured/failed（含真正的执行失败）。
func assertCheckpointInvariant(t *testing.T, name string, cp *Checkpoint) {
	t.Helper()
	for gate, recs := range cp.Records {
		for i, r := range recs {
			if r.Final && !isFinalStatus(r.Status) {
				t.Fatalf("[%s] gate=%s shot=%d final record with non-final status %s", name, gate, i, r.Status)
			}
			if !r.Final && r.Status != StatusCancelled && r.Status != StatusTimedOut {
				t.Fatalf("[%s] gate=%s shot=%d pending record with unexpected status %s", name, gate, i, r.Status)
			}
		}
	}
}

func TestResumeAfterCancelAtStartMiddleEnd(t *testing.T) {
	base := time.Date(2026, 9, 24, 10, 30, 0, 0, time.UTC)
	sched := complexSchedule(t)
	const shots = 8
	total := int64(len(sched.GateIDs) * shots)

	cases := []struct {
		name     string
		cancelAt int64
	}{
		{"at-start", 1},
		{"middle", total / 2},
		{"near-end", total - 1},
	}
	for _, tc := range cases {
		for _, concurrent := range []bool{false, true} {
			mode := "sequential"
			if concurrent {
				mode = "concurrent"
			}
			name := fmt.Sprintf("%s/%s", tc.name, mode)
			t.Run(name, func(t *testing.T) {
				s := newStore(t, time.Hour, 3)
				e := NewExecutor(s, DefaultNoise())

				ctx, cancel := context.WithCancel(context.Background())
				hooks, _ := cancelHooks(sched.GateIDs, tc.cancelAt, cancel)
				j := Job{ID: "job-" + name, Schedule: sched, Shots: shots, Seed: 77, Now: base, Hooks: hooks}
				logJob(t, name+" interrupted", j)

				first, cp := e.RunResumable(ctx, j, concurrent)
				logResult(t, name+" interrupted", first)
				assertCheckpointInvariant(t, name, cp)

				// 续跑：中断条件已解除（去掉 hook、换用新 ctx），作业身份不变。
				j2 := Job{ID: j.ID, Schedule: sched, Shots: shots, Seed: 77, Now: base}
				res, cp2, err := e.Resume(context.Background(), j2, cp, ResumeStrict, concurrent)
				if err != nil {
					t.Fatalf("resume rejected unexpectedly: %v", err)
				}
				logResume(t, name+" resumed", res, cp2)
				logResult(t, name+" resumed", res)

				// 参考：同一作业一次性不间断跑完。
				var ref *JobResult
				if concurrent {
					ref = e.RunConcurrent(context.Background(), j2)
				} else {
					ref = e.RunSequential(context.Background(), j2)
				}
				assertEquivalentToReference(t, name, res, ref)

				if res.Reused+res.Recomputed != int(total) {
					t.Fatalf("accounting broken: reused=%d recomputed=%d total=%d", res.Reused, res.Recomputed, total)
				}
				// 顺序模式下取消点确定，必然有未跑完的部分；并发模式下取消
				// 可能落在全部完成之后（near-end 时尤其如此），不做强求。
				if !concurrent && res.Recomputed == 0 {
					t.Fatalf("expected some shots to be recomputed after interrupt at %d", tc.cancelAt)
				}
				// 续跑后的新检查点应全部终态（这次跑完了）。
				for gate, recs := range cp2.Records {
					for i, r := range recs {
						if !r.Final {
							t.Fatalf("gate=%s shot=%d still pending after full resume", gate, i)
						}
					}
				}
				// 日志可区分复用与重算。
				sawReuse, sawDecision := false, false
				for _, ev := range res.Events {
					if ev.Kind == "reuse" {
						sawReuse = true
					}
					if ev.Kind == "resume-decision" {
						sawDecision = true
					}
				}
				if res.Reused > 0 && !sawReuse {
					t.Fatal("expected reuse events marking reused shots")
				}
				if !sawDecision {
					t.Fatal("expected resume-decision event explaining the resume")
				}
			})
		}
	}
}

func TestResumeAfterTimeoutConcurrent(t *testing.T) {
	base := time.Date(2026, 9, 24, 10, 30, 0, 0, time.UTC)
	sched := complexSchedule(t)
	const shots = 6
	s := newStore(t, time.Hour, 3)
	e := NewExecutor(s, DefaultNoise())

	// a1 恒失败（真正的执行失败，终态）；b0 阻塞直到门超时（被打断，非终态）。
	hooks := map[string]Hook{
		"a1": func(ctx context.Context) error { return errors.New("hardware fault") },
		"b0": func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() },
	}
	j := Job{
		ID: "job-timeout", Schedule: sched, Shots: shots, Seed: 91, Now: base,
		PerTaskTimeout: 30 * time.Millisecond, Hooks: hooks,
	}
	logJob(t, "timeout-interrupt", j)
	first, cp := e.RunResumable(context.Background(), j, true)
	logResult(t, "timeout-interrupt", first)
	assertCheckpointInvariant(t, "timeout-interrupt", cp)

	// 检查点必须区分三类记录：failed(终态) / timed-out(待重算) / cancelled(待重算)。
	var failedFinal, timedOutPending, cancelledPending int
	for gate, recs := range cp.Records {
		for _, r := range recs {
			switch {
			case gate == "a1" && r.Final && r.Status == StatusFailed:
				failedFinal++
			case gate == "b0" && !r.Final && r.Status == StatusTimedOut:
				timedOutPending++
			case !r.Final && r.Status == StatusCancelled:
				cancelledPending++
			}
		}
	}
	t.Logf("checkpoint categories: failed-final=%d timed-out-pending=%d cancelled-pending=%d",
		failedFinal, timedOutPending, cancelledPending)
	if failedFinal != shots {
		t.Fatalf("a1 should have %d final failed records, got %d", shots, failedFinal)
	}
	if timedOutPending == 0 {
		t.Fatal("b0 should have pending timed-out records")
	}
	if cancelledPending == 0 {
		t.Fatal("downstream should have pending cancelled records")
	}

	// 续跑：超时条件解除（b0 不再阻塞），a1 的故障仍在（验证失败终态被复用而非重算）。
	j2 := Job{
		ID: j.ID, Schedule: sched, Shots: shots, Seed: 91, Now: base,
		Hooks: map[string]Hook{"a1": hooks["a1"]},
	}
	res, _, err := e.Resume(context.Background(), j2, cp, ResumeStrict, true)
	if err != nil {
		t.Fatalf("resume rejected unexpectedly: %v", err)
	}
	logResume(t, "timeout-resumed", res, nil)
	logResult(t, "timeout-resumed", res)

	ref := e.RunConcurrent(context.Background(), j2)
	assertEquivalentToReference(t, "timeout-resumed", res, ref)
	// 复用量 == 检查点中全部终态记录数（含 a1 的 failed 终态与未受影响分支）；
	// 超时的 b0 与被级联取消的下游必须全部被重算。
	wantReused := 0
	for _, recs := range cp.Records {
		for _, r := range recs {
			if r.Final {
				wantReused++
			}
		}
	}
	if res.Reused != wantReused {
		t.Fatalf("reused=%d want %d (all final checkpoint records)", res.Reused, wantReused)
	}
	if res.Recomputed != timedOutPending+cancelledPending {
		t.Fatalf("recomputed=%d want %d (timed-out + cancelled pendings)", res.Recomputed, timedOutPending+cancelledPending)
	}
}

func TestComplexTopologySequentialConcurrentEquivalent(t *testing.T) {
	base := time.Date(2026, 9, 24, 10, 30, 0, 0, time.UTC)
	s := newStore(t, time.Hour, 3)
	e := NewExecutor(s, DefaultNoise())
	sched := complexSchedule(t)
	const shots = 40
	j := Job{ID: "job-topo", Schedule: sched, Shots: shots, Seed: 555, Now: base}
	logJob(t, "complex-topo", j)

	seq := e.RunSequential(context.Background(), j)
	con := e.RunConcurrent(context.Background(), j)
	logResult(t, "complex-seq", seq)
	logResult(t, "complex-con", con)

	if !seq.Equivalent(con) {
		t.Fatalf("complex topology: sequential vs concurrent differ:\nseq=%+v\ncon=%+v", seq.Reports, con.Reports)
	}
	// 分支/汇合/扇出拓扑必须正常结束：每门每 shot 恰好一个结果，不重不漏。
	for _, rep := range seq.Reports {
		if rep.Attempts != shots {
			t.Fatalf("gate %s attempts=%d want %d (stuck/dropped/duplicated work)", rep.GateID, rep.Attempts, shots)
		}
	}
	if len(seq.Reports) != len(sched.GateIDs) {
		t.Fatalf("reports=%d gates=%d", len(seq.Reports), len(sched.GateIDs))
	}
	t.Log("verdict basis: branch/join/fan-out DAG terminates; per-(gate,shot) outcomes identical across modes")
}

func TestResumeRejectedOnCalibrationChange(t *testing.T) {
	base := time.Date(2026, 9, 24, 10, 30, 0, 0, time.UTC)
	sched := complexSchedule(t)
	const shots = 8

	setup := func(t *testing.T) (*Executor, *calibration.Store, *Checkpoint, Job) {
		s := newStore(t, time.Hour, 3)
		e := NewExecutor(s, DefaultNoise())
		ctx, cancel := context.WithCancel(context.Background())
		hooks, _ := cancelHooks(sched.GateIDs, 10, cancel)
		j := Job{ID: "job-cal", Schedule: sched, Shots: shots, Seed: 31, Now: base, Hooks: hooks}
		_, cp := e.RunResumable(ctx, j, false)
		j2 := Job{ID: j.ID, Schedule: sched, Shots: shots, Seed: 31, Now: base}
		return e, s, cp, j2
	}

	t.Run("new-stable-version strict rejects", func(t *testing.T) {
		e, s, cp, j2 := setup(t)
		// 中断期间提交并稳定化一个参数不同的新版本。
		v, err := s.Commit(calibration.Candidate{
			Parameters: map[string]map[string]float64{calibration.GlobalLayer: {"amp": 0.55, "frequency": 5050}},
			ReadoutFid: map[int]float64{0: 0.9, 1: 0.9, 2: 0.9},
			Note:       "recalibrated during interruption",
		}, base.Add(time.Minute))
		if err != nil {
			t.Fatalf("commit: %v", err)
		}
		if err := s.MarkStable(base.Add(time.Minute)); err != nil {
			t.Fatalf("mark stable: %v", err)
		}
		t.Logf("input: checkpoint calibration id=%s fp=%016x; new stable=%s", cp.CalibrationID, cp.CalibrationFP, v.ID)

		_, _, err = e.Resume(context.Background(), j2, cp, ResumeStrict, false)
		var rerr *ResumeError
		if !errors.As(err, &rerr) {
			t.Fatalf("expected *ResumeError, got %v", err)
		}
		if rerr.Reason != ResumeCalibrationChanged {
			t.Fatalf("reason=%s want %s", rerr.Reason, ResumeCalibrationChanged)
		}
		t.Logf("verdict: resume rejected, no old/new results merged: %v", rerr)
	})

	t.Run("global-change recompute reruns all gates", func(t *testing.T) {
		e, s, cp, j2 := setup(t)
		if _, err := s.Commit(calibration.Candidate{
			Parameters: map[string]map[string]float64{calibration.GlobalLayer: {"amp": 0.55, "frequency": 5050}},
			ReadoutFid: map[int]float64{0: 0.9, 1: 0.9, 2: 0.9},
		}, base.Add(time.Minute)); err != nil {
			t.Fatalf("commit: %v", err)
		}
		if err := s.MarkStable(base.Add(time.Minute)); err != nil {
			t.Fatalf("mark stable: %v", err)
		}

		res, _, err := e.Resume(context.Background(), j2, cp, ResumeRecompute, false)
		if err != nil {
			t.Fatalf("recompute policy must not reject: %v", err)
		}
		logResume(t, "recompute-global", res, nil)
		// 全局层参数变化触及每个门：所有终态旧结果都必须重算，只允许复用
		// 不存在（终态记录在中断点前均来自同一全局层，全部失效）。
		if res.Reused != 0 {
			t.Fatalf("global calibration change must invalidate every gate, reused=%d", res.Reused)
		}
		// 重算结论 == 当前校准下全新一次性运行（顺序模式每 shot 都实际执行）。
		ref := e.RunSequential(context.Background(), j2)
		assertEquivalentToReference(t, "recompute-global", res, ref)
		sawDecision := false
		for _, ev := range res.Events {
			if ev.Kind == "resume-decision" {
				sawDecision = true
				t.Logf("decision event: %s", ev.Note)
			}
		}
		if !sawDecision {
			t.Fatal("expected resume-decision event explaining recompute scope")
		}
	})

	t.Run("expired calibration rejects under both policies", func(t *testing.T) {
		for _, policy := range []ResumePolicy{ResumeStrict, ResumeRecompute} {
			e, _, cp, j2 := setup(t)
			j2.Now = base.Add(2 * time.Hour) // 超过 1h TTL
			name := "strict"
			if policy == ResumeRecompute {
				name = "recompute"
			}
			t.Run(name, func(t *testing.T) {
				t.Logf("input: resume at %s with ttl=1h policy=%s (checkpoint id=%s)",
					j2.Now, name, cp.CalibrationID)
				res, _, err := e.Resume(context.Background(), j2, cp, policy, false)
				var rerr *ResumeError
				if !errors.As(err, &rerr) {
					t.Fatalf("expected *ResumeError, got res=%v err=%v", res, err)
				}
				if rerr.Reason != ResumeCalibrationUnavailable {
					t.Fatalf("reason=%s want %s", rerr.Reason, ResumeCalibrationUnavailable)
				}
				if res != nil {
					t.Fatalf("expired calibration must not yield a (all-failed) result, got %+v", res.Reports)
				}
				t.Logf("verdict: expired calibration rejected distinctly under %s: %v", name, rerr)
			})
		}
	})

	t.Run("rollback to same stable stays compatible", func(t *testing.T) {
		for _, concurrent := range []bool{false, true} {
			mode := "sequential"
			if concurrent {
				mode = "concurrent"
			}
			t.Run(mode, func(t *testing.T) {
				e, s, cp, j2 := setup(t)
				// 中断期间：提交新版本（head 移走），随后回滚到同一个稳定版本。
				if _, err := s.Commit(calibration.Candidate{
					Parameters: map[string]map[string]float64{calibration.GlobalLayer: {"amp": 0.55, "frequency": 5050}},
					ReadoutFid: map[int]float64{0: 0.9, 1: 0.9, 2: 0.9},
				}, base.Add(time.Minute)); err != nil {
					t.Fatalf("commit: %v", err)
				}
				rb, err := s.Rollback(base.Add(2 * time.Minute))
				if err != nil {
					t.Fatalf("rollback: %v", err)
				}
				t.Logf("input: rolled back to %s; checkpoint calibration id=%s", rb.ID, cp.CalibrationID)
				if rb.ID != cp.CalibrationID {
					t.Fatalf("test setup: rollback target %s != checkpoint calibration %s", rb.ID, cp.CalibrationID)
				}

				j2.Now = base.Add(3 * time.Minute)
				res, _, err := e.Resume(context.Background(), j2, cp, ResumeStrict, concurrent)
				if err != nil {
					t.Fatalf("rollback to same stable must stay compatible: %v", err)
				}
				logResume(t, "rollback-compatible/"+mode, res, nil)
				// 有效参数与中断点逐键一致：全部终态结果都应复用，没有门被判重算。
				wantReused := 0
				for _, recs := range cp.Records {
					for _, r := range recs {
						if r.Final {
							wantReused++
						}
					}
				}
				if res.Reused != wantReused {
					t.Fatalf("rollback must reuse every final outcome: reused=%d want %d", res.Reused, wantReused)
				}
				// 与最早那次（中断点校准状态下）一次性跑完的结论一致。
				var ref *JobResult
				refJob := Job{ID: j2.ID, Schedule: sched, Shots: shots, Seed: 31, Now: base}
				if concurrent {
					ref = e.RunConcurrent(context.Background(), refJob)
				} else {
					ref = e.RunSequential(context.Background(), refJob)
				}
				assertEquivalentToReference(t, "rollback-compatible/"+mode, res, ref)
			})
		}
	})
}

func TestResumeRejectsMismatchedJob(t *testing.T) {
	base := time.Date(2026, 9, 24, 10, 30, 0, 0, time.UTC)
	s := newStore(t, time.Hour, 3)
	e := NewExecutor(s, DefaultNoise())
	sched := complexSchedule(t)
	j := Job{ID: "job-a", Schedule: sched, Shots: 8, Seed: 11, Now: base}
	_, cp := e.RunResumable(context.Background(), j, false)

	other := demoSchedule(t)
	cases := []struct {
		name string
		cp   *Checkpoint
		job  Job
	}{
		{"nil-checkpoint", nil, j},
		{"wrong-job-id", cp, Job{ID: "job-b", Schedule: sched, Shots: 8, Seed: 11, Now: base}},
		{"wrong-shots", cp, Job{ID: "job-a", Schedule: sched, Shots: 9, Seed: 11, Now: base}},
		{"wrong-seed", cp, Job{ID: "job-a", Schedule: sched, Shots: 8, Seed: 12, Now: base}},
		{"wrong-schedule", cp, Job{ID: "job-a", Schedule: other, Shots: 8, Seed: 11, Now: base}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logJob(t, "mismatch/"+tc.name, tc.job)
			res, _, err := e.Resume(context.Background(), tc.job, tc.cp, ResumeStrict, false)
			var rerr *ResumeError
			if !errors.As(err, &rerr) {
				t.Fatalf("expected *ResumeError, got res=%v err=%v", res, err)
			}
			if rerr.Reason != ResumeMismatchJob {
				t.Fatalf("reason=%s want %s", rerr.Reason, ResumeMismatchJob)
			}
			if res != nil {
				t.Fatalf("rejected resume must not produce a result, got %+v", res.Reports)
			}
			t.Logf("verdict: mismatched resume explicitly rejected: %v", rerr)
		})
	}
}
