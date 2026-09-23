package compiler

import "fmt"

// Error 携带可程序化判别的编译失败原因。
type Error struct {
	Reason FailReason
	GateID string
	Detail string
}

func (e *Error) Error() string {
	if e.GateID != "" {
		return fmt.Sprintf("compiler: %s at gate %q: %s", e.Reason, e.GateID, e.Detail)
	}
	return fmt.Sprintf("compiler: %s: %s", e.Reason, e.Detail)
}

// IsError 判断 err 是否为指定原因的编译错误。
func IsError(err error, r FailReason) bool {
	if ce, ok := err.(*Error); ok {
		return ce.Reason == r
	}
	return false
}
