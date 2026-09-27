package sim

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xingjianluo/quantum-control-emulation-go/internal/calibration"
	"github.com/xingjianluo/quantum-control-emulation-go/internal/ir"
)

// Job 描述一次仿真作业。
type Job struct {
	Schedule       *ir.Schedule
	Shots          int
	Seed           int64
	Now            time.Time
	PerTaskTimeout time.Duration
	// Hooks 按门 ID 注入可控行为：返回 error 模拟脉冲失败；
	// block/delay 由 ctx 感知取消与超时（见 FailHook/TimeoutHook）。
	Hooks map[string]Hook
}

// Hook 允许测试注入可控的脉冲执行行为。
type Hook func(ctx context.Context) error

// Executor 并发/顺序执行脉冲仿真。
type Executor struct {
	store *calibration.Store
	noise NoiseModel
}

// NewExecutor 创建执行器。
func NewExecutor(store *calibration.Store, noise NoiseModel) *Executor {
	return &Executor{store: store, noise: noise}
}

// RunSequential 顺序执行作业：按门拓扑序逐门、逐 shot 执行。
func (e *Executor) RunSequential(ctx context.Context, j Job) *JobResult {
	calID := e.store.CurrentID()
	order := topoOrder(j.Schedule)
	rec := newRecorder()
	per := e.execSequential(ctx, j, order, nil, rec)
	return &JobResult{
		Reports:    aggregateReports(order, per),
		Events:     rec.events(),
		Checkpoint: newCheckpoint(j, calID, order, per),
	}
}

// execSequential 是顺序执行核；reuse 非空时，表中已有的 (门,shot) 结果直接
// 复用并记录 reuse 事件，不再执行。
func (e *Executor) execSequential(ctx context.Context, j Job, order []string, reuse map[string][]shotOutcome, rec *recorder) map[string][]shotOutcome {
	per := make(map[string][]shotOutcome)
	for shot := 0; shot < j.Shots; shot++ {
		for _, gateID := range order {
			if oc, ok := reuseOutcome(reuse, gateID, shot); ok {
				rec.event(gateID, "reuse", fmt.Sprintf("shot=%d status=%s source=checkpoint", shot, oc.status))
				per[gateID] = append(per[gateID], oc)
				continue
			}
			oc := e.runGate(ctx, j, gateID, shot, rec)
			per[gateID] = append(per[gateID], oc)
		}
	}
	return per
}

// RunConcurrent 并发执行作业。
//
// 每个门一个 goroutine，逐 shot 推进；门通过 shot 级屏障等待其所有依赖门的
// 同一 shot 完成后才开始（完成-开始语义，与编译器一致）。每门内部仍逐 shot
// 串行，保证不重复、不遗漏。父 ctx 取消或单门超时只影响未开始/进行中的
// shot，已完成的 shot 结果保留。
func (e *Executor) RunConcurrent(ctx context.Context, j Job) *JobResult {
	calID := e.store.CurrentID()
	order := topoOrder(j.Schedule)
	rec := newRecorder()
	per := e.execConcurrent(ctx, j, order, nil, rec)
	return &JobResult{
		Reports:    aggregateReports(order, per),
		Events:     rec.events(),
		Checkpoint: newCheckpoint(j, calID, order, per),
	}
}

// execConcurrent 是并发执行核；reuse 语义同 execSequential。复用的结果同样
// 写入 done 通道，保证下游屏障与因果事件日志行为一致。
func (e *Executor) execConcurrent(ctx context.Context, j Job, order []string, reuse map[string][]shotOutcome, rec *recorder) map[string][]shotOutcome {
	var cancel context.CancelFunc
	if j.PerTaskTimeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, j.PerTaskTimeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	// signals[gate][shot] 在该门该 shot 完成时关闭其中的 done 通道（广播语义），
	// 结果在关闭前写入 oc。下游据此同时判断“是否完成”与“是否因果可达”：
	// 上游被取消/超时的 shot，后继标记为 cancelled，避免把未执行的工作计入成功。
	// 注意必须是“关闭-广播”而非“发送-接收”：一个门的结果可能同时喂给多个
	// 后继门（fan-out），单接收者的通道会让其余后继永远等不到而卡死。
	signals := make(map[string][]*shotSignal, len(order))
	for _, g := range order {
		signals[g] = make([]*shotSignal, j.Shots)
		for s := 0; s < j.Shots; s++ {
			signals[g][s] = newShotSignal()
		}
	}
	var wg sync.WaitGroup
	per := make(map[string][]shotOutcome, len(order))
	for _, gateID := range order {
		gateID := gateID
		outs := make([]shotOutcome, j.Shots)
		per[gateID] = outs
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 每门独立超时：超时只归因于本门；父 ctx 取消仍级联到所有门。
			var gateCtx context.Context
			var gateCancel context.CancelFunc
			if j.PerTaskTimeout > 0 {
				gateCtx, gateCancel = context.WithTimeout(ctx, j.PerTaskTimeout)
			} else {
				gateCtx, gateCancel = context.WithCancel(ctx)
			}
			defer gateCancel()
			deps := append([]string(nil), j.Schedule.GateDeps[gateID]...)
			sort.Strings(deps)
			for shot := 0; shot < j.Shots; shot++ {
				if oc, ok := reuseOutcome(reuse, gateID, shot); ok {
					rec.event(gateID, "reuse", fmt.Sprintf("shot=%d status=%s source=checkpoint", shot, oc.status))
					outs[shot] = oc
					signals[gateID][shot].complete(oc)
					continue
				}
				ready, abortedDep := waitDeps(ctx, signals, deps, shot)
				if !ready {
					outs[shot] = shotOutcome{status: StatusCancelled}
					rec.event(gateID, "skip", fmt.Sprintf("shot=%d reason=cancelled-or-timeout", shot))
					signals[gateID][shot].complete(outs[shot])
					continue
				}
				if abortedDep != "" {
					outs[shot] = shotOutcome{status: StatusCancelled}
					rec.event(gateID, "skip", fmt.Sprintf("shot=%d reason=upstream-aborted dep=%s", shot, abortedDep))
					signals[gateID][shot].complete(outs[shot])
					continue
				}
				oc := e.runGate(gateCtx, j, gateID, shot, rec)
				outs[shot] = oc
				signals[gateID][shot].complete(oc)
			}
		}()
	}
	wg.Wait()
	return per
}

// reuseOutcome 查询 (门,shot) 是否有可复用的断点结果。
func reuseOutcome(reuse map[string][]shotOutcome, gateID string, shot int) (shotOutcome, bool) {
	if reuse == nil {
		return shotOutcome{}, false
	}
	ocs, ok := reuse[gateID]
	if !ok || shot >= len(ocs) {
		return shotOutcome{}, false
	}
	oc := ocs[shot]
	if !oc.reused {
		return shotOutcome{}, false
	}
	return oc, true
}

// runGate 执行单个门在单个 shot 上的仿真。它是顺序/并发共用的同一执行核，
// 随机数只依赖 (seed, gateID, shot)，因此两种模式结果一致。
func (e *Executor) runGate(ctx context.Context, j Job, gateID string, shot int, rec *recorder) shotOutcome {
	// 先处理 ctx 的取消/超时，避免开始后重复计数。
	if err := ctx.Err(); err != nil {
		st := StatusCancelled
		if err == context.DeadlineExceeded {
			st = StatusTimedOut
		}
		rec.event(gateID, "not-started", fmt.Sprintf("shot=%d status=%s err=%v", shot, st, err))
		return shotOutcome{status: st}
	}
	pulses := gatePulses(j.Schedule, gateID)
	rng := gateStream(j.Seed, gateID, shot)

	// 校准有效性检查：过期/缺失直接失败该 shot，绝不静默沿用旧值。
	for _, p := range pulses {
		qubit := qubitOf(p.Resource)
		if p.Kind == ir.PulseMeasure {
			qubit = measureQubit(j.Schedule, gateID)
		}
		params, _, cerr := e.store.CurrentFor(p.Resource, qubit, j.Now)
		if cerr != nil {
			rec.event(gateID, "calibration-halt", fmt.Sprintf("shot=%d pulse=%s err=%v", shot, p.ID, cerr))
			return shotOutcome{status: StatusFailed, bit: -1}
		}
		amp := p.Params["amp"]
		if eff, ok := params["amp"]; ok {
			amp = eff
		}
		scale := 1.0
		if s, ok := e.noise.AmpScale[p.Resource]; ok {
			scale = s
		}
		if driven := amp * scale; driven > e.noise.MaxAmp || driven <= 0 {
			rec.event(gateID, "illegal-drive", fmt.Sprintf("shot=%d pulse=%s driven=%.4f max=%.4f", shot, p.ID, driven, e.noise.MaxAmp))
			return shotOutcome{status: StatusFailed}
		}
	}

	// Hook 注入（测试取消/超时/脉冲失败）。Hook 按门执行一次，代表该 shot
	// 的外部执行环境，遵守 ctx。
	if hook, ok := j.Hooks[gateID]; ok {
		if err := hook(ctx); err != nil {
			st := classifyHookErr(ctx, err)
			rec.event(gateID, "hook", fmt.Sprintf("shot=%d status=%s err=%v", shot, st, err))
			return shotOutcome{status: st}
		}
	}

	// 逐脉冲确定性噪声：去极化失败概率按资源抽取。
	for pi, p := range pulses {
		if p.Kind != ir.PulseControl {
			continue
		}
		failP := 0.0
		if fp, ok := e.noise.DepolPerPulse[p.Resource]; ok {
			failP = fp
		}
		draw := rng.Float64()
		if draw < failP {
			rec.event(gateID, "pulse-fail", fmt.Sprintf("shot=%d pulse=%s idx=%d draw=%.4f<p=%.4f", shot, p.ID, pi, draw, failP))
			return shotOutcome{status: StatusFailed}
		}
	}

	// 测量门：读出比特由“理想比特 1 + 读出翻转概率(1-fidelity)”确定。
	if len(pulses) == 1 && pulses[0].Kind == ir.PulseMeasure {
		_, fid, cerr := e.store.CurrentFor(pulses[0].Resource, measureQubit(j.Schedule, gateID), j.Now)
		if cerr != nil {
			rec.event(gateID, "calibration-halt", fmt.Sprintf("shot=%d readout err=%v", shot, cerr))
			return shotOutcome{status: StatusFailed}
		}
		draw := rng.Float64()
		bit := 1
		if draw > fid {
			bit = 0
		}
		rec.event(gateID, "measure", fmt.Sprintf("shot=%d draw=%.4f fid=%.4f bit=%d", shot, draw, fid, bit))
		return shotOutcome{status: StatusMeasured, bit: bit}
	}

	rec.event(gateID, "complete", fmt.Sprintf("shot=%d pulses=%d", shot, len(pulses)))
	return shotOutcome{status: StatusOK}
}

// ---- 辅助 ----

// shotSignal 是 (门, shot) 的完成信号：complete 先写结果再关闭通道，
// 任意数量的等待者都能收到广播（close 的 happens-before 保证读到 oc）。
type shotSignal struct {
	oc   shotOutcome
	done chan struct{}
}

func newShotSignal() *shotSignal { return &shotSignal{done: make(chan struct{})} }

// complete 记录结果并广播完成。每个信号只允许 complete 一次。
func (s *shotSignal) complete(oc shotOutcome) {
	s.oc = oc
	close(s.done)
}

// waitDeps 等待某 shot 的所有依赖门完成。
//
// 返回值：
//   - ready=false: 父 ctx 被取消/超时，未能确认全部依赖完成；
//   - abortedDep!="": 依赖已完成但为 cancelled/timed-out，本 shot 因果不可达。
//
// 先用非阻塞 select 判定已就绪的依赖信号，避免“依赖已完成且 ctx 同时取消”
// 时 Go 的 select 随机选中 ctx 分支而错杀本可执行的 shot。
func waitDeps(ctx context.Context, signals map[string][]*shotSignal, deps []string, shot int) (ready bool, abortedDep string) {
	for _, d := range deps {
		sigs, ok := signals[d]
		if !ok || shot >= len(sigs) {
			return false, ""
		}
		sig := sigs[shot]
		select {
		case <-sig.done:
		default:
			select {
			case <-sig.done:
			case <-ctx.Done():
				return false, ""
			}
		}
		if sig.oc.status == StatusCancelled || sig.oc.status == StatusTimedOut {
			return true, d
		}
	}
	return true, ""
}

func classifyHookErr(ctx context.Context, err error) Status {
	if ctx.Err() == context.DeadlineExceeded {
		return StatusTimedOut
	}
	if ctx.Err() == context.Canceled {
		return StatusCancelled
	}
	return StatusFailed
}

func gatePulses(s *ir.Schedule, gateID string) []ir.Pulse {
	out := make([]ir.Pulse, 0)
	for _, p := range s.Pulses {
		if p.GateID == gateID {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// measureQubit 通过脉冲所属门在 Schedule 中查不到 GateTask（IR 不含 Qubits），
// 约定测量资源键不含比特；测量比特由作业通过 Params["qubit"] 携带（编译器在
// 展开测量脉冲时注入）。
func measureQubit(s *ir.Schedule, gateID string) int {
	for _, p := range s.Pulses {
		if p.GateID == gateID && p.Kind == ir.PulseMeasure {
			if q, ok := p.Params["qubit"]; ok {
				return int(q)
			}
			return 0
		}
	}
	return 0
}

// qubitOf 从控制资源键 qN.ctrl 解析比特号；测量总线返回 0（其比特以 Params 为准）。
func qubitOf(resource string) int {
	var q int
	if _, err := fmt.Sscanf(resource, "q%d.ctrl", &q); err == nil {
		return q
	}
	return 0
}

// topoOrder 返回门的确定性拓扑顺序（Kahn + GateIDs 输入序破紧）。
func topoOrder(s *ir.Schedule) []string {
	if s == nil {
		return nil
	}
	pred := make(map[string][]string)
	indeg := make(map[string]int)
	for _, id := range s.GateIDs {
		indeg[id] = 0
	}
	for g, ds := range s.GateDeps {
		indeg[g] = len(ds)
		for _, d := range ds {
			pred[d] = append(pred[d], g)
		}
	}
	ready := make([]string, 0)
	for _, id := range s.GateIDs {
		if indeg[id] == 0 {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)
	out := make([]string, 0, len(s.GateIDs))
	for len(ready) > 0 {
		g := ready[0]
		ready = ready[1:]
		out = append(out, g)
		ns := append([]string(nil), pred[g]...)
		sort.Strings(ns)
		for _, n := range ns {
			indeg[n]--
			if indeg[n] == 0 {
				ready = insertSortedStr(ready, n)
			}
		}
	}
	return out
}

func insertSortedStr(xs []string, x string) []string {
	i := sort.SearchStrings(xs, x)
	xs = append(xs, "")
	copy(xs[i+1:], xs[i:])
	xs[i] = x
	return xs
}

// recorder 是线程安全的全局因果事件记录器：Seq 单调递增，反映事件实际发生
// 的全局先后；goroutine 并发时交错顺序不确定，但同一执行内的因果先后
// （依赖门的完成先于后继门开始）得到保留。
type recorder struct {
	mu  sync.Mutex
	ev  []Event
	seq atomic.Int64
}

func newRecorder() *recorder { return &recorder{} }

func (r *recorder) event(gate, kind, note string) {
	r.mu.Lock()
	r.ev = append(r.ev, Event{Seq: int(r.seq.Add(1)) - 1, GateID: gate, Kind: kind, Note: note})
	r.mu.Unlock()
}

func (r *recorder) events() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]Event(nil), r.ev...)
	return out
}
