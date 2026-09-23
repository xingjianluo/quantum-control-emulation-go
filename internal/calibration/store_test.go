package calibration

import (
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(time.Hour, 2,
		map[string]float64{"amp": 0.1, "frequency": 50.0},
		[]string{"amp"})
}

func seedStable(t *testing.T, s *Store, at time.Time) Version {
	t.Helper()
	v := Version{
		ID:        "seed-v1",
		CreatedAt: at,
		Parameters: map[string]map[string]float64{
			GlobalLayer: {"amp": 0.5, "frequency": 5000.0},
		},
		ReadoutFid: map[int]float64{0: 0.98, 1: 0.95},
		Note:       "factory",
	}
	if err := s.Seed(v); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return v
}

func TestExpiredRejectsSilentReuse(t *testing.T) {
	t0 := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	s := testStore(t)
	seedStable(t, s, t0)

	v, err := s.Current(t0.Add(30 * time.Minute))
	t.Logf("input: now=t0+30m; decision: current=%s; verdict: %v", v.ID, err)
	if err != nil {
		t.Fatal("within TTL must be usable")
	}

	_, err = s.Current(t0.Add(2 * time.Hour))
	t.Logf("input: now=t0+2h (ttl=1h); verdict basis: %v", err)
	if err == nil || err.(*Error).Reason != FailExpired {
		t.Fatalf("expired version must be rejected with FailExpired, got %v", err)
	}
	// 分层取值同样拒绝，禁止执行层静默沿用。
	if _, _, err := s.CurrentFor("q0.ctrl", 0, t0.Add(2*time.Hour)); err == nil {
		t.Fatal("CurrentFor must reject when expired")
	}
}

func TestLayeredMerge(t *testing.T) {
	t0 := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	s := testStore(t)
	seedStable(t, s, t0)
	// 加一层资源覆盖。
	c := Candidate{
		Parameters: map[string]map[string]float64{
			GlobalLayer: {"amp": 0.5, "frequency": 5000.0},
			"q0.ctrl":   {"amp": 0.55},
		},
		ReadoutFid: map[int]float64{0: 0.98, 1: 0.95},
		Note:       "layered",
	}
	v, err := s.Commit(c, t0.Add(time.Minute))
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := s.MarkStable(t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	merged, fid, err := s.CurrentFor("q0.ctrl", 0, t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("input: resource=q0.ctrl; decision: merged=%v fid=%v version=%s", merged, fid, v.ID)
	if merged["amp"] != 0.55 || merged["frequency"] != 5000.0 {
		t.Fatalf("layer merge wrong: %v", merged)
	}
	merged1, _, _ := s.CurrentFor("q1.ctrl", 1, t0.Add(time.Minute))
	if merged1["amp"] != 0.5 {
		t.Fatalf("q1 should inherit global amp 0.5, got %v", merged1)
	}
}

func TestCommitFailuresAreClassified(t *testing.T) {
	t0 := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	s := testStore(t)
	seedStable(t, s, t0)

	bad := Candidate{
		Parameters: map[string]map[string]float64{GlobalLayer: {"amp": 1.5}},
		ReadoutFid: map[int]float64{0: 0.9, 1: 0.9},
	}
	_, err := s.Commit(bad, t0.Add(time.Minute))
	t.Logf("input candidate amp=1.5; verdict: %v", err)
	if err == nil || err.(*Error).Reason != FailBadValue {
		t.Fatalf("expected FailBadValue, got %v", err)
	}
	if id := s.CurrentID(); id != "seed-v1" {
		t.Fatalf("failed commit must not change head, got %s", id)
	}

	missing := Candidate{
		Parameters: map[string]map[string]float64{GlobalLayer: {"frequency": 5000}},
		ReadoutFid: map[int]float64{0: 0.9, 1: 0.9},
	}
	_, err = s.Commit(missing, t0.Add(time.Minute))
	t.Logf("input candidate without amp; verdict: %v", err)
	if err == nil || err.(*Error).Reason != FailMissingKey {
		t.Fatalf("expected FailMissingKey, got %v", err)
	}

	drift := Candidate{
		Parameters: map[string]map[string]float64{GlobalLayer: {"amp": 0.9, "frequency": 5000}},
		ReadoutFid: map[int]float64{0: 0.9, 1: 0.9},
	}
	_, err = s.Commit(drift, t0.Add(time.Minute))
	t.Logf("input candidate amp=0.9 (drift 0.4 > tol 0.1); verdict: %v", err)
	if err == nil || err.(*Error).Reason != FailOutOfTolerance {
		t.Fatalf("expected FailOutOfTolerance, got %v", err)
	}
}

func TestRollbackReproducesHistory(t *testing.T) {
	t0 := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	s := testStore(t)
	seedStable(t, s, t0)
	// 记录稳定版上的历史结论。
	hist := HistorySummary{Summary: "fidelity-bucket=0.98;expect-bit0=1"}
	if err := s.AttachHistory("seed-v1", hist); err != nil {
		t.Fatal(err)
	}

	// 提交一个新的非稳定版本（TTL 内）。
	c := Candidate{
		Parameters: map[string]map[string]float64{GlobalLayer: {"amp": 0.52, "frequency": 5010.0}},
		ReadoutFid: map[int]float64{0: 0.97, 1: 0.94},
		Note:       "drift-day2",
	}
	v2, err := s.Commit(c, t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("decision: committed unstable %s head=%s", v2.ID, s.CurrentID())
	if s.CurrentID() != v2.ID {
		t.Fatal("head should be new version")
	}

	// 回滚到上一稳定版本。
	rb, err := s.Rollback(t0.Add(2 * time.Minute))
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	t.Logf("decision: rolled back to %s stable=%v", rb.ID, rb.Stable)
	if rb.ID != "seed-v1" {
		t.Fatalf("expected seed-v1, got %s", rb.ID)
	}

	// 仿真结论必须与历史记录一致。
	got, err := s.HistoryAt("seed-v1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Summary != hist.Summary {
		t.Fatalf("history mismatch after rollback: %q != %q", got.Summary, hist.Summary)
	}
	cur, _ := s.Current(t0.Add(2 * time.Minute))
	merged, fid, _ := s.CurrentFor(GlobalLayer, 0, t0.Add(2*time.Minute))
	t.Logf("final verdict basis: version=%s params=%v fid=%v history=%q",
		cur.ID, merged, fid, got.Summary)
	if merged["amp"] != 0.5 || fid != 0.98 {
		t.Fatal("post-rollback effective calibration must equal stable baseline")
	}
}

func TestRollbackNoStable(t *testing.T) {
	s := testStore(t)
	_, err := s.Rollback(time.Now())
	if err == nil || err.(*Error).Reason != FailNoStable {
		t.Fatalf("expected FailNoStable, got %v", err)
	}
}

func TestRecalibrateAfterExpiry(t *testing.T) {
	t0 := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	s := testStore(t)
	seedStable(t, s, t0)

	// 过期后，基于稳定基线重新校准（容差内）应成功并刷新 head。
	c := Candidate{
		Parameters: map[string]map[string]float64{GlobalLayer: {"amp": 0.51, "frequency": 5005.0}},
		ReadoutFid: map[int]float64{0: 0.98, 1: 0.95},
	}
	v, err := s.Commit(c, t0.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("recalibration: %v", err)
	}
	t.Logf("recalibrated after expiry: new version=%s at %v", v.ID, v.CreatedAt)
	cur, err := s.Current(t0.Add(2 * time.Hour))
	if err != nil {
		t.Fatalf("new version must be fresh: %v", err)
	}
	if cur.ID != v.ID {
		t.Fatalf("head should be fresh version, got %s", cur.ID)
	}
}
