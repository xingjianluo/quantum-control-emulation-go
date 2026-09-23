package compile

import "fmt"

// Reason 描述计划被拒绝的具体原因类别，便于上层区分处理。
type Reason int

const (
	ReasonUnknown Reason = iota
	// ReasonConflict 同一硬件资源上存在无依赖关系的重叠占用。
	ReasonConflict
	// ReasonIllegalParam 门/脉冲参数非法，例如比特越界、门元数不符、参数越界。
	ReasonIllegalParam
	// ReasonUnsatisfiableDep 依赖缺失、成环或引用了不存在的门。
	ReasonUnsatisfiableDep
)

// String 返回原因的可读名称。
func (r Reason) String() string {
	switch r {
	case ReasonConflict:
		return "resource-conflict"
	case ReasonIllegalParam:
		return "illegal-param"
	case ReasonUnsatisfiableDep:
		return "unsatisfiable-dependency"
	default:
		return "unknown"
	}
}

// Error 是编译阶段的可分类错误。
type Error struct {
	Reason  Reason
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("compile error (%s): %s", e.Reason, e.Message)
}

func newError(r Reason, format string, args ...any) *Error {
	return &Error{Reason: r, Message: fmt.Sprintf(format, args...)}
}
