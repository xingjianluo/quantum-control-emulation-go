package simulator

import "fmt"

// FailReason 区分仿真执行被拒绝或单操作失败的原因。
type FailReason string

const (
	ReasonBadProgram    FailReason = "bad_program"         // 计划未通过调度校验
	ReasonCalibInvalid  FailReason = "calibration_invalid" // 校准版本不存在/失败态/草稿态
	ReasonCalibExpired  FailReason = "calibration_expired" // 校准超过 TTL
	ReasonCalibMismatch FailReason = "version_mismatch"    // 绑定版本 != 当前稳定版本
	ReasonTimeout       FailReason = "timeout"             // 单操作执行超过超时
	ReasonCanceled      FailReason = "canceled"            // 上下文取消，操作在途中被撤
	ReasonSkippedDep    FailReason = "skipped_dependency"  // 依赖失败，因果链上跳过
)

// Error 是引擎级（整批拒绝）错误。
type Error struct {
	Reason FailReason
	Detail string
}

func (e *Error) Error() string {
	return fmt.Sprintf("simulator: %s: %s", e.Reason, e.Detail)
}

// IsError 判断 err 是否为指定原因的仿真错误。
func IsError(err error, r FailReason) bool {
	if se, ok := err.(*Error); ok {
		return se.Reason == r
	}
	return false
}
