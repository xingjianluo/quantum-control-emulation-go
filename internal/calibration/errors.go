package calibration

import "fmt"

// FailReason 区分校准流水线中被拒绝的具体原因。
type FailReason string

const (
	// ReasonNoStable 没有任何稳定版本可用（无法回滚/编译/执行）。
	ReasonNoStable FailReason = "no_stable_version"
	// ReasonExpired 计划绑定的校准版本已超过有效期，禁止静默沿用。
	ReasonExpired FailReason = "calibration_expired"
	// ReasonVersionMismatch 计划绑定版本不是当前稳定版本。
	ReasonVersionMismatch FailReason = "version_mismatch"
	// ReasonBadSnapshot 引用了不存在、草稿态或失败态快照。
	ReasonBadSnapshot FailReason = "bad_snapshot"
	// ReasonParamOutOfTolerance 新校准参数相对上一稳定版本漂移超限。
	ReasonParamOutOfTolerance FailReason = "param_out_of_tolerance"
	// ReasonReadoutOutOfRange 读出保真度不在 [0,1] 或不在容差区间。
	ReasonReadoutOutOfRange FailReason = "readout_out_of_range"
	// ReasonNotDraft 试图提交/标失败一个不是草稿态的快照。
	ReasonNotDraft FailReason = "not_a_draft"
	// ReasonNoPrevious 没有"上一稳定版本"可回滚。
	ReasonNoPrevious FailReason = "no_previous_version"
)

// Error 携带可程序化判别的失败原因与人类可读说明。
type Error struct {
	Reason  FailReason
	Version int
	Detail  string
}

func (e *Error) Error() string {
	if e.Version != 0 {
		return fmt.Sprintf("calibration: %s (version=%d): %s", e.Reason, e.Version, e.Detail)
	}
	return fmt.Sprintf("calibration: %s: %s", e.Reason, e.Detail)
}

// IsError 判断 err 是否为指定原因的校准错误。
func IsError(err error, reason FailReason) bool {
	if ce, ok := err.(*Error); ok {
		return ce.Reason == reason
	}
	return false
}
