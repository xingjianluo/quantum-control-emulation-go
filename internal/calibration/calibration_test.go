package calibration

import (
	"testing"
	"time"

	"github.com/xingjianluo/quantum-control-emulation-go/internal/ir"
)

func baseSnapshot(v int, status Status, at time.Time) *Snapshot {
	return &Snapshot{
		Version:   v,
		Status:    status,
		CreatedAt: at,
		Note:      "base",
		GateParams: map[GateKey]map[string]float64{
			MakeGateKey(ir.GateX, 0): {"amp": 0.5, "freq": 5.0},
		},
		ReadoutFidelity: map[int]float64{0: 0.95},
	}
}

func TestCommitAndTolerance(t *testing.T) {
	t0 := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	s := NewStore()
	tol := Tolerance{ParamDelta: map[string]float64{"amp": 0.01, "freq": 0.2},
		ReadoutMin: 0.9, ReadoutMax: 1.0}

	d0 := baseSnapshot(0, StatusDraft, t0)
	v1, err := s.Commit(d0, tol, t0)
	if err != nil {
		t.Fatalf("首个稳定版本提交失败: %v", err)
	}
	t.Logf("输入: amp=0.5 freq=5.0 readout=0.95, 容差 amp±0.01 freq±0.2 -> 判定: 接受 v%d", v1)

	// 在容差内 -> 接受
	d1 := s.BeginDraft(0, "within", t0.Add(time.Minute))
	d1.GateParams[MakeGateKey(ir.GateX, 0)]["amp"] = 0.509
	v2, err := s.Commit(d1, tol, t0.Add(time.Minute))
	if err != nil {
		t.Fatalf("容差内漂移被错误拒绝: %v", err)
	}
	t.Logf("输入: amp=0.509 (Δ=0.009) -> 判定: 接受 v%d", v2)

	// 超出 amp 容差 -> 明确拒绝且不得产生新版本
	d2 := s.BeginDraft(0, "over", t0.Add(2*time.Minute))
	d2.GateParams[MakeGateKey(ir.GateX, 0)]["amp"] = 0.6
	_, err = s.Commit(d2, tol, t0.Add(2*time.Minute))
	if !IsError(err, ReasonParamOutOfTolerance) {
		t.Fatalf("期望 param_out_of_tolerance，得到 %v", err)
	}
	t.Logf("输入: amp=0.6 (Δ=0.1>0.01) -> 判定: 拒绝 %v", err)
	if s.CurrentVersion() != v2 {
		t.Fatalf("失败提交不得推进当前版本, got=%d want=%d", s.CurrentVersion(), v2)
	}

	// 读出保真度越界 -> 明确拒绝
	d3 := s.BeginDraft(0, "badreadout", t0.Add(3*time.Minute))
	d3.ReadoutFidelity[0] = 0.5
	_, err = s.Commit(d3, tol, t0.Add(3*time.Minute))
	if !IsError(err, ReasonReadoutOutOfRange) {
		t.Fatalf("期望 readout_out_of_range，得到 %v", err)
	}
	t.Logf("输入: readout=0.5 (<0.9) -> 判定: 拒绝 %v", err)
}

func TestFailedSnapshotNeverUsed(t *testing.T) {
	t0 := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	s := NewStore()
	tol := Tolerance{ReadoutMin: 0.0, ReadoutMax: 1.0}
	v1, _ := s.Commit(baseSnapshot(0, StatusDraft, t0), tol, t0)

	fail := s.BeginDraft(v1, "bad fit", t0.Add(time.Minute))
	if err := s.MarkFailed(fail, "拟合不收敛", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	failedVer := fail.Version
	t.Logf("输入: 草稿 v%d 标记失败(拟合不收敛)", failedVer)

	if _, err := s.ValidateAt(failedVer, t0.Add(2*time.Minute), 0); !IsError(err, ReasonBadSnapshot) {
		t.Fatalf("失败快照必须不可用，得到 %v", err)
	}
	t.Logf("判定: 使用失败版本 -> 拒绝 bad_snapshot；当前稳定版仍为 v%d", s.CurrentVersion())
}

func TestExpirationRejectsSilentReuse(t *testing.T) {
	t0 := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	s := NewStore()
	tol := Tolerance{ReadoutMin: 0.9, ReadoutMax: 1.0}
	v, _ := s.Commit(baseSnapshot(0, StatusDraft, t0), tol, t0)

	// TTL 内可用
	if _, err := s.ValidateAt(v, t0.Add(59*time.Second), time.Minute); err != nil {
		t.Fatalf("TTL 内不应过期: %v", err)
	}
	// 超过 TTL 必须明确拒绝，禁止静默沿用
	_, err := s.ValidateAt(v, t0.Add(61*time.Second), time.Minute)
	if !IsError(err, ReasonExpired) {
		t.Fatalf("期望 calibration_expired，得到 %v", err)
	}
	t.Logf("输入: v%d 创建于 %s, TTL=1m, 使用时刻 %s -> 判定: 拒绝过期", v, t0, t0.Add(61*time.Second))
}

func TestRollbackReproducesHistory(t *testing.T) {
	t0 := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	s := NewStore()
	tol := Tolerance{ParamDelta: map[string]float64{"amp": 1, "freq": 1},
		ReadoutMin: 0.0, ReadoutMax: 1.0}

	d1 := baseSnapshot(0, StatusDraft, t0)
	v1, _ := s.Commit(d1, tol, t0)

	d2 := s.BeginDraft(v1, "v2", t0.Add(time.Minute))
	d2.GateParams[MakeGateKey(ir.GateX, 0)]["amp"] = 0.6
	v2, _ := s.Commit(d2, tol, t0.Add(time.Minute))

	// 用 v2 的参数做一次"历史结论"
	hist, _ := s.Get(v2)
	histAmp := hist.GateParams[MakeGateKey(ir.GateX, 0)]["amp"]

	// v3 漂移后回滚到 v2（上一稳定版）
	d3 := s.BeginDraft(v2, "v3", t0.Add(2*time.Minute))
	d3.GateParams[MakeGateKey(ir.GateX, 0)]["amp"] = 0.7
	v3, _ := s.Commit(d3, tol, t0.Add(2*time.Minute))
	t.Logf("提交链: v%d(amp=0.5) -> v%d(amp=%.1f) -> v%d(amp=0.7)", v1, v2, histAmp, v3)

	rb, err := s.Rollback(t0.Add(3 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if rb != v2 {
		t.Fatalf("回滚目标应为 v%d，得到 v%d", v2, rb)
	}
	cur := s.Current()
	if got := cur.GateParams[MakeGateKey(ir.GateX, 0)]["amp"]; got != histAmp {
		t.Fatalf("回滚后参数必须与历史记录一致: got=%.2f want=%.2f", got, histAmp)
	}
	t.Logf("判定: 回滚到 v%d，amp=%.1f，与历史记录一致；审计链: %+v", rb, histAmp, s.AuditLog())

	// 历史版本仍可取回，不可变
	if h2, ok := s.Get(v3); !ok || h2.Status != StatusStable {
		t.Fatalf("回滚不得删除历史版本 v%d", v3)
	}
}

func TestRollbackWithoutHistory(t *testing.T) {
	s := NewStore()
	if _, err := s.Rollback(time.Now()); !IsError(err, ReasonNoPrevious) {
		t.Fatalf("无历史时回滚应被拒绝，得到 %v", err)
	}
}
