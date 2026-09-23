package calibration

import "fmt"

// FailReason 描述校准被拒绝或不可用的原因类别。
type FailReason int

const (
	FailUnknown FailReason = iota
	// FailExpired 当前版本超过 TTL，参数已过期，禁止静默沿用。
	FailExpired
	// FailOutOfTolerance 候选参数与基线漂移超过容差。
	FailOutOfTolerance
	// FailBadValue 候选包含非法值（NaN/越界/保真度不在[0,1]）。
	FailBadValue
	// FailMissingKey 候选缺少必需的参数键。
	FailMissingKey
	// FailNoStable 不存在可回滚/可参照的稳定版本。
	FailNoStable
)

// String 返回原因可读名称。
func (r FailReason) String() string {
	switch r {
	case FailExpired:
		return "calibration-expired"
	case FailOutOfTolerance:
		return "out-of-tolerance"
	case FailBadValue:
		return "bad-value"
	case FailMissingKey:
		return "missing-key"
	case FailNoStable:
		return "no-stable-version"
	default:
		return "unknown"
	}
}

// Error 是校准阶段的可分类错误。
type Error struct {
	Reason  FailReason
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("calibration error (%s): %s", e.Reason, e.Message)
}

func calErr(r FailReason, format string, args ...any) *Error {
	return &Error{Reason: r, Message: fmt.Sprintf(format, args...)}
}
