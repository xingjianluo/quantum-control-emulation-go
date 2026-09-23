package sim

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/xingjianluo/quantum-control-emulation-go/internal/calibration"
	"github.com/xingjianluo/quantum-control-emulation-go/internal/compile"
	"github.com/xingjianluo/quantum-control-emulation-go/internal/ir"
)

func newStore(t *testing.T, ttl time.Duration, qubits int) *calibration.Store {
	t.Helper()
	s := calibration.NewStore(ttl, qubits,
		map[string]float64{"amp": 0.2, "frequency": 100},
		[]string{"amp"})
	fids := map[int]float64{}
	params := map[string]map[string]float64{calibration.GlobalLayer: {"amp": 0.5, "frequency": 5000}}
	for q := 0; q < qubits; q++ {
		fids[q] = 0.9
	}
	err := s.Seed(calibration.Version{
		ID: "v-stable", CreatedAt: time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC),
		Parameters: params, ReadoutFid: fids, Note: "test",
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	return s
}

func demoSchedule(t *testing.T) *ir.Schedule {
	t.Helper()
	c, err := compile.NewCompiler(compile.DefaultProtocol(3))
	if err != nil {
		t.Fatal(err)
	}
	gates := []ir.GateTask{
		{ID: "g1", Kind: ir.GateX, Qubits: []int{0}},
		{ID: "g2", Kind: ir.GateX, Qubits: []int{1}},
		{ID: "g3", Kind: ir.GateCNOT, Qubits: []int{0, 1}, DependsOn: []string{"g1", "g2"}},
		{ID: "m0", Kind: ir.GateMeasure, Qubits: []int{0}, DependsOn: []string{"g3"}},
	}
	s, err := c.Compile(gates)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return s
}

func logJob(t *testing.T, name string, j Job) {
	t.Helper()
	t.Logf("[%s] input: shots=%d seed=%d now=%s timeout=%s hooks=%v gates=%v makespan=%dns",
		name, j.Shots, j.Seed, j.Now.Format("15:04:05"), j.PerTaskTimeout, hookNames(j.Hooks),
		j.Schedule.GateIDs, j.Schedule.MakespanNS())
}

func hookNames(h map[string]Hook) []string {
	out := make([]string, 0, len(h))
	for k := range h {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

func sortStrings(xs []string) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j-1] > xs[j]; j-- {
			xs[j-1], xs[j] = xs[j], xs[j-1]
		}
	}
}

func logResult(t *testing.T, name string, r *JobResult) {
	t.Helper()
	t.Logf("[%s] aggregate verdict:", name)
	for _, rep := range r.sortedReports() {
		t.Logf("  gate=%-3s status=%-9s bit=%d attempts=%d detail=%s perShot=%v",
			rep.GateID, rep.Status, rep.Bit, rep.Attempts, rep.Detail, rep.PerShot)
	}
	t.Logf("[%s] causal event log (%d events, first 12):", name, len(r.Events))
	n := len(r.Events)
	if n > 12 {
		n = 12
	}
	for _, e := range r.Events[:n] {
		t.Logf("  seq=%-3d gate=%-3s kind=%-16s %s", e.Seq, e.GateID, e.Kind, e.Note)
	}
}

func TestSequentialConcurrentEquivalent(t *testing.T) {
	now := time.Date(2026, 9, 24, 10, 30, 0, 0, time.UTC)
	s := newStore(t, time.Hour, 3)
	e := NewExecutor(s, DefaultNoise())
	j := Job{Schedule: demoSchedule(t), Shots: 50, Seed: 42, Now: now}

	logJob(t, "seq-vs-con", j)
	seq := e.RunSequential(context.Background(), j)
	con := e.RunConcurrent(context.Background(), j)
	logResult(t, "sequential", seq)
	logResult(t, "concurrent", con)

	if !seq.Equivalent(con) {
		t.Fatalf("sequential vs concurrent results differ:\nseq=%+v\ncon=%+v", seq.Reports, con.Reports)
	}
	// 不重不漏：每门 attempts 恰好等于 shots。
	for _, rep := range seq.Reports {
		if rep.Attempts != 50 {
			t.Fatalf("gate %s attempts=%d want 50 (no double count / omission)", rep.GateID, rep.Attempts)
		}
	}
	// 事件总数也应相等（每个 shot 每门一个终结事件）。
	if len(seq.Events) != len(con.Events) {
		t.Fatalf("event count differs: %d vs %d", len(seq.Events), len(con.Events))
	}
	// 因果序保持：对每个 shot，依赖门的完成事件早于后继门开始。
	if !causalOrderPreserved(t, con, j.Shots) {
		t.Fatal("concurrent execution violated causal ordering")
	}
	t.Log("verdict basis: deterministic per-(gate,shot) RNG + same execution core => equivalent reports")
}

// causalOrderPreserved 校验事件中：每个 shot 上 g1/g2 的终结事件先于 g3 的任何事件。
func causalOrderPreserved(t *testing.T, r *JobResult, shots int) bool {
	t.Helper()
	for shot := 0; shot < shots; shot++ {
		var lastPred, firstSucc int
		lastPred, firstSucc = -1, -1
		for _, ev := range r.Events {
			if shotFromNote(ev.Note) != shot {
				continue
			}
			if (ev.GateID == "g1" || ev.GateID == "g2") && terminalKind(ev.Kind) {
				if ev.Seq > lastPred {
					lastPred = ev.Seq
				}
			}
			if ev.GateID == "g3" && firstSucc < 0 {
				firstSucc = ev.Seq
			}
		}
		if lastPred < 0 || firstSucc < 0 || firstSucc < lastPred {
			t.Logf("shot %d causal violation lastPred=%d firstSucc=%d", shot, lastPred, firstSucc)
			return false
		}
	}
	return true
}

func shotFromNote(note string) int {
	var shot int
	if _, err := fmt.Sscanf(note, "shot=%d", &shot); err == nil {
		return shot
	}
	return -1
}

func terminalKind(kind string) bool {
	return kind == "complete" || kind == "pulse-fail" || kind == "hook" ||
		kind == "calibration-halt" || kind == "illegal-drive" || kind == "measure"
}

func TestCancellationNoDoubleCount(t *testing.T) {
	now := time.Date(2026, 9, 24, 10, 30, 0, 0, time.UTC)
	s := newStore(t, time.Hour, 3)
	e := NewExecutor(s, DefaultNoise())

	// g1 在第 3 次进入 hook 时取消整个作业；门内逐 shot 串行，前 3 个已完成
	// 的结果保留，后续 shot 标记为 cancelled 且绝不重复执行。
	var mu sync.Mutex
	entries, exits := 0, 0
	ctx, cancel := context.WithCancel(context.Background())
	hooks := map[string]Hook{
		"g1": func(ctx context.Context) error {
			mu.Lock()
			entries++
			n := entries
			mu.Unlock()
			if n == 3 {
				cancel()
			}
			mu.Lock()
			exits++
			mu.Unlock()
			return nil
		},
	}
	j := Job{Schedule: demoSchedule(t), Shots: 10, Seed: 7, Now: now, Hooks: hooks}
	logJob(t, "cancel", j)
	r := e.RunConcurrent(ctx, j)
	logResult(t, "cancel", r)
	var g1 TaskReport
	for _, rep := range r.Reports {
		if rep.GateID == "g1" {
			g1 = rep
		}
	}
	if g1.Attempts != 10 {
		t.Fatalf("g1 must have exactly one outcome per shot, got %d", g1.Attempts)
	}
	cancelled, executed := 0, 0
	for _, st := range g1.PerShot {
		switch st {
		case StatusCancelled:
			cancelled++
		default:
			// 进入执行核（含确定性噪声导致的 failed）都算一次且仅一次执行。
			executed++
		}
	}
	if cancelled == 0 || executed == 0 {
		t.Fatalf("expected some executed and some cancelled shots, got executed=%d cancelled=%d", executed, cancelled)
	}
	mu.Lock()
	gotExits := exits
	mu.Unlock()
	if executed != gotExits {
		t.Fatalf("double execution detected: executed shots=%d hook entries=%d", executed, gotExits)
	}
	// 下游门被取消的 shot 必须标记 cancelled（因果不可达），且不计数成功。
	for _, rep := range r.Reports {
		if rep.GateID == "g3" || rep.GateID == "m0" {
			hasCancelled := false
			for _, st := range rep.PerShot {
				if st == StatusCancelled {
					hasCancelled = true
				}
			}
			if !hasCancelled {
				t.Fatalf("downstream gate %s expected cancelled shots", rep.GateID)
			}
		}
	}
	t.Logf("verdict: g1 executed=%d cancelled=%d, downstream cascaded, attempts total=%d", executed, cancelled, g1.Attempts)
}

func TestTimeoutClassifiedDistinctly(t *testing.T) {
	now := time.Date(2026, 9, 24, 10, 30, 0, 0, time.UTC)
	s := newStore(t, time.Hour, 3)
	e := NewExecutor(s, DefaultNoise())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hooks := map[string]Hook{
		"g2": func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}
	j := Job{
		Schedule: demoSchedule(t), Shots: 5, Seed: 9, Now: now,
		PerTaskTimeout: 30 * time.Millisecond, Hooks: hooks,
	}
	logJob(t, "timeout", j)
	r := e.RunConcurrent(ctx, j)
	logResult(t, "timeout", r)
	var g2 TaskReport
	for _, rep := range r.Reports {
		if rep.GateID == "g2" {
			g2 = rep
		}
	}
	timedOut := 0
	for _, st := range g2.PerShot {
		if st == StatusTimedOut {
			timedOut++
		}
	}
	if timedOut == 0 {
		t.Fatalf("expected timed-out shots, got %v", g2.PerShot)
	}
	// 超时与取消必须可区分：其余未触发超时路径但被级联的是 cancelled。
	for _, rep := range r.Reports {
		for _, st := range rep.PerShot {
			if st == StatusTimedOut && rep.GateID != "g2" {
				// 下游应是 cancelled，而非 timeout。
				t.Fatalf("downstream gate %s should be cancelled, not timed-out", rep.GateID)
			}
		}
	}
}

func TestPartialPulseFailureNoOmission(t *testing.T) {
	now := time.Date(2026, 9, 24, 10, 30, 0, 0, time.UTC)
	s := newStore(t, time.Hour, 3)
	// g2 独占 q1.ctrl 且失败概率 1；其余资源失败率 0，避免结果抖动。
	noise := NoiseModel{
		DepolPerPulse: map[string]float64{"q0.ctrl": 0, "q1.ctrl": 1.0, "q2.ctrl": 0},
		AmpScale:      map[string]float64{"q0.ctrl": 1, "q1.ctrl": 1, "q2.ctrl": 1},
		MaxAmp:        1.0,
	}
	e := NewExecutor(s, noise)
	j := Job{Schedule: demoSchedule(t), Shots: 8, Seed: 11, Now: now}
	logJob(t, "partial-fail", j)
	r := e.RunSequential(context.Background(), j)
	logResult(t, "partial-fail", r)
	counts := r.Counts()
	if counts["g2"][StatusFailed] != 8 {
		t.Fatalf("g2 should fail all shots, got %v", counts["g2"])
	}
	if counts["g1"][StatusOK] != 8 {
		t.Fatalf("g1 on q0 (p=0) should be ok all shots, got %v", counts["g1"])
	}
	// g3 是双脉冲门：其 q1 端脉冲必失败，故 g3 也失败，但每个 shot 都确实被
	// 执行并恰好计入一次——部分失败不得造成遗漏。
	if counts["g3"][StatusFailed] != 8 {
		t.Fatalf("g3 should fail all shots via its q1 pulse, got %v", counts["g3"])
	}
	for _, rep := range r.Reports {
		if rep.Attempts != 8 {
			t.Fatalf("gate %s attempts=%d, partial failure must not omit shots", rep.GateID, rep.Attempts)
		}
	}
}

func TestExpiredCalibrationHaltsNotSilent(t *testing.T) {
	// now 超过 1 小时 TTL：执行必须按校准失败分类，不得沿用旧参数产出比特。
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	s := newStore(t, time.Hour, 3)
	e := NewExecutor(s, DefaultNoise())
	j := Job{Schedule: demoSchedule(t), Shots: 4, Seed: 3, Now: now}
	logJob(t, "expired-cal", j)
	r := e.RunSequential(context.Background(), j)
	logResult(t, "expired-cal", r)
	counts := r.Counts()
	for gate, m := range counts {
		if m[StatusOK] != 0 || m[StatusMeasured] != 0 {
			t.Fatalf("gate %s must not succeed with expired calibration, got %v", gate, m)
		}
		if m[StatusFailed] == 0 {
			t.Fatalf("gate %s must record failed shots due to expiry, got %v", gate, m)
		}
	}
	// 事件日志必须能解释判定依据是校准过期。
	sawExpiry := false
	for _, ev := range r.Events {
		if ev.Kind == "calibration-halt" {
			sawExpiry = true
		}
	}
	if !sawExpiry {
		t.Fatal("expected calibration-halt events explaining the verdict")
	}
}

func TestHookGenericFailureIsFailed(t *testing.T) {
	now := time.Date(2026, 9, 24, 10, 30, 0, 0, time.UTC)
	s := newStore(t, time.Hour, 3)
	e := NewExecutor(s, DefaultNoise())
	hooks := map[string]Hook{"g1": func(ctx context.Context) error { return errors.New("hardware fault") }}
	j := Job{Schedule: demoSchedule(t), Shots: 3, Seed: 5, Now: now, Hooks: hooks}
	logJob(t, "hook-fail", j)
	r := e.RunSequential(context.Background(), j)
	logResult(t, "hook-fail", r)
	if r.Counts()["g1"][StatusFailed] != 3 {
		t.Fatalf("generic hook error must be StatusFailed, got %v", r.Counts()["g1"])
	}
}
