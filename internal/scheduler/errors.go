package scheduler

import "fmt"

// FailReason 区分调度被拒绝的原因。
type FailReason string

const (
	ReasonMissingDep   FailReason = "missing_dependency"     // 依赖的操作不存在
	ReasonCyclicDeps   FailReason = "cyclic_dependencies"    // 依赖图成环
	ReasonSelfDep      FailReason = "self_dependency"        // 操作依赖自身
	ReasonResourceBusy FailReason = "resource_overlap"       // 同一资源时间窗口重叠
	ReasonDepOrder     FailReason = "dependency_after_start" // 依赖开始晚于本操作完成
	ReasonDuplicateOp  FailReason = "duplicate_op_id"        // 操作 ID 重复
	ReasonInvalidTime  FailReason = "invalid_time"           // 负时长/负开始时间
)

// Error 携带可程序化判别的调度失败原因及涉及的对象。
type Error struct {
	Reason   FailReason
	OpID     string
	Resource string
	Detail   string
}

func (e *Error) Error() string {
	switch {
	case e.OpID != "" && e.Resource != "":
		return fmt.Sprintf("scheduler: %s (op=%s resource=%s): %s", e.Reason, e.OpID, e.Resource, e.Detail)
	case e.OpID != "":
		return fmt.Sprintf("scheduler: %s (op=%s): %s", e.Reason, e.OpID, e.Detail)
	default:
		return fmt.Sprintf("scheduler: %s: %s", e.Reason, e.Detail)
	}
}

// IsError 判断 err 是否为指定原因的调度错误。
func IsError(err error, r FailReason) bool {
	if se, ok := err.(*Error); ok {
		return se.Reason == r
	}
	return false
}
