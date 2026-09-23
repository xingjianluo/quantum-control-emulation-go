package calibration

import (
	"math"
	"sync"
	"time"
)

// Store 是校准快照的分层版本仓库。
//
// 版本规则：
//   - BeginDraft 复制某基线版本（默认当前稳定版）得到 version=0 的草稿；
//   - Commit 做容差校验，通过则分配新版本号并追加为唯一当前稳定版；
//   - MarkFailed 追加失败记录，失败快照永不可用；
//   - Rollback 将"当前稳定版"指针退回上一条稳定版本（仅线性后退一步）；
//   - 所有已提交快照不可变，回滚后仍可按版本号取回，保证历史可复现。
type Store struct {
	mu      sync.Mutex
	snaps   map[int]*Snapshot // 所有已提交（稳定/失败）快照
	order   []int             // 提交顺序的版本号（含失败快照）
	current int               // 当前稳定版本号；0 表示无
	maxVer  int
	audit   []Event
}

// NewStore 创建空仓库。
func NewStore() *Store {
	return &Store{snaps: map[int]*Snapshot{}}
}

// Clone 深拷贝快照（门参数与读出保真度表都会独立复制）。
func (s *Snapshot) Clone() *Snapshot {
	cp := &Snapshot{
		Version:         s.Version,
		Status:          s.Status,
		CreatedAt:       s.CreatedAt,
		Note:            s.Note,
		GateParams:      make(map[GateKey]map[string]float64, len(s.GateParams)),
		ReadoutFidelity: make(map[int]float64, len(s.ReadoutFidelity)),
	}
	for k, v := range s.GateParams {
		m := make(map[string]float64, len(v))
		for pk, pv := range v {
			m[pk] = pv
		}
		cp.GateParams[k] = m
	}
	for k, v := range s.ReadoutFidelity {
		cp.ReadoutFidelity[k] = v
	}
	return cp
}

// BeginDraft 创建草稿。base=0 表示以当前稳定版本为基线（无稳定版则空快照）；
// 显式给定版本号则以该已存在快照为基线。
func (s *Store) BeginDraft(base int, note string, at time.Time) *Snapshot {
	s.mu.Lock()
	src := s.lockedResolve(base)
	s.mu.Unlock()

	draft := &Snapshot{
		Status:          StatusDraft,
		CreatedAt:       at,
		Note:            note,
		GateParams:      map[GateKey]map[string]float64{},
		ReadoutFidelity: map[int]float64{},
	}
	if src != nil {
		draft = src.Clone()
		draft.Status = StatusDraft
		draft.CreatedAt = at
		draft.Note = note
		draft.Version = 0
	}
	s.mu.Lock()
	s.audit = append(s.audit, Event{Version: baseVersion(src), Kind: "begin", At: at, Detail: note})
	s.mu.Unlock()
	return draft
}

func baseVersion(s *Snapshot) int {
	if s == nil {
		return 0
	}
	return s.Version
}

func (s *Store) lockedResolve(base int) *Snapshot {
	if base == 0 {
		if s.current == 0 {
			return nil
		}
		return s.snaps[s.current]
	}
	return s.snaps[base]
}

// Commit 校验草稿与容差，通过后提升为新稳定版本。
func (s *Store) Commit(draft *Snapshot, tol Tolerance, at time.Time) (int, error) {
	if draft == nil || draft.Status != StatusDraft {
		return 0, &Error{Reason: ReasonNotDraft, Detail: "只有草稿态快照可以提交"}
	}
	if err := validateReadout(draft, tol); err != nil {
		return 0, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	prev := s.lockedResolve(0)
	if err := validateAgainst(prev, draft, tol); err != nil {
		return 0, err
	}

	s.maxVer++
	v := s.maxVer
	d := draft.Clone()
	d.Version = v
	d.Status = StatusStable
	d.CreatedAt = at
	s.snaps[v] = d
	s.order = append(s.order, v)
	s.current = v
	s.audit = append(s.audit, Event{Version: v, Kind: "commit", At: at, Detail: d.Note})
	return v, nil
}

// MarkFailed 将草稿标记为校准失败并留档；失败快照永远不可作为编译/执行依据。
func (s *Store) MarkFailed(draft *Snapshot, reason string, at time.Time) error {
	if draft == nil || draft.Status != StatusDraft {
		return &Error{Reason: ReasonNotDraft, Detail: "只有草稿态快照可以标记失败"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maxVer++
	v := s.maxVer
	d := draft.Clone()
	d.Version = v
	d.Status = StatusFailed
	d.CreatedAt = at
	if d.Note == "" {
		d.Note = reason
	}
	s.snaps[v] = d
	s.order = append(s.order, v)
	s.audit = append(s.audit, Event{Version: v, Kind: "fail", At: at, Detail: reason})
	draft.Status = StatusFailed
	draft.Version = v
	return nil
}

// Current 返回当前稳定快照的不可变副本。
func (s *Store) Current() *Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == 0 {
		return nil
	}
	return s.snaps[s.current].Clone()
}

// CurrentVersion 返回当前稳定版本号（0 表示无）。
func (s *Store) CurrentVersion() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current
}

// Get 返回指定版本快照的不可变副本。
func (s *Store) Get(version int) (*Snapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, ok := s.snaps[version]
	if !ok {
		return nil, false
	}
	return snap.Clone(), true
}

// Rollback 把当前稳定指针退回到上一个稳定版本。
// 失败快照不计入"稳定版本链"；若历史不足两个稳定版本则拒绝。
func (s *Store) Rollback(at time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var stable []int
	for _, v := range s.order {
		if s.snaps[v].Status == StatusStable {
			stable = append(stable, v)
		}
	}
	if len(stable) < 2 {
		return 0, &Error{Reason: ReasonNoPrevious, Detail: "不存在上一个稳定版本，无法回滚"}
	}
	// 语义：沿稳定版本链把"当前指针"前移一步（失败快照不在链上）。
	pos := -1
	for i, v := range stable {
		if v == s.current {
			pos = i
		}
	}
	if pos <= 0 {
		return 0, &Error{Reason: ReasonNoPrevious, Detail: "当前稳定版本没有更早的稳定版本可回滚"}
	}
	target := stable[pos-1]
	s.current = target
	s.audit = append(s.audit, Event{Version: target, Kind: "rollback", At: at, Detail: "rollback to previous stable"})
	return target, nil
}

// AuditLog 返回审计日志副本。
func (s *Store) AuditLog() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, len(s.audit))
	copy(out, s.audit)
	return out
}

// StableHistory 返回按提交顺序排列的稳定版本号。
func (s *Store) StableHistory() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []int
	for _, v := range s.order {
		if s.snaps[v].Status == StatusStable {
			out = append(out, v)
		}
	}
	return out
}

func validateReadout(d *Snapshot, tol Tolerance) error {
	for q, f := range d.ReadoutFidelity {
		if math.IsNaN(f) || f < 0 || f > 1 {
			return &Error{Reason: ReasonReadoutOutOfRange, Version: d.Version,
				Detail: "qubit 读出保真度必须在 [0,1]"}
		}
		if (tol.ReadoutMin != 0 || tol.ReadoutMax != 0) && (f < tol.ReadoutMin || f > tol.ReadoutMax) {
			return &Error{Reason: ReasonReadoutOutOfRange, Version: d.Version,
				Detail: "qubit 读出保真度超出容差区间"}
		}
		_ = q
	}
	return nil
}

// validateAgainst 校验新草稿相对上一稳定版本的参数漂移；未列入 ParamDelta 的参数默认零容差。
func validateAgainst(prev, draft *Snapshot, tol Tolerance) error {
	if prev == nil {
		return nil // 首个稳定版本无基线可比
	}
	for key, newParams := range draft.GateParams {
		oldParams, ok := prev.GateParams[key]
		if !ok {
			continue // 新增的门键不做漂移比较
		}
		for name, nv := range newParams {
			ov, exists := oldParams[name]
			if !exists {
				continue
			}
			limit, known := tol.ParamDelta[name]
			if !known {
				limit = 0
			}
			if math.Abs(nv-ov) > limit+1e-12 {
				return &Error{Reason: ReasonParamOutOfTolerance, Version: draft.Version,
					Detail: "门参数漂移超过容差"}
			}
		}
	}
	return nil
}
