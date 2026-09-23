// Package calibration 管理门脉冲参数与读出保真度的分层版本：
// 草稿(draft) -> 稳定(stable)/失败(failed)，支持过期检测、容差校验与回滚。
package calibration

import (
	"strconv"
	"time"

	"github.com/xingjianluo/quantum-control-emulation-go/internal/ir"
)

// Status 是校准快照的生命周期状态。
type Status string

const (
	StatusDraft  Status = "draft"
	StatusStable Status = "stable"
	StatusFailed Status = "failed"
)

// GateKey 是规范化的门校准键，如 "X:0"、"RZ:1"、"CNOT:0,1"、"MEASURE:2"。
// 采用字符串以便作为 map 键；请始终通过 MakeGateKey 构造。
type GateKey string

// MakeGateKey 生成规范化门键，CNOT 等多比特门保持入参顺序（control,target）。
func MakeGateKey(kind ir.GateKind, qubits ...int) GateKey {
	b := make([]byte, 0, 16)
	b = append(b, string(kind)...)
	b = append(b, ':')
	for i, q := range qubits {
		if i > 0 {
			b = append(b, ',')
		}
		if q < 10 {
			b = append(b, byte('0'+q))
		} else {
			b = append(b, strconv.Itoa(q)...)
		}
	}
	return GateKey(b)
}

// Snapshot 是某一版本的全部校准数据（已提交快照不可变；访问器均返回副本）。
type Snapshot struct {
	Version         int
	Status          Status
	CreatedAt       time.Time
	Note            string
	GateParams      map[GateKey]map[string]float64
	ReadoutFidelity map[int]float64 // qubit -> 正确读出概率 [0,1]
}

// Tolerance 描述提交新校准时相对于上一稳定版本允许的参数漂移，以及读出保真度约束。
type Tolerance struct {
	ParamDelta map[string]float64 // 参数名 -> 最大绝对偏差；未列出的参数默认 0 容差
	ReadoutMin float64            // 读出保真度下限
	ReadoutMax float64            // 读出保真度上限
}

// Event 是校准仓库审计日志中的一条不可变记录。
type Event struct {
	Version int
	Kind    string // begin | commit | fail | rollback
	At      time.Time
	Detail  string
}
