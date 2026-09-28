package sim

import (
	"context"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
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
	// CalibrationParams 是中断时本作业实际用到的各资源层“合并全局层后”的
	// 有效参数快照（键为资源，如 q0.ctrl / measure.bus）；
	// CalibrationFid 是本作业测量门实际用到的各比特读出保真度快照。
	// 续跑时逐键与当前校准比较，据此判断哪些任务的校准前提已变、必须重算。
	// 中断时校准已不可用则快照为 nil（续跑时按“基线未知”保守处理）。
	CalibrationParams map[string]map[string]float64
	CalibrationFid    map[int]float64
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
	// ResumeCalibrationUnavailable 当前校准已过期或不可用（无版本等）。
	// 无论调用方选择哪种恢复策略都必须拒绝：禁止把旧结果拼成一份看似
	// 正常、实则整体失败的结果。
	ResumeCalibrationUnavailable
	// ResumeCalibrationChanged 当前校准可用，但有效参数/保真度相对中断点已
	// 变化。strict 策略拒绝；recompute 策略只重算受影响的任务。
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
//  1. 校验作业身份（JobID/拓扑指纹/Shots/Seed），对不上返回
//     ResumeMismatchJob，绝不执行；
//  2. 读取当前校准：已过期或不可用时，无论哪种策略都返回
//     ResumeCalibrationUnavailable，明确拒绝恢复——绝不拼出一份整体
//     变成失败状态的“正常”结果，也不把旧结果带回新结果；
//  3. 在本作业实际用到的资源/比特范围内逐键比较当前有效参数与中断点快照：
//     - 完全一致（含回滚到同一个稳定版本）：复用全部终态结果；
//     - 存在差异：strict 策略返回 ResumeCalibrationChanged，拒绝恢复；
//     recompute 策略只把受差异影响的门标记为重算，其余终态结果继续复用；
//  4. 无论哪种处置，都写入一条 resume-decision 事件，日志可区分复用、
//     按范围重算与拒绝原因。
//
// 返回的新检查点合并了复用与重算后的全量状态，并以当前校准为基线，
// 可再次用于续跑。
func (e *Executor) Resume(ctx context.Context, j Job, cp *Checkpoint, policy ResumePolicy, concurrent bool) (*JobResult, *Checkpoint, error) {
	if err := e.validateCheckpoint(j, cp); err != nil {
		return nil, nil, err
	}
	cur, calErr := e.currentCalibration(j)
	if calErr != nil {
		// 当前校准不可用（过期/无版本）：两种策略都拒绝，绝不执行、绝不拼接。
		return nil, nil, &ResumeError{
			Reason: ResumeCalibrationUnavailable,
			Detail: fmt.Sprintf("resume rejected; current calibration unusable at %s (%v); "+
				"checkpoint calibration id=%s; no old/new results were merged",
				j.Now.Format(time.RFC3339), calErr, cp.CalibrationID),
		}
	}

	// 只在本作业实际用到的范围内比较：作业不碰的资源/比特变化与本作业无关。
	diff := diffCalibration(cp, cur, calibrationScope(j.Schedule))
	var prior map[string][]shotOutcome
	var decision string
	switch {
	case diff.empty():
		prior = recordsToOutcomes(cp.Records)
		decision = fmt.Sprintf("decision=reuse policy=%s calibration=%s fp=%016x matches checkpoint id=%s "+
			"(effective params identical within job scope)",
			policy, cur.id, cur.fp, cp.CalibrationID)
	case policy == ResumeStrict:
		return nil, nil, &ResumeError{
			Reason: ResumeCalibrationChanged,
			Detail: fmt.Sprintf("resume rejected; calibration changed since checkpoint id=%s fp=%016x; "+
				"current=%s fp=%016x; changed=%s; no old/new results were merged",
				cp.CalibrationID, cp.CalibrationFP, cur.id, cur.fp, diff.describe()),
		}
	default:
		// 按范围重算：只重算其校准前提发生变化的门，其他终态结果继续复用。
		affected := affectedGates(j.Schedule, diff)
		prior = filterReusable(cp.Records, affected)
		decision = fmt.Sprintf("decision=recompute-scoped policy=%s changed=%s; affected-gates=%s; "+
			"all other final outcomes are reused; checkpoint id=%s current=%s",
			policy, diff.describe(), sortedGateNames(affected), cp.CalibrationID, cur.id)
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

// currentCalView 是当前校准在本作业范围内的有效视图。
type currentCalView struct {
	id     string
	fp     uint64
	params map[string]map[string]float64 // 资源层 -> 合并全局层后的有效参数
	fid    map[int]float64               // 比特 -> 读出保真度
}

// currentCalibration 返回当前校准版本在本作业实际用到的资源/比特范围内的
// 有效参数视图；版本过期或缺失时返回错误（调用方必须明确拒绝恢复）。
func (e *Executor) currentCalibration(j Job) (currentCalView, error) {
	v, err := e.store.Current(j.Now)
	if err != nil {
		return currentCalView{}, err
	}
	scope := calibrationScope(j.Schedule)
	params := effectiveParams(v, scope.resources)
	fid := make(map[int]float64, len(scope.qubits))
	for _, q := range scope.qubits {
		f, ok := v.ReadoutFid[q]
		if !ok {
			// 作业测量门要用的比特没有保真度：当前校准对本作业不可用。
			return currentCalView{}, &calibration.Error{
				Reason:  calibration.FailMissingKey,
				Message: fmt.Sprintf("no readout fidelity for qubit %d in version %s", q, v.ID),
			}
		}
		fid[q] = f
	}
	return currentCalView{id: v.ID, fp: fingerprintVersion(v), params: params, fid: fid}, nil
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
		scope := calibrationScope(j.Schedule)
		cp.CalibrationID = v.ID
		cp.CalibrationFP = fingerprintVersion(v)
		cp.CalibrationParams = effectiveParams(v, scope.resources)
		cp.CalibrationFid = make(map[int]float64, len(scope.qubits))
		for _, q := range scope.qubits {
			if f, ok := v.ReadoutFid[q]; ok {
				cp.CalibrationFid[q] = f
			}
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

// filterReusable 按“受影响门集合”裁剪检查点：受影响门的旧结果（即使是
// 终态）一律丢弃，保证不同校准下产生的结果不会混在一起；其余门保留全部
// 记录（非终态记录本来也不会被复用，它们代表被打断、必须重算的 shot）。
func filterReusable(recs map[string][]ShotRecord, affected map[string]bool) map[string][]shotOutcome {
	out := make(map[string][]shotOutcome, len(recs))
	for gate, rs := range recs {
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

// calScope 描述本作业实际依赖校准的范围：脉冲用到的资源层与测量比特。
type calScope struct {
	resources []string
	qubits    []int
}

// calibrationScope 从脉冲计划提取作业实际用到的校准资源与测量比特（有序去重）。
func calibrationScope(s *ir.Schedule) calScope {
	sc := calScope{}
	if s == nil {
		return sc
	}
	resSet := make(map[string]bool)
	qSet := make(map[int]bool)
	for _, p := range s.Pulses {
		resSet[p.Resource] = true
		if p.Kind == ir.PulseMeasure {
			qSet[measureQubit(s, p.GateID)] = true
		}
	}
	for r := range resSet {
		sc.resources = append(sc.resources, r)
	}
	sort.Strings(sc.resources)
	for q := range qSet {
		sc.qubits = append(sc.qubits, q)
	}
	sort.Ints(sc.qubits)
	return sc
}

// effectiveParams 返回给定版本在指定资源层上“合并全局层后”的有效参数拷贝，
// 与执行核 runGate 的取值方式一致。全局层 "*" 也单独纳入快照，以便检测
// 全局参数变化（它影响作业的每一个门）。
func effectiveParams(v calibration.Version, resources []string) map[string]map[string]float64 {
	layers := make([]string, 0, len(resources)+1)
	layers = append(layers, calibration.GlobalLayer)
	for _, r := range resources {
		if r != calibration.GlobalLayer {
			layers = append(layers, r)
		}
	}
	out := make(map[string]map[string]float64, len(layers))
	for _, r := range layers {
		merged := make(map[string]float64)
		for k, val := range v.Parameters[calibration.GlobalLayer] {
			merged[k] = val
		}
		if r != calibration.GlobalLayer {
			for k, val := range v.Parameters[r] {
				merged[k] = val
			}
		}
		out[r] = merged
	}
	return out
}

// calDiff 描述当前校准相对中断点快照在作业范围内的差异。
type calDiff struct {
	// params[resource] 是该资源层发生变化的参数键（新增/删除/值改变）。
	params map[string][]string
	// fid 是读出保真度发生变化的比特。
	fid []int
	// unknown 为 true 表示中断点快照缺失（中断时校准已不可用），无法逐键
	// 比较，只能按“基线未知”保守处理。
	unknown bool
}

func (d calDiff) empty() bool {
	return !d.unknown && len(d.params) == 0 && len(d.fid) == 0
}

// describe 给出稳定、可读的差异描述，用于拒绝原因与决策日志。
func (d calDiff) describe() string {
	if d.unknown {
		return "checkpoint calibration snapshot missing (calibration was unusable at interrupt)"
	}
	parts := make([]string, 0)
	rs := make([]string, 0, len(d.params))
	for r := range d.params {
		rs = append(rs, r)
	}
	sort.Strings(rs)
	for _, r := range rs {
		keys := append([]string(nil), d.params[r]...)
		sort.Strings(keys)
		for _, k := range keys {
			parts = append(parts, r+"/"+k)
		}
	}
	for _, q := range d.fid {
		parts = append(parts, fmt.Sprintf("fid.q%d", q))
	}
	return "effective-value-changed[" + strings.Join(parts, ",") + "]"
}

// diffCalibration 在作业范围内逐键比较检查点快照与当前有效校准。
// 比较的层 = 两份快照出现过的层的并集（始终含全局层 "*"）。
func diffCalibration(cp *Checkpoint, cur currentCalView, scope calScope) calDiff {
	d := calDiff{params: make(map[string][]string)}
	if cp.CalibrationParams == nil {
		d.unknown = true
		return d
	}
	layers := make(map[string]bool, len(scope.resources)+1)
	layers[calibration.GlobalLayer] = true
	for _, r := range scope.resources {
		layers[r] = true
	}
	for r := range cp.CalibrationParams {
		layers[r] = true
	}
	ls := make([]string, 0, len(layers))
	for r := range layers {
		ls = append(ls, r)
	}
	sort.Strings(ls)
	for _, r := range ls {
		old := cp.CalibrationParams[r]
		new := cur.params[r]
		keys := make(map[string]bool, len(old)+len(new))
		for k := range old {
			keys[k] = true
		}
		for k := range new {
			keys[k] = true
		}
		changed := make([]string, 0)
		for k := range keys {
			ov, ook := old[k]
			nv, nok := new[k]
			if ook != nok || ov != nv {
				changed = append(changed, k)
			}
		}
		sort.Strings(changed)
		if len(changed) > 0 {
			d.params[r] = changed
		}
	}
	for _, q := range scope.qubits {
		ov, ook := cp.CalibrationFid[q]
		nv, nok := cur.fid[q]
		if ook != nok || ov != nv {
			d.fid = append(d.fid, q)
		}
	}
	sort.Ints(d.fid)
	return d
}

// affectedGates 返回校准前提被 diff 触及、必须整体重算的门集合。
//
// 规则（与执行核取值方式一一对应）：
//   - 全局层参数键变化（资源 "*"）：所有门的校准前提都可能改变；
//   - 控制资源 qN.ctrl 参数变化：拥有该资源脉冲的门；
//   - 测量资源 measure.bus 参数变化：所有测量门；
//   - 比特 N 的读出保真度变化：作用于比特 N 的测量门。
//
// 只重算被触及的门；其他门的终态结果继续复用，不因“校准变过”就全量重跑。
func affectedGates(s *ir.Schedule, d calDiff) map[string]bool {
	out := make(map[string]bool)
	if s == nil {
		return out
	}
	// 中断点快照缺失（中断时校准不可用）：无法证明任何旧结果的校准前提，
	// 保守地把全部门判为受影响，绝不复用来源不明的旧结果。
	if d.unknown {
		for _, g := range s.GateIDs {
			out[g] = true
		}
		return out
	}
	globalChanged := len(d.params[calibration.GlobalLayer]) > 0
	if globalChanged {
		for _, g := range s.GateIDs {
			out[g] = true
		}
		return out
	}
	fidQ := make(map[int]bool, len(d.fid))
	for _, q := range d.fid {
		fidQ[q] = true
	}
	for _, p := range s.Pulses {
		if keys, ok := d.params[p.Resource]; ok && len(keys) > 0 {
			out[p.GateID] = true
		}
		if p.Kind == ir.PulseMeasure && fidQ[measureQubit(s, p.GateID)] {
			out[p.GateID] = true
		}
	}
	return out
}

// sortedGateNames 返回门集合的有序名，供稳定日志使用。
func sortedGateNames(gates map[string]bool) []string {
	out := make([]string, 0, len(gates))
	for g := range gates {
		out = append(out, g)
	}
	sort.Strings(out)
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
