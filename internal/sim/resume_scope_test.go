package sim

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xingjianluo/quantum-control-emulation-go/internal/calibration"
	"github.com/xingjianluo/quantum-control-emulation-go/internal/ir"
)

// newStoreWideTolerance 与 newStore 相同，但把 amp 容差放宽，允许构造
// q1.ctrl 幅度显著变化的新校准版本（用于按资源范围重算测试）。
func newStoreWideTolerance(t *testing.T) *calibration.Store {
	t.Helper()
	s := calibration.NewStore(time.Hour, 3,
		map[string]float64{"amp": 1.0, "frequency": 100},
		[]string{"amp"})
	err := s.Seed(calibration.Version{
		ID:         "v-stable",
		CreatedAt:  time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC),
		Parameters: map[string]map[string]float64{calibration.GlobalLayer: {"amp": 0.5, "frequency": 5000}},
		ReadoutFid: map[int]float64{0: 0.9, 1: 0.9, 2: 0.9},
		Note:       "test",
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	return s
}

// gateReuseEvents 统计结果中每个门的 reuse 事件数。
func gateReuseEvents(res *JobResult) map[string]int {
	out := make(map[string]int)
	for _, ev := range res.Events {
		if ev.Kind == "reuse" {
			out[ev.GateID]++
		}
	}
	return out
}

// assertCheckpointsPerShotEqual 逐 (门, shot) 校验两个检查点的状态与测量
// 比特完全一致，用于证明“续跑结论 == 新校准下不间断跑完”。
func assertCheckpointsPerShotEqual(t *testing.T, name string, got, want *Checkpoint) {
	t.Helper()
	for g, wantRecs := range want.Records {
		gotRecs, ok := got.Records[g]
		if !ok {
			t.Fatalf("[%s] gate %s missing from resumed checkpoint", name, g)
		}
		if len(gotRecs) != len(wantRecs) {
			t.Fatalf("[%s] gate %s shot count %d != %d", name, g, len(gotRecs), len(wantRecs))
		}
		for i := range wantRecs {
			if gotRecs[i].Status != wantRecs[i].Status || gotRecs[i].Bit != wantRecs[i].Bit {
				t.Fatalf("[%s] gate %s shot %d resumed=(%s,bit=%d) reference=(%s,bit=%d)",
					name, g, i, gotRecs[i].Status, gotRecs[i].Bit, wantRecs[i].Status, wantRecs[i].Bit)
			}
		}
	}
}

// interruptAt 在 sched 上执行作业，在第 cancelAt 次进入门时取消，
// 返回中断检查点与解除中断条件后的续跑作业（Now 仍为 base）。
func interruptAt(t *testing.T, e *Executor, sched *ir.Schedule, shots int, seed int64,
	base time.Time, cancelAt int, concurrent bool, jobID string) (*Checkpoint, Job) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	hooks, _ := cancelHooks(sched.GateIDs, int64(cancelAt), cancel)
	j := Job{ID: jobID, Schedule: sched, Shots: shots, Seed: seed, Now: base, Hooks: hooks}
	_, cp := e.RunResumable(ctx, j, concurrent)
	assertCheckpointInvariant(t, jobID, cp)
	j2 := Job{ID: j.ID, Schedule: sched, Shots: shots, Seed: seed, Now: base}
	return cp, j2
}

// countFinalIn 返回检查点中给定门集合内的终态记录数。
func countFinalIn(cp *Checkpoint, gates map[string]bool) int {
	n := 0
	for g, recs := range cp.Records {
		if gates[g] {
			for _, r := range recs {
				if r.Final {
					n++
				}
			}
		}
	}
	return n
}

func gateIDSet(s *ir.Schedule) map[string]bool {
	out := make(map[string]bool, len(s.GateIDs))
	for _, g := range s.GateIDs {
		out[g] = true
	}
	return out
}

// TestResumeScopedRecomputeControlResource 校准变化只触及 q1.ctrl：
// 只有拥有 q1.ctrl 脉冲的门重算，其他门（含测量门 m1，因其读出保真度未变）
// 继续复用；严格策略拒绝；顺序/并发结论都与不间断运行逐项一致。
func TestResumeScopedRecomputeControlResource(t *testing.T) {
	base := time.Date(2026, 9, 24, 10, 30, 0, 0, time.UTC)
	sched := complexSchedule(t)
	const shots = 8
	total := len(sched.GateIDs) * shots

	// 复杂拓扑中拥有 q1.ctrl 脉冲的门：a1(H@q1)、b0(CNOT q0,q1)、c1(X@q1)。
	wantAffected := map[string]bool{"a1": true, "b0": true, "c1": true}

	commitQ1Change := func(t *testing.T, s *calibration.Store) {
		t.Helper()
		if _, err := s.Commit(calibration.Candidate{
			Parameters: map[string]map[string]float64{
				calibration.GlobalLayer: {"amp": 0.5, "frequency": 5000},
				"q1.ctrl":               {"amp": 0.25},
			},
			ReadoutFid: map[int]float64{0: 0.9, 1: 0.9, 2: 0.9},
			Note:       "q1 re-drive amplitude changed during interruption",
		}, base.Add(time.Minute)); err != nil {
			t.Fatalf("commit: %v", err)
		}
		if err := s.MarkStable(base.Add(time.Minute)); err != nil {
			t.Fatalf("mark stable: %v", err)
		}
	}

	t.Run("strict rejects scoped change", func(t *testing.T) {
		s := newStoreWideTolerance(t)
		e := NewExecutor(s, DefaultNoise())
		cp, j2 := interruptAt(t, e, sched, shots, 31, base, total/2, false, "job-q1-strict")
		commitQ1Change(t, s)
		res, _, err := e.Resume(context.Background(), j2, cp, ResumeStrict, false)
		var rerr *ResumeError
		if !errors.As(err, &rerr) {
			t.Fatalf("expected *ResumeError, got res=%v err=%v", res, err)
		}
		if rerr.Reason != ResumeCalibrationChanged {
			t.Fatalf("reason=%s want %s", rerr.Reason, ResumeCalibrationChanged)
		}
		if res != nil {
			t.Fatalf("strict resume on changed calibration must not produce a result")
		}
		t.Logf("verdict: %v", rerr)
	})

	for _, concurrent := range []bool{false, true} {
		mode := "sequential"
		if concurrent {
			mode = "concurrent"
		}
		t.Run("recompute-scoped/"+mode, func(t *testing.T) {
			s := newStoreWideTolerance(t)
			e := NewExecutor(s, DefaultNoise())
			cp, j2 := interruptAt(t, e, sched, shots, 31, base, total/2, concurrent, "job-q1-"+mode)
			commitQ1Change(t, s)

			res, cp2, err := e.Resume(context.Background(), j2, cp, ResumeRecompute, concurrent)
			if err != nil {
				t.Fatalf("recompute resume: %v", err)
			}
			logResume(t, "q1-scoped/"+mode, res, cp2)

			// 决策日志必须列出受影响门，且标注是按范围重算而非全量。
			var decision string
			for _, ev := range res.Events {
				if ev.Kind == "resume-decision" {
					decision = ev.Note
				}
			}
			if decision == "" {
				t.Fatal("missing resume-decision event")
			}
			t.Logf("decision: %s", decision)
			if !strings.Contains(decision, "decision=recompute-scoped") {
				t.Fatalf("decision must be scoped, got: %s", decision)
			}
			for g := range wantAffected {
				if !strings.Contains(decision, g) {
					t.Fatalf("decision note must list affected gate %s: %s", g, decision)
				}
			}

			// 受影响门：一个复用事件都不许有（旧校准下的结果不得沿用/混杂）。
			reuseByGate := gateReuseEvents(res)
			for g := range wantAffected {
				if reuseByGate[g] != 0 {
					t.Fatalf("affected gate %s must not reuse any old shot, got %d reuse events", g, reuseByGate[g])
				}
			}
			// 复用量恰好等于“非受影响门的终态记录数”。
			wantReused := 0
			for g, recs := range cp.Records {
				if !wantAffected[g] {
					for _, r := range recs {
						if r.Final {
							wantReused++
						}
					}
				}
			}
			if res.Reused != wantReused {
				t.Fatalf("reused=%d want %d (only finals of unaffected gates)", res.Reused, wantReused)
			}
			if res.Recomputed != total-wantReused {
				t.Fatalf("recomputed=%d want %d", res.Recomputed, total-wantReused)
			}
			if res.Reused+res.Recomputed != total {
				t.Fatalf("accounting broken: %d+%d != %d", res.Reused, res.Recomputed, total)
			}
			if res.Reused == 0 {
				t.Fatal("partial calibration change must still reuse unaffected completed work")
			}

			// 参考：当前（新）校准下一次性不间断跑完。续跑必须逐项一致：
			// 受影响门全部按新校准重算；其他门虽复用，但其有效参数两版相同，
			// 复用结果天然等于新校准结果。
			var ref *JobResult
			if concurrent {
				ref = e.RunConcurrent(context.Background(), j2)
			} else {
				ref = e.RunSequential(context.Background(), j2)
			}
			assertEquivalentToReference(t, "q1-scoped/"+mode, res, ref)
			_, refCP := e.RunResumable(context.Background(), j2, concurrent)
			assertCheckpointsPerShotEqual(t, "q1-scoped/"+mode, cp2, refCP)

			for g, recs := range cp2.Records {
				for i, r := range recs {
					if !r.Final {
						t.Fatalf("gate %s shot %d still pending after resume", g, i)
					}
				}
			}
		})
	}
}

// TestResumeScopedRecomputeReadoutFidelity 只改比特 1 的读出保真度：
// 仅测量门 m1 重算（旧高保真测量比特不得混入新高翻转率结果）；m0 复用。
func TestResumeScopedRecomputeReadoutFidelity(t *testing.T) {
	base := time.Date(2026, 9, 24, 10, 30, 0, 0, time.UTC)
	sched := complexSchedule(t)
	const shots = 10
	total := len(sched.GateIDs) * shots

	for _, concurrent := range []bool{false, true} {
		mode := "sequential"
		if concurrent {
			mode = "concurrent"
		}
		t.Run(mode, func(t *testing.T) {
			s := newStore(t, time.Hour, 3)
			e := NewExecutor(s, DefaultNoise())
			cp, j2 := interruptAt(t, e, sched, shots, 42, base, total/2, concurrent, "job-fid-"+mode)

			if _, err := s.Commit(calibration.Candidate{
				Parameters: map[string]map[string]float64{calibration.GlobalLayer: {"amp": 0.5, "frequency": 5000}},
				// 只动比特 1 的保真度：0.9 -> 0.1（读出几乎必然翻转）。
				ReadoutFid: map[int]float64{0: 0.9, 1: 0.1, 2: 0.9},
				Note:       "qubit 1 readout recalibrated",
			}, base.Add(time.Minute)); err != nil {
				t.Fatalf("commit: %v", err)
			}
			if err := s.MarkStable(base.Add(time.Minute)); err != nil {
				t.Fatalf("mark stable: %v", err)
			}

			res, cp2, err := e.Resume(context.Background(), j2, cp, ResumeRecompute, concurrent)
			if err != nil {
				t.Fatalf("resume: %v", err)
			}
			logResume(t, "fid-scoped/"+mode, res, cp2)

			if reuse := gateReuseEvents(res)["m1"]; reuse != 0 {
				t.Fatalf("m1 (qubit 1 readout changed) must not reuse old measured shots, got %d", reuse)
			}
			wantM0Reuse := 0
			for _, r := range cp.Records["m0"] {
				if r.Final {
					wantM0Reuse++
				}
			}
			if wantM0Reuse > 0 && gateReuseEvents(res)["m0"] != wantM0Reuse {
				t.Fatalf("m0 (unchanged qubit 0 readout) should reuse %d finals", wantM0Reuse)
			}
			wantReused := 0
			for g, recs := range cp.Records {
				if g != "m1" {
					for _, r := range recs {
						if r.Final {
							wantReused++
						}
					}
				}
			}
			if res.Reused != wantReused {
				t.Fatalf("reused=%d want %d (all gates except m1)", res.Reused, wantReused)
			}

			var ref *JobResult
			if concurrent {
				ref = e.RunConcurrent(context.Background(), j2)
			} else {
				ref = e.RunSequential(context.Background(), j2)
			}
			assertEquivalentToReference(t, "fid-scoped/"+mode, res, ref)
			_, refCP := e.RunResumable(context.Background(), j2, concurrent)
			assertCheckpointsPerShotEqual(t, "fid-scoped/"+mode, cp2, refCP)

			// 明确验证“不混杂”：m1 续跑比特必须全部等于新校准参考（0.1 保真度，
			// 几乎全 0），且至少有 shot 与检查点中的旧比特（0.9 保真度）不同。
			differFromOld, matchNew := 0, 0
			for i := range cp2.Records["m1"] {
				if cp2.Records["m1"][i].Bit != cp.Records["m1"][i].Bit {
					differFromOld++
				}
				if cp2.Records["m1"][i].Bit == refCP.Records["m1"][i].Bit {
					matchNew++
				}
			}
			t.Logf("m1 mixing check: shots differing from old checkpoint=%d, matching new reference=%d/%d",
				differFromOld, matchNew, shots)
			if matchNew != shots {
				t.Fatalf("m1 resumed bits must all match new calibration, got %d/%d", matchNew, shots)
			}
			if differFromOld == 0 {
				t.Fatal("m1 readout changed drastically; expected bits to differ from old checkpoint")
			}
		})
	}
}

// TestResumeUnrelatedOrEquivalentCalibrationFullyReuses 校准“版本”变了，
// 但本作业范围内的有效参数完全一致：strict 也必须接受并全部复用。
func TestResumeUnrelatedOrEquivalentCalibrationFullyReuses(t *testing.T) {
	base := time.Date(2026, 9, 24, 10, 30, 0, 0, time.UTC)
	sched := complexSchedule(t)
	const shots = 8
	total := len(sched.GateIDs) * shots

	cases := []struct {
		name   string
		commit func(t *testing.T, s *calibration.Store)
	}{
		{
			name: "unchanged-unmeasured-qubit-fidelity",
			// 比特 2 在本作业中从不测量（m0@q0、m1@q1）：改它的保真度
			// 与本作业无关，应视为兼容、全部复用。
			commit: func(t *testing.T, s *calibration.Store) {
				v, err := s.Commit(calibration.Candidate{
					Parameters: map[string]map[string]float64{calibration.GlobalLayer: {"amp": 0.5, "frequency": 5000}},
					ReadoutFid: map[int]float64{0: 0.9, 1: 0.9, 2: 0.2},
				}, base.Add(time.Minute))
				if err != nil {
					t.Fatalf("commit: %v", err)
				}
				if err := s.MarkStable(base.Add(time.Minute)); err != nil {
					t.Fatalf("mark stable: %v", err)
				}
				t.Logf("committed unrelated change as %s", v.ID)
			},
		},
		{
			name: "equivalent-effective-params-new-version",
			// 版本 ID 不同，但有效参数逐键相同：仍兼容。
			commit: func(t *testing.T, s *calibration.Store) {
				v, err := s.Commit(calibration.Candidate{
					Parameters: map[string]map[string]float64{calibration.GlobalLayer: {"amp": 0.5, "frequency": 5000}},
					ReadoutFid: map[int]float64{0: 0.9, 1: 0.9, 2: 0.9},
					Note:       "reissued with identical effective values",
				}, base.Add(time.Minute))
				if err != nil {
					t.Fatalf("commit: %v", err)
				}
				if err := s.MarkStable(base.Add(time.Minute)); err != nil {
					t.Fatalf("mark stable: %v", err)
				}
				t.Logf("committed equivalent version %s", v.ID)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t, time.Hour, 3)
			e := NewExecutor(s, DefaultNoise())
			cp, j2 := interruptAt(t, e, sched, shots, 77, base, total/2, false, "job-equiv-"+tc.name)
			tc.commit(t, s)

			res, _, err := e.Resume(context.Background(), j2, cp, ResumeStrict, false)
			if err != nil {
				t.Fatalf("effective params identical within job scope must stay compatible: %v", err)
			}
			logResume(t, tc.name, res, nil)
			wantReused := countFinalIn(cp, gateIDSet(sched))
			if res.Reused != wantReused {
				t.Fatalf("reused=%d want %d (every final outcome)", res.Reused, wantReused)
			}
			ref := e.RunSequential(context.Background(), j2)
			assertEquivalentToReference(t, tc.name, res, ref)
		})
	}
}

// TestResumeExpiredRejectsBothPoliciesConcurrent 并发模式下，过期校准同样被
// 两种策略明确拒绝，且不返回任何拼装结果。
func TestResumeExpiredRejectsBothPoliciesConcurrent(t *testing.T) {
	base := time.Date(2026, 9, 24, 10, 30, 0, 0, time.UTC)
	sched := complexSchedule(t)
	const shots = 6
	total := len(sched.GateIDs) * shots
	for _, policy := range []ResumePolicy{ResumeStrict, ResumeRecompute} {
		name := policy.String()
		t.Run(name, func(t *testing.T) {
			s := newStore(t, time.Hour, 3)
			e := NewExecutor(s, DefaultNoise())
			cp, j2 := interruptAt(t, e, sched, shots, 5, base, total/2, true, "job-exp-"+name)
			j2.Now = base.Add(2 * time.Hour)
			res, _, err := e.Resume(context.Background(), j2, cp, policy, true)
			var rerr *ResumeError
			if !errors.As(err, &rerr) {
				t.Fatalf("expected *ResumeError, got res=%v err=%v", res, err)
			}
			if rerr.Reason != ResumeCalibrationUnavailable {
				t.Fatalf("reason=%s want %s", rerr.Reason, ResumeCalibrationUnavailable)
			}
			if res != nil {
				t.Fatalf("must not return an all-failed normal result")
			}
			t.Logf("verdict: %v", rerr)
		})
	}
}
