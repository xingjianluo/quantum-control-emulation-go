// Package simulator 对脉冲计划做确定性噪声仿真，支持顺序/并发两种执行模式，
// 并对取消、超时与部分失败做恰好一次的聚合与因果事件记录。
//
// 并发语义（关键约定）：
//   - 操作必须在所有依赖完成后才可开始；依赖失败的操作不执行，记 skipped_dependency；
//   - 并发模式用 goroutine 执行"等待"，但所有会改变共享状态的动作
//     （理想态演化、测量取值、结论与事件落账）都通过一个提交屏障，
//     严格按 (虚拟结束时刻, OpID) 全局有序提交；顺序模式走完全相同的提交路径，
//     因此两种模式的聚合结果逐字段等价且与调度时序无关地可复现；
//   - 取消/超时不产生测量值；每条操作在结果中恰好出现一次，不重复、不遗漏；
//   - 噪声判定来自 (seed, 校准版本, opID, 用途) 的哈希，无全局随机源。
package simulator

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/xingjianluo/quantum-control-emulation-go/internal/calibration"
	"github.com/xingjianluo/quantum-control-emulation-go/internal/ir"
	"github.com/xingjianluo/quantum-control-emulation-go/internal/scheduler"
)

// Mode 是执行模式。
type Mode string

const (
	ModeSequential Mode = "sequential"
	ModeConcurrent Mode = "concurrent"
)

// Phase 是事件所处的执行阶段。
type Phase string

const (
	PhaseStart   Phase = "start"
	PhaseFinish  Phase = "finish"
	PhaseTimeout Phase = "timeout"
	PhaseCancel  Phase = "cancel"
	PhaseSkip    Phase = "skip"
)

func phaseRank(p Phase) int {
	switch p {
	case PhaseStart:
		return 0
	case PhaseFinish:
		return 1
	case PhaseTimeout, PhaseCancel:
		return 2
	case PhaseSkip:
		return 3
	}
	return 9
}

// Event 是一条带虚拟时刻的因果记录。
type Event struct {
	Seq     int
	Virtual time.Duration
	OpID    string
	Phase   Phase
	Detail  string
}

// OpOutcome 是单条脉冲的执行结果。
type OpOutcome struct {
	OpID       string
	GateID     string
	Start      time.Duration
	Measured   int // 读出结果；非读出操作或失败时为 -1
	OK         bool
	FailReason string
}

// Result 是整批任务的聚合结论。
type Result struct {
	Mode         Mode
	CalibVersion int
	Outcomes     []OpOutcome
	Events       []Event
	Completed    int
	Failed       int
}

// Config 是仿真配置。
type Config struct {
	Mode      Mode
	Seed      int64
	TTL       time.Duration
	OpTimeout time.Duration // 单操作超时上限（虚拟时间规则）；0 表示不限
	CancelAt  time.Duration // >0 时启用虚拟时刻取消：结束时刻 > CancelAt 的在途操作被取消；0 表示不取消
	At        time.Time     // 校准有效性判定时刻；零值取 time.Now()
	Waiter    Waiter
}

// Engine 仿真引擎。
type Engine struct {
	store *calibration.Store
}

// NewEngine 创建引擎。
func NewEngine(store *calibration.Store) *Engine { return &Engine{store: store} }

// commitKind 是工作方向屏障提交的结论类别。
type commitKind int

const (
	ckFinish commitKind = iota // 等待正常结束（是否门噪声错误由 gateBad 决定）
	ckTimeout
	ckCancel
	ckSkip
)

type commit struct {
	op      *ir.PulseOp
	kind    commitKind
	detail  string
	gateBad bool
}

type execState struct {
	mu        sync.Mutex
	cond      *sync.Cond
	noise     *noiseModel
	fidelity  map[int]float64
	ideal     map[int]int
	outcomes  map[string]*OpOutcome
	events    []Event
	seq       int
	done      map[string]bool
	failed    map[string]bool
	pending   map[string]*commit // 已完成等待、等待屏障提交
	committed map[string]bool
}

// Run 执行计划；顺序/并发模式的聚合结论必须等价。
func (e *Engine) Run(ctx context.Context, prog *ir.PulseProgram, cfg Config) (*Result, error) {
	if err := scheduler.New().Validate(prog); err != nil {
		return nil, &Error{Reason: ReasonBadProgram, Detail: err.Error()}
	}
	at := cfg.At
	if at.IsZero() {
		at = time.Now()
	}
	snap, err := e.store.ValidateAt(prog.CalibVersion, at, cfg.TTL)
	if err != nil {
		switch {
		case calibration.IsError(err, calibration.ReasonExpired):
			return nil, &Error{Reason: ReasonCalibExpired, Detail: err.Error()}
		case calibration.IsError(err, calibration.ReasonVersionMismatch):
			return nil, &Error{Reason: ReasonCalibMismatch, Detail: err.Error()}
		default:
			return nil, &Error{Reason: ReasonCalibInvalid, Detail: err.Error()}
		}
	}

	waiter := cfg.Waiter
	if waiter == nil {
		waiter = ImmediateWaiter{}
	}
	mode := cfg.Mode
	if mode == "" {
		mode = ModeSequential
	}

	st := &execState{
		noise:     &noiseModel{seed: cfg.Seed},
		fidelity:  snap.ReadoutFidelity,
		ideal:     map[int]int{},
		outcomes:  map[string]*OpOutcome{},
		done:      map[string]bool{},
		failed:    map[string]bool{},
		pending:   map[string]*commit{},
		committed: map[string]bool{},
	}
	st.cond = sync.NewCond(&st.mu)

	// 全局提交顺序：(虚拟结束时刻, OpID)。理想态演化与测量取值都按此序生效。
	commitOrder := make([]*ir.PulseOp, len(prog.Ops))
	copy(commitOrder, prog.Ops)
	sort.Slice(commitOrder, func(i, j int) bool {
		a, b := commitOrder[i], commitOrder[j]
		if a.End() != b.End() {
			return a.End() < b.End()
		}
		return a.ID < b.ID
	})

	driverDone := make(chan struct{})
	go func() {
		st.driveCommits(commitOrder)
		close(driverDone)
	}()

	if mode == ModeConcurrent {
		var wg sync.WaitGroup
		for _, op := range prog.Ops {
			wg.Add(1)
			go func(op *ir.PulseOp) {
				defer wg.Done()
				executeOp(ctx, op, cfg, waiter, st)
			}(op)
		}
		wg.Wait()
	} else {
		for _, idx := range topoOrder(prog) {
			executeOp(ctx, prog.Ops[idx], cfg, waiter, st)
		}
	}
	<-driverDone
	return assemble(mode, prog, st), nil
}

// driveCommits 按全局提交顺序逐条落账：这是唯一修改共享仿真状态的地方。
func (st *execState) driveCommits(order []*ir.PulseOp) {
	for _, op := range order {
		st.mu.Lock()
		for st.pending[op.ID] == nil {
			st.cond.Wait()
		}
		c := st.pending[op.ID]
		delete(st.pending, op.ID)

		out := &OpOutcome{OpID: op.ID, GateID: op.GateID, Start: op.Start, Measured: -1}
		switch c.kind {
		case ckSkip:
			out.OK = false
			out.FailReason = string(ReasonSkippedDep)
			st.appendEvent(op, PhaseSkip, c.detail, op.End())
			st.finish(op.ID, out, true)
		case ckTimeout, ckCancel:
			out.OK = false
			if c.kind == ckTimeout {
				out.FailReason = string(ReasonTimeout)
				st.appendEvent(op, PhaseTimeout, c.detail, op.End())
			} else {
				out.FailReason = string(ReasonCanceled)
				st.appendEvent(op, PhaseCancel, c.detail, op.End())
			}
			st.finish(op.ID, out, true)
		default: // ckFinish
			if op.Kind == ir.OpReadout {
				q := readoutQubit(op)
				out.Measured = st.noise.readoutBit(op.CalibVersion, op, st.fidelity[q], st.ideal[q])
			} else {
				applyIdeal(op, st.ideal)
			}
			if c.gateBad {
				out.OK = false
				out.FailReason = "gate_error"
				st.appendEvent(op, PhaseFinish, "完成：确定性门噪声判定为错误", op.End())
			} else {
				out.OK = true
				st.appendEvent(op, PhaseFinish, c.detail, op.End())
			}
			st.finish(op.ID, out, c.gateBad)
		}
		st.committed[op.ID] = true
		st.cond.Broadcast()
		st.mu.Unlock()
	}
}

// finish 必须在持锁状态调用：登记结论并唤醒依赖方。
func (st *execState) finish(id string, out *OpOutcome, failed bool) {
	st.outcomes[id] = out
	st.done[id] = true
	st.failed[id] = failed
}

func (st *execState) appendEvent(op *ir.PulseOp, p Phase, detail string, virtual time.Duration) {
	st.seq++
	st.events = append(st.events, Event{Seq: st.seq, Virtual: virtual, OpID: op.ID, Phase: p, Detail: detail})
}

// executeOp 是工作侧逻辑：等依赖 -> 等待执行 -> 投递提交。可被多个 goroutine 并发调用。
func executeOp(ctx context.Context, op *ir.PulseOp, cfg Config, w Waiter, st *execState) {
	// 1) 等所有依赖落账；依赖失败则仅提交 skip，不产生 start 事件、不占用执行。
	st.mu.Lock()
	for {
		allDone, anyFailed := true, false
		for _, dep := range op.Deps {
			if !st.done[dep] {
				allDone = false
			}
			if st.failed[dep] {
				anyFailed = true
			}
		}
		if allDone {
			st.mu.Unlock()
			if anyFailed {
				st.submit(&commit{op: op, kind: ckSkip, detail: "上游依赖失败，因果链跳过，不重复执行"})
				return
			}
			break
		}
		st.cond.Wait()
	}

	// 2) 执行等待。
	// 超时与取消都按虚拟时间规则确定性判定，不使用墙钟 deadline，
	// 避免机器调度抖动引入不可复现；外层 ctx/Waiter 也可触发墙钟取消。
	overTimeout := cfg.OpTimeout > 0 && op.Duration > cfg.OpTimeout
	// 取消点落在半开窗口 (Start, End] 内视为"在途取消"。
	cancelInFlight := cfg.CancelAt > 0 && op.Start < cfg.CancelAt && op.End() >= cfg.CancelAt
	st.mu.Lock()
	st.appendEvent(op, PhaseStart, "开始执行", op.Start)
	st.mu.Unlock()

	if cancelInFlight {
		_ = w.Wait(ctx, op) // 尊重真实等待器；结论由虚拟时间规则确定
		st.submit(&commit{op: op, kind: ckCancel,
			detail: "虚拟取消点 " + cfg.CancelAt.String() + " 落在操作窗口 (Start,End] 内，在途取消"})
		return
	}

	werr := w.Wait(ctx, op)
	if werr != nil || overTimeout {
		kind := ckCancel
		detail := "执行被取消: " + errText(werr)
		if werr == context.DeadlineExceeded || (overTimeout && ctx.Err() == nil) {
			kind = ckTimeout
			detail = "脉冲时长 " + op.Duration.String() + " 超过单操作超时上限 " + cfg.OpTimeout.String()
		}
		st.submit(&commit{op: op, kind: kind, detail: detail})
		return
	}

	// 3) 门噪声是纯哈希判定，与提交顺序无关；测量取值在屏障内按序完成。
	st.submit(&commit{
		op:      op,
		kind:    ckFinish,
		detail:  "执行完成",
		gateBad: st.noise.gateFailed(op.CalibVersion, op),
	})
}

// submit 把结论投递到屏障并阻塞到本操作完成有序提交（保证工作方看到的依赖状态已生效）。
func (st *execState) submit(c *commit) {
	st.mu.Lock()
	st.pending[c.op.ID] = c
	st.cond.Broadcast()
	for !st.committed[c.op.ID] {
		st.cond.Wait()
	}
	st.mu.Unlock()
}

// topoOrder 返回确定性拓扑序（Kahn，候选按 op ID 排序）。
func topoOrder(prog *ir.PulseProgram) []int {
	idx := map[string]int{}
	for i, op := range prog.Ops {
		idx[op.ID] = i
	}
	indeg := make([]int, len(prog.Ops))
	kids := make([][]int, len(prog.Ops))
	for i, op := range prog.Ops {
		for _, d := range op.Deps {
			j := idx[d]
			kids[j] = append(kids[j], i)
			indeg[i]++
		}
	}
	var avail []int
	for i, d := range indeg {
		if d == 0 {
			avail = append(avail, i)
		}
	}
	var order []int
	for len(avail) > 0 {
		sort.Slice(avail, func(a, b int) bool { return prog.Ops[avail[a]].ID < prog.Ops[avail[b]].ID })
		i := avail[0]
		avail = avail[1:]
		order = append(order, i)
		for _, ch := range kids[i] {
			indeg[ch]--
			if indeg[ch] == 0 {
				avail = append(avail, ch)
			}
		}
	}
	return order
}

// assemble 以统一规范生成最终聚合结果，保证两种模式输出结构一致。
func assemble(mode Mode, prog *ir.PulseProgram, st *execState) *Result {
	res := &Result{Mode: mode, CalibVersion: prog.CalibVersion}
	for _, op := range prog.Ops {
		o := st.outcomes[op.ID]
		if o == nil { // 防御：任何情况下都不允许遗漏
			o = &OpOutcome{OpID: op.ID, GateID: op.GateID, Start: op.Start, Measured: -1,
				OK: false, FailReason: string(ReasonCanceled)}
		}
		res.Outcomes = append(res.Outcomes, *o)
		if o.OK {
			res.Completed++
		} else {
			res.Failed++
		}
	}
	evs := make([]Event, len(st.events))
	copy(evs, st.events)
	sort.SliceStable(evs, func(i, j int) bool {
		a, b := evs[i], evs[j]
		if a.Virtual != b.Virtual {
			return a.Virtual < b.Virtual
		}
		if a.OpID != b.OpID {
			return a.OpID < b.OpID
		}
		if phaseRank(a.Phase) != phaseRank(b.Phase) {
			return phaseRank(a.Phase) < phaseRank(b.Phase)
		}
		return a.Seq < b.Seq
	})
	res.Events = evs
	return res
}

// readoutQubit 从资源名 readout-N 还原比特号。
func readoutQubit(op *ir.PulseOp) int {
	return parseTrailingInt(op, ir.ResourceReadout, "readout-")
}

// applyIdeal 用最简经典理想模型更新期望比特，仅用于产生可解释的预期对比：
// X/X90 翻转计算位，RZ 仅改相位（计算位不变），CNOT 在控制位为 1 时翻转目标位。
func applyIdeal(op *ir.PulseOp, ideal map[int]int) {
	if op.Kind == ir.OpReadout {
		return
	}
	if len(op.Resources) >= 2 { // CNOT
		ctl, tgt := cnotQubits(op)
		if ctl >= 0 && tgt >= 0 && ideal[ctl] == 1 {
			ideal[tgt] ^= 1
		}
		return
	}
	if _, isRZ := op.Params["angle"]; isRZ {
		return
	}
	if q := pulseQubit(op); q >= 0 {
		ideal[q] ^= 1
	}
}

func pulseQubit(op *ir.PulseOp) int {
	return parseTrailingInt(op, ir.ResourceQubit, "qubit-")
}

func cnotQubits(op *ir.PulseOp) (int, int) {
	for _, r := range op.Resources {
		if r.Kind == ir.ResourceCoupler {
			name := r.Name[len("coupler-"):]
			var nums []int
			n := 0
			started := false
			flush := func() {
				if started {
					nums = append(nums, n)
					n, started = 0, false
				}
			}
			for i := 0; i < len(name); i++ {
				c := name[i]
				if c >= '0' && c <= '9' {
					n = n*10 + int(c-'0')
					started = true
				} else {
					flush()
				}
			}
			flush()
			if len(nums) == 2 {
				return nums[0], nums[1]
			}
		}
	}
	return -1, -1
}

func parseTrailingInt(op *ir.PulseOp, kind ir.ResourceKind, prefix string) int {
	for _, r := range op.Resources {
		if r.Kind == kind && len(r.Name) > len(prefix) && r.Name[:len(prefix)] == prefix {
			n := 0
			got := false
			for i := len(prefix); i < len(r.Name); i++ {
				c := r.Name[i]
				if c >= '0' && c <= '9' {
					n = n*10 + int(c-'0')
					got = true
				}
			}
			if got {
				return n
			}
		}
	}
	return -1
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
