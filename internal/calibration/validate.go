package calibration

import "time"

// ValidateAt 校验某版本快照在时刻 at 是否可用于编译/仿真：
// 版本必须存在、必须为 stable，且距提交时间不得超过 ttl（ttl<=0 表示永不过期）。
// 任何一条不满足都返回携带明确 FailReason 的 *Error，绝不静默放行。
func (s *Store) ValidateAt(version int, at time.Time, ttl time.Duration) (*Snapshot, error) {
	s.mu.Lock()
	snap, ok := s.snaps[version]
	cur := s.current
	s.mu.Unlock()

	if !ok {
		return nil, &Error{Reason: ReasonBadSnapshot, Version: version, Detail: "校准版本不存在"}
	}
	if snap.Status == StatusDraft {
		return nil, &Error{Reason: ReasonBadSnapshot, Version: version, Detail: "草稿态校准不可使用"}
	}
	if snap.Status == StatusFailed {
		return nil, &Error{Reason: ReasonBadSnapshot, Version: version, Detail: "校准失败版本不可使用"}
	}
	if ttl > 0 && at.Sub(snap.CreatedAt) > ttl {
		return nil, &Error{Reason: ReasonExpired, Version: version,
			Detail: "校准已超过有效期 " + ttl.String() + "，禁止沿用过期参数"}
	}
	if cur != version {
		return nil, &Error{Reason: ReasonVersionMismatch, Version: version,
			Detail: "计划绑定版本不是当前稳定版本"}
	}
	return snap.Clone(), nil
}

// MustCurrent 在 at 时刻返回当前稳定版本并做存在性/过期校验。
func (s *Store) MustCurrent(at time.Time, ttl time.Duration) (*Snapshot, error) {
	cur := s.CurrentVersion()
	if cur == 0 {
		return nil, &Error{Reason: ReasonNoStable, Detail: "仓库中没有任何稳定校准版本"}
	}
	return s.ValidateAt(cur, at, ttl)
}
