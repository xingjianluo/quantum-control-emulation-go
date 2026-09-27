package sim

import (
	"context"
	"fmt"
	"sort"

	"github.com/xingjianluo/quantum-control-emulation-go/internal/calibration"
	"github.com/xingjianluo/quantum-control-emulation-go/internal/ir"
)

// ResumeReason 描述续跑被拒绝的原因类别，便于调用方区分处理。
type ResumeReason int

const (
	ResumeErrUnknown ResumeReason = iota
	// ResumeErrMismatch 断点与作业对不上（指纹不同或断点为空）：
	// 拿错作业/状态来续跑会被明确挡掉，而不是算出看似正常的结果。
	ResumeErrMismatch
	// ResumeErrCalChanged 校准 head 已离开断点时的版本，且策略为严格模式。
	ResumeErrCalChanged
	// ResumeErrCalExpired 续跑时刻校准已过期：继续跑只会产出校准失败，
	// 且与旧结果拼接会破坏“等价于一次性跑完”的保证，故拒绝。
	ResumeErrCalExpired
	// ResumeErrCalLost 断点记录的校准版本在存储中已不存在，无法比较兼容性。
	ResumeErrCalLost
)

// String 返回原因可读名称。
func (r ResumeReason) String() string {
	switch r {
	case ResumeErrMismatch:
		return "checkpoint-mismatch"
	case ResumeErrCalChanged:
		return "calibration-changed"
	case ResumeErrCalExpired:
		return "calibration-expired"
	case ResumeErrCalLost:
		return "calibration-version-lost"
	default:
		return "unknown"
	}
}

// ResumeError 是续跑被拒绝的可分类错误。
type ResumeError struct {
	Reason  ResumeReason
	Message string
}

func (e *ResumeError) Error() string {
	return fmt.Sprintf("resume error (%s): %s", e.Reason, e.Message)
}

func resumeErr(r ResumeReason, format string, args ...any) *ResumeError {
	return &ResumeError{Reason: r, Message: fmt.Sprintf(format, args...)}
}

// ResumePolicy 决定“中断期间校准 head 发生了变化”时的续跑行为。
type ResumePolicy int

const (
	// ResumeStrict 严格模式：校准版本任何变化都拒绝续跑（ResumeErrCalChanged），
	// 绝不把旧校准下的结果与新校准下的结果拼在一起。
	ResumeStrict ResumePolicy = iota
	// ResumeRecomputeAffected 重算模式：比较断点版本与当前版本的有效参数，
	// 受影响的门（其资源参数或测量比特保真度发生变化）全部 shot 重算，
	// 未受影响的门仍复用已完成结果；受影响为空时等价于纯续跑。
	ResumeRecomputeAffected
)

// ResumeSequential 从断点续跑作业（顺序模式）。见 prepareResume 的兼容语义。
func (e *Executor) ResumeSequential(ctx context.Context, j Job, cp *Checkpoint, policy ResumePolicy) (*JobResult, error) {
	reuse, decision, err := e.prepareResume(j, cp, policy)
	if err != nil {
		return nil, err
	}
	calID := e.store.CurrentID()
	order := topoOrder(j.Schedule)
	rec := newRecorder()
	rec.event("job", "resume-decision", decision)
	per := e.execSequential(ctx, j, order, reuse, rec)
	return &JobResult{
		Reports:    aggregateReports(order, per),
		Events:     rec.events(),
		Checkpoint: newCheckpoint(j, calID, order, per),
	}, nil
}

// ResumeConcurrent 从断点续跑作业（并发模式）。语义与 ResumeSequential 相同，
// 二者对同一断点产出等价的聚合结论。
func (e *Executor) ResumeConcurrent(ctx context.Context, j Job, cp *Checkpoint, policy ResumePolicy) (*JobResult, error) {
	reuse, decision, err := e.prepareResume(j, cp, policy)
	if err != nil {
		return nil, err
	}
	calID := e.store.CurrentID()
	order := topoOrder(j.Schedule)
	rec := newRecorder()
	rec.event("job", "resume-decision", decision)
	per := e.execConcurrent(ctx, j, order, reuse, rec)
	return &JobResult{
		Reports:    aggregateReports(order, per),
		Events:     rec.events(),
		Checkpoint: newCheckpoint(j, calID, order, per),
	}, nil
}

// prepareResume 校验断点与校准兼容性，返回可复用结果表与决策说明（写日志）。
//
// 兼容规则：
//  1. 断点为空或指纹与作业不符 → ResumeErrMismatch；
//  2. 当前 head 与断点版本相同（含“回滚到同一个稳定版本”的情形，因为 head
//     指回同一版本 ID）→ 复用已完成部分、重算未完成部分；前提是该版本在
//     续跑时刻未过期，否则 ResumeErrCalExpired；
//  3. head 已变化：严格模式拒绝（ResumeErrCalChanged）；重算模式要求断点
//     版本仍可查（否则 ResumeErrCalLost）、当前版本未过期，然后按有效参数
//     差异圈定受影响门，这些门的全部 shot 重算，其余门照常复用。
func (e *Executor) prepareResume(j Job, cp *Checkpoint, policy ResumePolicy) (map[string][]shotOutcome, string, error) {
	if cp == nil {
		return nil, "", resumeErr(ResumeErrMismatch, "nil checkpoint")
	}
	key := jobFingerprint(j)
	if cp.JobKey != key {
		return nil, "", resumeErr(ResumeErrMismatch,
			"checkpoint job key %s does not match job fingerprint %s", cp.JobKey, key)
	}
	if cp.Shots != j.Shots {
		return nil, "", resumeErr(ResumeErrMismatch,
			"checkpoint shots=%d but job shots=%d", cp.Shots, j.Shots)
	}
	curID := e.store.CurrentID()
	if curID == cp.CalVersion {
		if _, err := e.store.Current(j.Now); err != nil {
			return nil, "", resumeErr(ResumeErrCalExpired,
				"calibration version %s unchanged but expired at resume time: %v", curID, err)
		}
		reuse := cp.reuseOutcomes(nil)
		return reuse, fmt.Sprintf("mode=resume same-version=%s reused-final-shots=%d recompute-rest",
			curID, cp.finalShots()), nil
	}
	// head 已离开断点版本。
	if policy == ResumeStrict {
		return nil, "", resumeErr(ResumeErrCalChanged,
			"calibration moved from %s to %s during interruption; strict policy refuses to mix results",
			cp.CalVersion, curID)
	}
	oldV, ok := e.store.VersionByID(cp.CalVersion)
	if !ok {
		return nil, "", resumeErr(ResumeErrCalLost,
			"checkpoint calibration version %s no longer exists in store", cp.CalVersion)
	}
	newV, err := e.store.Current(j.Now)
	if err != nil {
		return nil, "", resumeErr(ResumeErrCalExpired,
			"current calibration %s unusable at resume time: %v", curID, err)
	}
	globalChanged, resources, qubits := calibration.DiffEffective(oldV, newV)
	affected := affectedGates(j.Schedule, globalChanged, resources, qubits)
	if len(affected) == 0 {
		reuse := cp.reuseOutcomes(nil)
		return reuse, fmt.Sprintf("mode=recompute-affected version %s->%s affected=none reused-final-shots=%d",
			cp.CalVersion, curID, cp.finalShots()), nil
	}
	// 未受影响的门才允许复用；受影响门整门重算（旧结果产自不同校准，不可拼接）。
	unaffected := make(map[string]bool)
	for _, g := range j.Schedule.GateIDs {
		if !affected[g] {
			unaffected[g] = true
		}
	}
	reuse := cp.reuseOutcomes(unaffected)
	affectedList := make([]string, 0, len(affected))
	for g := range affected {
		affectedList = append(affectedList, g)
	}
	sort.Strings(affectedList)
	return reuse, fmt.Sprintf("mode=recompute-affected version %s->%s global-changed=%v resources=%v qubits=%v affected-gates=%v",
		cp.CalVersion, curID, globalChanged, resources, qubits, affectedList), nil
}

// affectedGates 根据校准差异圈定受影响的门：全局层变化影响所有门；
// 否则资源层参数变化的门、以及测量比特保真度变化的测量门受影响。
func affectedGates(s *ir.Schedule, globalChanged bool, resources []string, qubits []int) map[string]bool {
	out := make(map[string]bool)
	if s == nil {
		return out
	}
	if globalChanged {
		for _, g := range s.GateIDs {
			out[g] = true
		}
		return out
	}
	resSet := make(map[string]bool, len(resources))
	for _, r := range resources {
		resSet[r] = true
	}
	qSet := make(map[int]bool, len(qubits))
	for _, q := range qubits {
		qSet[q] = true
	}
	for _, g := range s.GateIDs {
		for _, p := range s.Pulses {
			if p.GateID != g {
				continue
			}
			if resSet[p.Resource] {
				out[g] = true
				break
			}
			if p.Kind == ir.PulseMeasure && qSet[measureQubit(s, g)] {
				out[g] = true
				break
			}
		}
	}
	return out
}
