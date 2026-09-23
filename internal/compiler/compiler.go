// Package compiler 把高层门任务编译为绑定校准参数的脉冲操作列表（不含时序）。
package compiler

import (
	"math"
	"time"

	"github.com/xingjianluo/quantum-control-emulation-go/internal/calibration"
	"github.com/xingjianluo/quantum-control-emulation-go/internal/ir"
)

// FailReason 区分编译被拒绝的原因。
type FailReason string

const (
	ReasonUnknownGate        FailReason = "unknown_gate"
	ReasonIllegalParam       FailReason = "illegal_param"
	ReasonDuplicateID        FailReason = "duplicate_task_id"
	ReasonUnresolvedDep      FailReason = "unresolved_dependency"
	ReasonMissingCalibration FailReason = "missing_calibration"
)

// 默认脉冲时长（cfg.Durations 未配置时使用）。
var defaultDurations = map[ir.GateKind]time.Duration{
	ir.GateX:    30 * time.Nanosecond,
	ir.GateX90:  20 * time.Nanosecond,
	ir.GateRZ:   0, // RZ 为虚拟相位门，无脉冲时长
	ir.GateCNOT: 80 * time.Nanosecond,
	ir.GateMeas: 200 * time.Nanosecond,
}

// Config 是编译器配置。
type Config struct {
	Durations map[ir.GateKind]time.Duration
	TTL       time.Duration // 校准快照允许的有效期；<=0 表示不过期
}

// Compiler 绑定一个校准仓库。
type Compiler struct {
	store *calibration.Store
	cfg   Config
}

// New 创建编译器。
func New(store *calibration.Store, cfg Config) *Compiler {
	if cfg.Durations == nil {
		cfg.Durations = map[ir.GateKind]time.Duration{}
	}
	return &Compiler{store: store, cfg: cfg}
}

func (c *Compiler) dur(k ir.GateKind) time.Duration {
	if d, ok := c.cfg.Durations[k]; ok {
		return d
	}
	return defaultDurations[k]
}

// Compile 将门任务编译为脉冲计划：
//  1. 校验任务 ID 唯一、门类型已知、参数合法、依赖可解析；
//  2. 绑定当前稳定校准版本（必须存在、未过期），各门所需参数必须齐备；
//  3. 每个门映射为一条脉冲操作，保留 GateID 与依赖关系以便解释与重放。
//
// 编译结果的 Start 字段留零，由 scheduler 排时。
func (c *Compiler) Compile(tasks []ir.GateTask, at time.Time) (*ir.PulseProgram, error) {
	seen := make(map[string]int, len(tasks))
	for i, t := range tasks {
		if t.ID == "" {
			return nil, &Error{Reason: ReasonIllegalParam, Detail: "门任务 ID 不能为空"}
		}
		if _, dup := seen[t.ID]; dup {
			return nil, &Error{Reason: ReasonDuplicateID, GateID: t.ID,
				Detail: "任务 ID 重复"}
		}
		seen[t.ID] = i
		if err := validateTask(t); err != nil {
			return nil, err
		}
	}
	for _, t := range tasks {
		for _, dep := range t.DependsOn {
			if _, ok := seen[dep]; !ok {
				return nil, &Error{Reason: ReasonUnresolvedDep, GateID: t.ID,
					Detail: "依赖的任务不存在: " + dep}
			}
			if dep == t.ID {
				return nil, &Error{Reason: ReasonUnresolvedDep, GateID: t.ID,
					Detail: "任务不能依赖自身"}
			}
		}
	}

	snap, err := c.store.MustCurrent(at, c.cfg.TTL)
	if err != nil {
		return nil, err // 直接透传可区分的校准错误（过期/无稳定版等）
	}

	prog := &ir.PulseProgram{CalibVersion: snap.Version}
	for _, t := range tasks {
		op, err := c.emit(t, snap)
		if err != nil {
			return nil, err
		}
		prog.Ops = append(prog.Ops, op)
	}
	return prog, nil
}

func validateTask(t ir.GateTask) error {
	bad := func(detail string) error {
		return &Error{Reason: ReasonIllegalParam, GateID: t.ID, Detail: detail}
	}
	switch t.Kind {
	case ir.GateX, ir.GateX90:
		if len(t.Qubits) != 1 {
			return bad("单比特门需要恰好 1 个比特")
		}
		if t.Qubits[0] < 0 {
			return bad("比特编号不能为负")
		}
	case ir.GateRZ:
		if len(t.Qubits) != 1 {
			return bad("RZ 需要恰好 1 个比特")
		}
		if t.Qubits[0] < 0 {
			return bad("比特编号不能为负")
		}
		if math.IsNaN(t.Angle) || math.IsInf(t.Angle, 0) {
			return bad("RZ 角度必须是有限实数")
		}
	case ir.GateCNOT:
		if len(t.Qubits) != 2 {
			return bad("CNOT 需要恰好 2 个比特 [control,target]")
		}
		if t.Qubits[0] < 0 || t.Qubits[1] < 0 || t.Qubits[0] == t.Qubits[1] {
			return bad("CNOT 比特编号必须非负且互不相同")
		}
	case ir.GateMeas:
		if len(t.Qubits) != 1 {
			return bad("MEASURE 需要恰好 1 个比特")
		}
		if t.Qubits[0] < 0 {
			return bad("比特编号不能为负")
		}
	default:
		return &Error{Reason: ReasonUnknownGate, GateID: t.ID, Detail: "未知门类型: " + string(t.Kind)}
	}
	return nil
}

// emit 将单个合法门映射为脉冲操作并校验校准参数齐备。
func (c *Compiler) emit(t ir.GateTask, snap *calibration.Snapshot) (*ir.PulseOp, error) {
	op := &ir.PulseOp{
		ID:           "p-" + t.ID,
		GateID:       t.ID,
		Deps:         append([]string(nil), t.DependsOn...),
		Start:        0,
		CalibVersion: snap.Version,
	}
	// 任务依赖是 GateID；脉冲 ID 是 "p-"+GateID。
	for i := range op.Deps {
		op.Deps[i] = "p-" + op.Deps[i]
	}
	copyParams := func(key calibration.GateKey, need []string) (map[string]float64, error) {
		ps, ok := snap.GateParams[key]
		if !ok {
			return nil, &Error{Reason: ReasonMissingCalibration, GateID: t.ID,
				Detail: "缺少校准参数: " + string(key)}
		}
		out := map[string]float64{}
		for _, n := range need {
			v, ok := ps[n]
			if !ok {
				return nil, &Error{Reason: ReasonMissingCalibration, GateID: t.ID,
					Detail: "校准参数缺项: " + string(key) + "." + n}
			}
			out[n] = v
		}
		return out, nil
	}

	switch t.Kind {
	case ir.GateX, ir.GateX90:
		q := t.Qubits[0]
		params, err := copyParams(calibration.MakeGateKey(t.Kind, q), []string{"amp", "freq"})
		if err != nil {
			return nil, err
		}
		op.Kind = ir.OpPulse
		op.Resources = []ir.Resource{{Kind: ir.ResourceQubit, Name: qubitName(q)}}
		op.Params = params
		op.Duration = c.dur(t.Kind)
	case ir.GateRZ:
		q := t.Qubits[0]
		key := calibration.MakeGateKey(t.Kind, q)
		if _, ok := snap.GateParams[key]; !ok {
			return nil, &Error{Reason: ReasonMissingCalibration, GateID: t.ID,
				Detail: "缺少校准参数: " + string(key)}
		}
		op.Kind = ir.OpPulse
		op.Resources = []ir.Resource{{Kind: ir.ResourceQubit, Name: qubitName(q)}}
		op.Params = map[string]float64{"angle": t.Angle}
		op.Duration = c.dur(t.Kind)
	case ir.GateCNOT:
		ctl, tgt := t.Qubits[0], t.Qubits[1]
		params, err := copyParams(calibration.MakeGateKey(t.Kind, ctl, tgt), []string{"amp", "freq"})
		if err != nil {
			return nil, err
		}
		op.Kind = ir.OpPulse
		op.Resources = []ir.Resource{
			{Kind: ir.ResourceCoupler, Name: couplerName(ctl, tgt)},
			{Kind: ir.ResourceQubit, Name: qubitName(ctl)},
			{Kind: ir.ResourceQubit, Name: qubitName(tgt)},
		}
		op.Params = params
		op.Duration = c.dur(t.Kind)
	case ir.GateMeas:
		q := t.Qubits[0]
		if f, ok := snap.ReadoutFidelity[q]; !ok || f < 0 || f > 1 {
			return nil, &Error{Reason: ReasonMissingCalibration, GateID: t.ID,
				Detail: "缺少合法的读出保真度: qubit " + itoa(q)}
		}
		op.Kind = ir.OpReadout
		op.Resources = []ir.Resource{{Kind: ir.ResourceReadout, Name: readoutName(q)}}
		op.Params = map[string]float64{}
		op.Duration = c.dur(t.Kind)
	}
	return op, nil
}

func qubitName(q int) string   { return "qubit-" + itoa(q) }
func readoutName(q int) string { return "readout-" + itoa(q) }
func couplerName(a, b int) string {
	return "coupler-" + itoa(a) + "-" + itoa(b)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
