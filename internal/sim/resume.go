package sim

import (
	"context"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"sort"
	"time"

	"github.com/xingjianluo/quantum-control-emulation-go/internal/calibration"
	"github.com/xingjianluo/quantum-control-emulation-go/internal/ir"
)

// ShotRecord 是检查点中单个 (门, shot) 的记录。
//
// Final=true 表示该 shot 已有终态结果（ok/measured/failed），续跑时直接复用；
// Final=false 表示该 shot 被打断时还未开始或未跑完（cancelled/timed-out），
// 续跑时必须重新执行，与真正的执行失败（failed，终态）严格区分。
type ShotRecord struct {
	Status Status
	Bit    int
	Final  bool
}

// Checkpoint 是一次被中断作业的可续跑状态。
//
// 它带走两类信息：已完成的 (门, shot) 终态结果，以及用于续跑前校验兼容性的
// 身份指纹（作业、拓扑、校准）。校验不通过时续跑被明确拒绝，而不是拼出一个
// 看似正常的结果。
type Checkpoint struct {
	JobID      string
	ScheduleFP uint64 // 脉冲计划拓扑与参数指纹
	Shots      int
	Seed       int64
	// CalibrationID/CalibrationFP 记录中断时生效的校准版本身份与有效参数指纹。
	CalibrationID string
	CalibrationFP uint64
	// Records 按门记录每个 shot 的状态；每门长度恒等于 Shots。
	Records map[string][]ShotRecord
}

// ResumePolicy 决定校准状态不兼容时的续跑行为。
type ResumePolicy int

const (
	// ResumeStrict 不兼容时拒绝续跑，返回 *ResumeError，绝不拼接旧新结果。
	ResumeStrict ResumePolicy = iota
	// ResumeRecompute 不兼容时丢弃检查点中的全部旧结果，整体重跑，
	// 并在结果中明确标注“未复用任何旧结果”。
	ResumeRecompute
)

// String 返回策略的可读名称。
func (p ResumePolicy) String() string {
	switch p {
	case ResumeStrict:
		return "strict"
	case ResumeRecompute:
		return "recompute"
	default:
		return "unknown"
	}
}

// ResumeReason 是续跑被拒绝的可区分原因。
type ResumeReason int

const (
	// ResumeMismatchJob 作业身份对不上：JobID、拓扑指纹、Shots 或 Seed 不一致。
	ResumeMismatchJob ResumeReason = iota
	// ResumeIncompatibleCalibration 校准状态与中断点不兼容（过期或有效参数已变）。
	ResumeIncompatibleCalibration
)

// String 返回原因的可读名称。
func (r ResumeReason) String() string {
	switch r {
	case ResumeMismatchJob:
		return "resume-mismatch-job"
	case ResumeIncompatibleCalibration:
		return "resume-incompatible-calibration"
	default:
		return "resume-unknown"
	}
}

// ResumeError 是续跑被明确拒绝的错误，Reason 可区分。
type ResumeError struct {
	Reason ResumeReason
	Detail string
}

// Error 实现 error。
func (e *ResumeError) Error() string {
	return e.Reason.String() + ": " + e.Detail
}

// RunResumable 执行作业并返回可用于续跑的检查点。
//
// 无论作业是跑完还是被打断（取消/超时），返回的检查点都完整记录了
// 每个 (门, shot) 的状态：终态结果可复用，未完成的标记为待重算。
func (e *Executor) RunResumable(ctx context.Context, j Job, concurrent bool) (*JobResult, *Checkpoint) {
	res, outcomes := e.runMode(ctx, j, nil, concurrent)
	return res, e.newCheckpoint(j, outcomes)
}

// Resume 从检查点续跑作业。
//
// 流程：
//  1. 校验作业身份（JobID/拓扑指纹/Shots/Seed），对不上返回
//     ResumeMismatchJob，绝不执行；
//  2. 校验校准兼容性：当前校准必须未过期且有效参数指纹与中断点一致
//     （回滚到同一稳定版本天然满足，因为有效参数逐键相同）；
//  3. 兼容：复用终态结果，只重跑未完成的 (门, shot)；
//     不兼容：strict 策略返回 ResumeIncompatibleCalibration；
//     recompute 策略丢弃全部旧结果整体重跑，并在结果事件中标注。
//
// 返回的新检查点合并了复用与重算后的全量状态，可再次用于续跑。
func (e *Executor) Resume(ctx context.Context, j Job, cp *Checkpoint, policy ResumePolicy, concurrent bool) (*JobResult, *Checkpoint, error) {
	if err := e.validateCheckpoint(j, cp); err != nil {
		return nil, nil, err
	}
	calID, calFP, calErr := e.currentCalibration(j.Now)
	compatible := calErr == nil && calFP == cp.CalibrationFP

	var prior map[string][]shotOutcome
	var decision string
	if compatible {
		prior = recordsToOutcomes(cp.Records)
		decision = fmt.Sprintf("decision=reuse calibration=%s fp=%016x matches checkpoint id=%s",
			calID, calFP, cp.CalibrationID)
	} else {
		reason := fmt.Sprintf("checkpoint calibration id=%s fp=%016x", cp.CalibrationID, cp.CalibrationFP)
		if calErr != nil {
			reason += fmt.Sprintf(", current unusable: %v", calErr)
		} else {
			reason += fmt.Sprintf(", current id=%s fp=%016x differs", calID, calFP)
		}
		if policy == ResumeStrict {
			return nil, nil, &ResumeError{
				Reason: ResumeIncompatibleCalibration,
				Detail: "resume rejected; no old/new results were merged (" + reason + ")",
			}
		}
		decision = "decision=recompute-all policy=recompute; discarded all checkpoint outcomes (" + reason + ")"
	}

	res, outcomes := e.runMode(ctx, j, prior, concurrent)
	decision += fmt.Sprintf("; reused=%d recomputed=%d", res.Reused, res.Recomputed)
	res.Events = append([]Event{{Seq: -1, GateID: "", Kind: "resume-decision", Note: decision}}, res.Events...)
	return res, e.newCheckpoint(j, outcomes), nil
}

// runMode 按模式分派到顺序/并发内部实现。
func (e *Executor) runMode(ctx context.Context, j Job, prior map[string][]shotOutcome, concurrent bool) (*JobResult, map[string][]shotOutcome) {
	if concurrent {
		return e.runConcurrent(ctx, j, prior)
	}
	return e.runSequential(ctx, j, prior)
}

// validateCheckpoint 校验检查点是否属于本作业；不属于则明确拒绝。
func (e *Executor) validateCheckpoint(j Job, cp *Checkpoint) error {
	mismatch := func(format string, args ...any) error {
		return &ResumeError{Reason: ResumeMismatchJob, Detail: fmt.Sprintf(format, args...)}
	}
	if cp == nil {
		return mismatch("nil checkpoint")
	}
	if cp.JobID != j.ID {
		return mismatch("checkpoint job id %q != job id %q", cp.JobID, j.ID)
	}
	if cp.Shots != j.Shots {
		return mismatch("checkpoint shots %d != job shots %d", cp.Shots, j.Shots)
	}
	if cp.Seed != j.Seed {
		return mismatch("checkpoint seed %d != job seed %d", cp.Seed, j.Seed)
	}
	if fp := fingerprintSchedule(j.Schedule); cp.ScheduleFP != fp {
		return mismatch("checkpoint schedule fingerprint %016x != job schedule fingerprint %016x", cp.ScheduleFP, fp)
	}
	return nil
}

// currentCalibration 返回当前校准版本 ID 与有效参数指纹；版本过期或缺失时返回错误。
func (e *Executor) currentCalibration(now time.Time) (string, uint64, error) {
	v, err := e.store.Current(now)
	if err != nil {
		return "", 0, err
	}
	return v.ID, fingerprintVersion(v), nil
}

// newCheckpoint 从一次执行的全量 (门, shot) 结果构建检查点。
func (e *Executor) newCheckpoint(j Job, outcomes map[string][]shotOutcome) *Checkpoint {
	cp := &Checkpoint{
		JobID:      j.ID,
		ScheduleFP: fingerprintSchedule(j.Schedule),
		Shots:      j.Shots,
		Seed:       j.Seed,
		Records:    make(map[string][]ShotRecord, len(outcomes)),
	}
	if v, err := e.store.Current(j.Now); err == nil {
		cp.CalibrationID = v.ID
		cp.CalibrationFP = fingerprintVersion(v)
	}
	for gate, ocs := range outcomes {
		recs := make([]ShotRecord, len(ocs))
		for i, oc := range ocs {
			recs[i] = ShotRecord{Status: oc.status, Bit: oc.bit, Final: isFinalStatus(oc.status)}
		}
		cp.Records[gate] = recs
	}
	return cp
}

// recordsToOutcomes 把检查点记录转回内部结果表示；
// 非终态记录不会被复用（见 reusableOutcome），转换本身无损。
func recordsToOutcomes(recs map[string][]ShotRecord) map[string][]shotOutcome {
	out := make(map[string][]shotOutcome, len(recs))
	for gate, rs := range recs {
		ocs := make([]shotOutcome, len(rs))
		for i, r := range rs {
			ocs[i] = shotOutcome{status: r.Status, bit: r.Bit}
		}
		out[gate] = ocs
	}
	return out
}

// fingerprintSchedule 计算脉冲计划的确定性指纹：门集合、依赖边、每个脉冲的
// 资源/类型/时序/参数全部参与，任何拓扑或参数变化都会改变指纹。
func fingerprintSchedule(s *ir.Schedule) uint64 {
	h := fnv.New64a()
	if s == nil {
		return h.Sum64()
	}
	ids := append([]string(nil), s.GateIDs...)
	sort.Strings(ids)
	for _, id := range ids {
		hashStr(h, "gate")
		hashStr(h, id)
		deps := append([]string(nil), s.GateDeps[id]...)
		sort.Strings(deps)
		for _, d := range deps {
			hashStr(h, d)
		}
	}
	pulses := append([]ir.Pulse(nil), s.Pulses...)
	sort.Slice(pulses, func(i, j int) bool { return pulses[i].ID < pulses[j].ID })
	for _, p := range pulses {
		hashStr(h, p.ID)
		hashStr(h, p.GateID)
		hashStr(h, p.Resource)
		hashInt(h, int64(p.Kind))
		hashInt(h, p.StartNS)
		hashInt(h, p.DurationNS)
		keys := make([]string, 0, len(p.Params))
		for k := range p.Params {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			hashStr(h, k)
			hashInt(h, int64(p.Params[k]*1e9))
		}
	}
	return h.Sum64()
}

// fingerprintVersion 计算校准版本有效参数的确定性指纹（不含版本 ID：
// 两个版本有效参数逐键相同即视为兼容，例如回滚到同一稳定版本）。
func fingerprintVersion(v calibration.Version) uint64 {
	h := fnv.New64a()
	layers := make([]string, 0, len(v.Parameters))
	for l := range v.Parameters {
		layers = append(layers, l)
	}
	sort.Strings(layers)
	for _, l := range layers {
		hashStr(h, "layer")
		hashStr(h, l)
		keys := make([]string, 0, len(v.Parameters[l]))
		for k := range v.Parameters[l] {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			hashStr(h, k)
			hashInt(h, int64(v.Parameters[l][k]*1e9))
		}
	}
	qs := make([]int, 0, len(v.ReadoutFid))
	for q := range v.ReadoutFid {
		qs = append(qs, q)
	}
	sort.Ints(qs)
	for _, q := range qs {
		hashStr(h, "fid")
		hashInt(h, int64(q))
		hashInt(h, int64(v.ReadoutFid[q]*1e9))
	}
	return h.Sum64()
}

func hashStr(h interface{ Write([]byte) (int, error) }, s string) {
	_, _ = h.Write([]byte(s))
	_, _ = h.Write([]byte{0})
}

func hashInt(h interface{ Write([]byte) (int, error) }, v int64) {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], uint64(v))
	_, _ = h.Write(b[:])
}
