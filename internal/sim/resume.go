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
	// CalibrationID/CalibrationFP 记录中断时生效的校准版本身份与全量有效参数指纹。
	CalibrationID string
	CalibrationFP uint64
	// GateCalibration 是门级有效校准指纹：只含该门脉冲实际使用的资源的
	// 合并有效参数与该门测量比特的读出保真度。续跑时逐门比对——只有指纹
	// 变化的门才需要重算，其余门的已完成结果在不同版本下依然兼容可复用。
	GateCalibration map[string]uint64
	// Records 按门记录每个 shot 的状态；每门长度恒等于 Shots。
	Records map[string][]ShotRecord
}

// ResumePolicy 决定校准状态不兼容时的续跑行为。
type ResumePolicy int

const (
	// ResumeStrict 存在校准变化时拒绝续跑，返回 *ResumeError，绝不拼接旧新结果。
	ResumeStrict ResumePolicy = iota
	// ResumeRecompute 校准换了一份时按门级影响范围重算：只有有效校准发生
	// 变化的门整体重算（含其已完成 shot），未受影响的门继续复用已完成结果；
	// 不同校准下产生的结果不会混在同一个门里。
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
	// ResumeCalibrationUnavailable 当前校准不可用：已过期、不存在或缺读出保真度。
	// 两种策略下都必须拒绝恢复——执行核只会把每个 shot 判成 failed，
	// 绝不能返回一个整体失败的“正常结果”，也不能把旧结果拼进去。
	ResumeCalibrationUnavailable
	// ResumeCalibrationChanged 当前校准可用，但换成了另一份（有效参数/保真度
	// 与中断点不一致）。strict 策略据此拒绝；recompute 策略按门级范围重算。
	ResumeCalibrationChanged
)

// String 返回原因的可读名称。
func (r ResumeReason) String() string {
	switch r {
	case ResumeMismatchJob:
		return "resume-mismatch-job"
	case ResumeCalibrationUnavailable:
		return "resume-calibration-unavailable"
	case ResumeCalibrationChanged:
		return "resume-calibration-changed"
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
//  1. 校验作业身份（JobID/拓扑指纹/Shots/Seed）与检查点记录完整性，
//     对不上返回 ResumeMismatchJob，绝不执行；
//  2. 取当前校准。已过期/不存在/缺键时，无论 strict 还是 recompute，
//     一律返回 ResumeCalibrationUnavailable——不执行、不拼接旧结果；
//  3. 逐门比对门级校准指纹：
//     - 没有任何门变化（含回滚到同一稳定版本、有效参数与中断点一致）：
//     全部终态结果复用，只重跑未完成的 (门, shot)；
//     - 有门变化：strict 策略返回 ResumeCalibrationChanged（拒绝执行）；
//     recompute 策略只对受影响的门整体重算（该门的旧结果一律不进入 prior），
//     未受影响门的终态结果继续复用；待重算门无论校准是否变化都重跑。
//
// 返回的新检查点合并了复用与重算后的全量状态（盖当前校准指纹），可再次续跑。
func (e *Executor) Resume(ctx context.Context, j Job, cp *Checkpoint, policy ResumePolicy, concurrent bool) (*JobResult, *Checkpoint, error) {
	if err := e.validateCheckpoint(j, cp); err != nil {
		return nil, nil, err
	}
	v, err := e.store.Current(j.Now)
	if err != nil {
		return nil, nil, &ResumeError{
			Reason: ResumeCalibrationUnavailable,
			Detail: fmt.Sprintf(
				"resume rejected; no old/new results were merged "+
					"(checkpoint calibration id=%s fp=%016x, current calibration unusable at %s: %v)",
				cp.CalibrationID, cp.CalibrationFP, j.Now.Format(time.RFC3339), err),
		}
	}
	// 版本级可读还不够：按执行核 runGate 的真实读取路径预检每个门用到的
	// (资源, 比特)——缺某比特读出保真度等情况下执行核只会把 shot 判 failed，
	// 这同样属于“校准不可用”，两种策略都必须拒绝，不能跑出整体失败的结果。
	if detail, ok := calibrationUsableFor(e, j); !ok {
		return nil, nil, &ResumeError{
			Reason: ResumeCalibrationUnavailable,
			Detail: fmt.Sprintf(
				"resume rejected; no old/new results were merged "+
					"(checkpoint calibration id=%s, current id=%s unusable for this schedule: %s)",
				cp.CalibrationID, v.ID, detail),
		}
	}
	curID, curFP := v.ID, fingerprintVersion(v)

	// 逐门判定哪些门的有效校准相对中断点发生了变化。
	affected := affectedGates(j.Schedule, cp.GateCalibration, v)

	var prior map[string][]shotOutcome
	var markers []Event
	var decision string

	switch {
	case len(affected) == 0:
		prior = recordsToOutcomes(cp.Records)
		decision = fmt.Sprintf("decision=reuse calibration=%s fp=%016x matches checkpoint id=%s; "+
			"all gate-level calibration fingerprints match (rollback/equal effective params included)",
			curID, curFP, cp.CalibrationID)
	case policy == ResumeStrict:
		affectedList := gatesInOrder(j.Schedule, affected)
		return nil, nil, &ResumeError{
			Reason: ResumeCalibrationChanged,
			Detail: fmt.Sprintf(
				"resume rejected; no old/new results were merged (checkpoint calibration id=%s fp=%016x, "+
					"current id=%s fp=%016x; %d/%d gates have changed effective calibration: %v)",
				cp.CalibrationID, cp.CalibrationFP, curID, curFP,
				len(affectedList), len(j.Schedule.GateIDs), affectedList),
		}
	default:
		// recompute：受影响门的旧结果全部丢弃（整体重算），未受影响门继续复用。
		prior = scopedPrior(j.Schedule, cp, affected)
		affectedList := gatesInOrder(j.Schedule, affected)
		markers = make([]Event, 0, len(affectedList))
		for _, g := range affectedList {
			markers = append(markers, Event{Seq: -1, GateID: g, Kind: "recompute-scope",
				Note: fmt.Sprintf("gate=%s reason=calibration-changed checkpoint-fp=%016x current-fp=%016x; all shots rerun",
					g, cp.GateCalibration[g], gateCalibrationFP(j.Schedule, g, v))})
		}
		decision = fmt.Sprintf("decision=recompute-scoped policy=recompute current=%s checkpoint=%s; "+
			"reran=%d affected gates %v; unaffected gates reused checkpoint outcomes",
			curID, cp.CalibrationID, len(affectedList), affectedList)
	}

	res, outcomes := e.runMode(ctx, j, prior, concurrent)
	decision += fmt.Sprintf("; reused=%d recomputed=%d", res.Reused, res.Recomputed)

	// 决策与范围标记放在事件流最前面（Seq=-1，保持确定性顺序），逐条可审计。
	head := make([]Event, 0, len(markers)+1)
	head = append(head, Event{Seq: -1, GateID: "", Kind: "resume-decision", Note: decision})
	head = append(head, markers...)
	res.Events = append(head, res.Events...)
	return res, e.newCheckpoint(j, outcomes), nil
}

// scopedPrior 构建按门级范围裁剪后的先验结果：
// 受影响门的记录（无论终态与否）一律不进入先验，等价于该门全部重算；
// 其余门保留原记录，执行核只会复用其中的终态结果。
func scopedPrior(s *ir.Schedule, cp *Checkpoint, affected map[string]bool) map[string][]shotOutcome {
	out := make(map[string][]shotOutcome, len(cp.Records))
	for gate, rs := range cp.Records {
		if affected[gate] {
			continue
		}
		ocs := make([]shotOutcome, len(rs))
		for i, r := range rs {
			ocs[i] = shotOutcome{status: r.Status, bit: r.Bit}
		}
		out[gate] = ocs
	}
	return out
}

// affectedGates 逐门比较检查点门级指纹与当前版本指纹，返回有效校准发生
// 变化的门集合。检查点缺少门级指纹的门保守地视为受影响。
func affectedGates(s *ir.Schedule, checkpointFP map[string]uint64, v calibration.Version) map[string]bool {
	out := make(map[string]bool)
	for _, g := range topoOrder(s) {
		cur := gateCalibrationFP(s, g, v)
		old, ok := checkpointFP[g]
		if !ok || old != cur {
			out[g] = true
		}
	}
	return out
}

// gatesInOrder 按确定性拓扑序返回集合中的门。
func gatesInOrder(s *ir.Schedule, set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for _, g := range topoOrder(s) {
		if set[g] {
			out = append(out, g)
		}
	}
	return out
}

// calibrationUsableFor 按 runGate 的真实读取路径验证当前校准对本作业是否可用：
// 每个脉冲都要能取到其 (资源, 比特) 的有效参数；测量脉冲还要能取到测量比特
// 的读出保真度。任一读取失败即不可用，返回可读的失败明细。
func calibrationUsableFor(e *Executor, j Job) (string, bool) {
	seen := make(map[string]bool)
	for _, gateID := range topoOrder(j.Schedule) {
		pulses := gatePulses(j.Schedule, gateID)
		measureQ, measureRes := 0, ""
		hasMeasure := false
		for _, p := range pulses {
			qubit := qubitOf(p.Resource)
			if p.Kind == ir.PulseMeasure {
				qubit = measureQubit(j.Schedule, gateID)
				measureQ, measureRes, hasMeasure = qubit, p.Resource, true
			}
			key := fmt.Sprintf("p:%s:%d", p.Resource, qubit)
			if !seen[key] {
				seen[key] = true
				if _, _, cerr := e.store.CurrentFor(p.Resource, qubit, j.Now); cerr != nil {
					return fmt.Sprintf("gate %s pulse %s CurrentFor(%s,%d): %v",
						gateID, p.ID, p.Resource, qubit, cerr), false
				}
			}
		}
		if hasMeasure {
			// 测量门额外读取该比特的读出保真度（资源参数已在上面覆盖）。
			key := fmt.Sprintf("m:%s:%d", measureRes, measureQ)
			if !seen[key] {
				seen[key] = true
				if _, _, cerr := e.store.CurrentFor(measureRes, measureQ, j.Now); cerr != nil {
					return fmt.Sprintf("gate %s readout fidelity for qubit %d: %v",
						gateID, measureQ, cerr), false
				}
			}
		}
	}
	return "", true
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
	// 记录完整性：每个门都必须有且仅有 Shots 条记录，残缺检查点不得参与复用。
	for _, g := range j.Schedule.GateIDs {
		recs, ok := cp.Records[g]
		if !ok {
			return mismatch("checkpoint has no records for gate %q", g)
		}
		if len(recs) != j.Shots {
			return mismatch("checkpoint gate %q has %d records, want %d", g, len(recs), j.Shots)
		}
	}
	return nil
}

// newCheckpoint 从一次执行的全量 (门, shot) 结果构建检查点。
func (e *Executor) newCheckpoint(j Job, outcomes map[string][]shotOutcome) *Checkpoint {
	cp := &Checkpoint{
		JobID:           j.ID,
		ScheduleFP:      fingerprintSchedule(j.Schedule),
		Shots:           j.Shots,
		Seed:            j.Seed,
		GateCalibration: make(map[string]uint64, len(j.Schedule.GateIDs)),
		Records:         make(map[string][]ShotRecord, len(outcomes)),
	}
	if v, err := e.store.Current(j.Now); err == nil {
		cp.CalibrationID = v.ID
		cp.CalibrationFP = fingerprintVersion(v)
		for _, g := range j.Schedule.GateIDs {
			cp.GateCalibration[g] = gateCalibrationFP(j.Schedule, g, v)
		}
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

// gateCalibrationFP 计算单个门的“门级有效校准”指纹。
//
// 纳入项恰好是 runGate 执行该门时会读取的校准量：
//   - 门内每个脉冲所在资源的合并有效参数（全局层继承 + 资源层覆盖）；
//   - 测量脉冲对应比特的读出保真度。
//
// 门没有用到的资源/比特不参与，因此只改 q1.ctrl 的校准只会让触及 q1 的
// 门指纹变化，其它门的已完成结果依然兼容。
func gateCalibrationFP(s *ir.Schedule, gateID string, v calibration.Version) uint64 {
	h := fnv.New64a()
	resources, qubits := gateCalibrationInputs(s, gateID)
	for _, r := range resources {
		hashStr(h, "resource")
		hashStr(h, r)
		merged := mergedEffective(v.Parameters, r)
		keys := make([]string, 0, len(merged))
		for k := range merged {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			hashStr(h, k)
			hashInt(h, int64(merged[k]*1e9))
		}
	}
	for _, q := range qubits {
		hashStr(h, "fid")
		hashInt(h, int64(q))
		if fid, ok := v.ReadoutFid[q]; ok {
			hashInt(h, int64(fid*1e9))
		} else {
			// 缺保真度时写入一个不可能与正常值碰撞的标记；
			// 真正的可用性拒绝在执行核/调用方取 CurrentFor 时触发。
			hashStr(h, "missing-fid")
		}
	}
	return h.Sum64()
}

// gateCalibrationInputs 收集一个门的脉冲触及的资源集合与测量比特集合
// （均去重并排序），与 runGate 的读取路径保持一致。
func gateCalibrationInputs(s *ir.Schedule, gateID string) (resources []string, qubits []int) {
	resSet := make(map[string]bool)
	qSet := make(map[int]bool)
	for _, p := range s.Pulses {
		if p.GateID != gateID {
			continue
		}
		resSet[p.Resource] = true
		if p.Kind == ir.PulseMeasure {
			qSet[measureQubit(s, gateID)] = true
		}
	}
	for r := range resSet {
		resources = append(resources, r)
	}
	sort.Strings(resources)
	for q := range qSet {
		qubits = append(qubits, q)
	}
	sort.Ints(qubits)
	return resources, qubits
}

// mergedEffective 返回某资源层继承全局层覆盖后的合并有效参数（拷贝）。
func mergedEffective(params map[string]map[string]float64, resource string) map[string]float64 {
	out := make(map[string]float64)
	for k, val := range params[calibration.GlobalLayer] {
		out[k] = val
	}
	for k, val := range params[resource] {
		out[k] = val
	}
	return out
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
