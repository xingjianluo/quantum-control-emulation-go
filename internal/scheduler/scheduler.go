// Package scheduler 对脉冲计划做依赖校验、资源冲突检测与确定性 ASAP 时序排布。
//
// 排程模型：
//   - 每条操作声明占用若干资源与半开时间窗口 [Start, Start+Duration)；
//   - 依赖必须严格先完成（dep.End <= op.Start），否则拒绝；
//   - 同资源上的窗口首尾相接允许（[a,b) 与 [b,c) 不冲突），任何重叠拒绝；
//   - 就绪选择顺序为 (maxDepEnd, 稳定ID)，ID 采用 index#id 形式，
//     保证相同输入永远得到相同计划，且在满足依赖前提下尽早压实空闲。
package scheduler

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/xingjianluo/quantum-control-emulation-go/internal/ir"
)

// Decision 记录一次调度决策，便于单测打印"调度决策"依据。
type Decision struct {
	OpID  string
	Start time.Duration
	Why   string
}

// window 是某资源上的一个半开占用区间。
type window struct{ start, end time.Duration }

// Scheduler 排布脉冲计划。
type Scheduler struct {
	lastDecisions []Decision
}

// New 创建调度器。
func New() *Scheduler { return &Scheduler{} }

// Decisions 返回上一次 Assign/Replan 的决策日志副本。
func (s *Scheduler) Decisions() []Decision {
	out := make([]Decision, len(s.lastDecisions))
	copy(out, s.lastDecisions)
	return out
}

// Assign 为未排时（Start 全为 0）的计划计算确定性 ASAP 开始时间，就地写入。
func (s *Scheduler) Assign(prog *ir.PulseProgram) error {
	index, order, err := s.prepare(prog.Ops)
	if err != nil {
		return err
	}

	start := make([]time.Duration, len(prog.Ops))
	// 每个资源维护已放置窗口；槽位计算统一排序，结果确定。
	resWindows := map[string][]window{}
	var decisions []Decision

	placed := make([]bool, len(prog.Ops))
	for remaining := len(order); remaining > 0; {
		// 在每轮选择"可放置且键序最小"的操作：
		// 键为 (所有依赖的最大结束时间, 稳定ID)。
		best := -1
		var bestEarliest time.Duration
		for _, idx := range order {
			if placed[idx] {
				continue
			}
			ready := true
			var earliest time.Duration
			for _, dep := range prog.Ops[idx].Deps {
				d := index[dep]
				if !placed[d] {
					ready = false
					break
				}
				if e := start[d] + prog.Ops[d].Duration; e > earliest {
					earliest = e
				}
			}
			if !ready {
				continue
			}
			if best == -1 || pickLess(earliest, stableKey(idx, prog.Ops[idx]), bestEarliest, stableKey(best, prog.Ops[best])) {
				best, bestEarliest = idx, earliest
			}
		}
		if best == -1 {
			// 理论上 prepare 已做拓扑校验，此处为防御性分支。
			return &Error{Reason: ReasonCyclicDeps, Detail: "调度期检测到不可满足依赖"}
		}
		op := prog.Ops[best]
		t := earliestAfterWindows(bestEarliest, op.Duration, op.Resources, resWindows)
		var blockers []string
		for _, r := range op.Resources {
			resWindows[r.Name] = append(resWindows[r.Name], window{start: t, end: t + op.Duration})
			blockers = append(blockers, r.Name)
		}
		start[best] = t
		placed[best] = true
		remaining--
		decisions = append(decisions, Decision{
			OpID: op.ID, Start: t,
			Why: fmt.Sprintf("earliest=%s resources=[%s]", bestEarliest, strings.Join(blockers, ",")),
		})
	}
	for i, op := range prog.Ops {
		op.Start = start[i]
	}
	s.lastDecisions = decisions
	return nil
}

// earliestAfterWindows 在所有涉及资源的已放置窗口中，找到 >= lowerBound 的最早可容纳
// duration 的槽位（窗口半开，首尾相接不冲突）。
func earliestAfterWindows(lowerBound, duration time.Duration, resources []ir.Resource, resWindows map[string][]window) time.Duration {
	var all []window
	for _, r := range resources {
		all = append(all, resWindows[r.Name]...)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].start != all[j].start {
			return all[i].start < all[j].start
		}
		return all[i].end < all[j].end
	})
	t := lowerBound
	for _, w := range all {
		if w.end <= t {
			continue // 窗口完全在候选点之前
		}
		if w.start >= t+duration {
			break // 后续窗口都在本槽位之后，当前 t 可用
		}
		t = w.end // 与窗口重叠，推进到窗口末尾
	}
	return t
}

func pickLess(earliest time.Duration, key string, earliest2 time.Duration, key2 string) bool {
	if earliest != earliest2 {
		return earliest < earliest2
	}
	return key < key2
}

// stableKey 用输入下标 + ID 构成稳定排序键，避免重复 ID（已被拒绝）之外的不可预期。
func stableKey(idx int, op *ir.PulseOp) string {
	return fmt.Sprintf("%06d#%s", idx, op.ID)
}

// Validate 校验一份已带时序的计划，失败原因可区分。
func (s *Scheduler) Validate(prog *ir.PulseProgram) error {
	if _, _, err := s.prepare(prog.Ops); err != nil {
		return err
	}
	index := map[string]int{}
	for i, op := range prog.Ops {
		if op.Start < 0 {
			return &Error{Reason: ReasonInvalidTime, OpID: op.ID, Detail: "开始时间为负"}
		}
		if op.Duration < 0 {
			return &Error{Reason: ReasonInvalidTime, OpID: op.ID, Detail: "时长为负"}
		}
		index[op.ID] = i
	}
	// 依赖时序
	for _, op := range prog.Ops {
		for _, dep := range op.Deps {
			d := prog.Ops[index[dep]]
			if d.Start+d.Duration > op.Start {
				return &Error{Reason: ReasonDepOrder, OpID: op.ID, Detail: "依赖 " + dep + " 结束于 " + (d.Start + d.Duration).String() +
					"，晚于本操作开始 " + op.Start.String()}
			}
		}
	}
	// 资源重叠
	type usage struct {
		start, end time.Duration
		op         string
	}
	res := map[string][]usage{}
	for _, op := range prog.Ops {
		u := usage{start: op.Start, end: op.End(), op: op.ID}
		for _, r := range op.Resources {
			res[r.Name] = append(res[r.Name], u)
		}
	}
	names := make([]string, 0, len(res))
	for n := range res {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		us := res[n]
		sort.Slice(us, func(i, j int) bool {
			if us[i].start != us[j].start {
				return us[i].start < us[j].start
			}
			return us[i].op < us[j].op
		})
		for i := 1; i < len(us); i++ {
			if us[i].start < us[i-1].end {
				return &Error{Reason: ReasonResourceBusy, OpID: us[i].op, Resource: n,
					Detail: fmt.Sprintf("与操作 %s 的占用窗口 [%s,%s) 重叠",
						us[i-1].op, us[i-1].start, us[i-1].end)}
			}
		}
	}
	return nil
}

// Replan 在不改变语义的前提下重新 ASAP 压实时序，返回新计划。
func (s *Scheduler) Replan(prog *ir.PulseProgram) (*ir.PulseProgram, error) {
	np := deepCopy(prog)
	if err := s.Assign(np); err != nil {
		return nil, err
	}
	return np, nil
}

// TotalIdle 统计计划内所有资源（makespan×占用数 - 忙碌时长）的空闲总量。
func TotalIdle(prog *ir.PulseProgram) time.Duration {
	if len(prog.Ops) == 0 {
		return 0
	}
	mk := prog.Makespan()
	busy := map[string]time.Duration{}
	used := map[string]bool{}
	for _, op := range prog.Ops {
		for _, r := range op.Resources {
			busy[r.Name] += op.Duration
			used[r.Name] = true
		}
	}
	var idle time.Duration
	for n := range used {
		idle += mk - busy[n]
	}
	return idle
}

// prepare 做身份、依赖与成环校验，返回索引和一个确定性拓扑顺序（按稳定键）。
func (s *Scheduler) prepare(ops []*ir.PulseOp) (map[string]int, []int, error) {
	index := map[string]int{}
	for i, op := range ops {
		if op.ID == "" {
			return nil, nil, &Error{Reason: ReasonInvalidTime, Detail: "脉冲操作缺少 ID"}
		}
		if op.Duration < 0 {
			return nil, nil, &Error{Reason: ReasonInvalidTime, OpID: op.ID, Detail: "时长为负"}
		}
		if _, dup := index[op.ID]; dup {
			return nil, nil, &Error{Reason: ReasonDuplicateOp, OpID: op.ID, Detail: "脉冲操作 ID 重复"}
		}
		index[op.ID] = i
	}
	indeg := make([]int, len(ops))
	children := make([][]int, len(ops))
	for i, op := range ops {
		seenDep := map[string]bool{}
		for _, dep := range op.Deps {
			if seenDep[dep] {
				continue
			}
			seenDep[dep] = true
			j, ok := index[dep]
			if !ok {
				return nil, nil, &Error{Reason: ReasonMissingDep, OpID: op.ID, Detail: "依赖的脉冲不存在: " + dep}
			}
			if j == i {
				return nil, nil, &Error{Reason: ReasonSelfDep, OpID: op.ID, Detail: "操作不能依赖自身"}
			}
			children[j] = append(children[j], i)
			indeg[i]++
		}
	}
	// Kahn，候选集合内按稳定键取最小，保证确定性。
	var order []int
	avail := newMinSet(ops)
	for i, d := range indeg {
		if d == 0 {
			avail.push(i)
		}
	}
	for avail.len() > 0 {
		i := avail.pop()
		order = append(order, i)
		for _, ch := range children[i] {
			indeg[ch]--
			if indeg[ch] == 0 {
				avail.push(ch)
			}
		}
	}
	if len(order) != len(ops) {
		var cyc []string
		for i, d := range indeg {
			if d > 0 {
				cyc = append(cyc, ops[i].ID)
			}
		}
		sort.Strings(cyc)
		return nil, nil, &Error{Reason: ReasonCyclicDeps, Detail: "依赖图成环，涉及: " + strings.Join(cyc, ",")}
	}
	return index, order, nil
}

type minSet struct {
	ops []*ir.PulseOp
	set map[int]bool
}

func newMinSet(ops []*ir.PulseOp) *minSet { return &minSet{ops: ops, set: map[int]bool{}} }
func (m *minSet) push(i int)              { m.set[i] = true }
func (m *minSet) len() int                { return len(m.set) }
func (m *minSet) pop() int {
	best := -1
	for i := range m.set {
		if best == -1 || stableKey(i, m.ops[i]) < stableKey(best, m.ops[best]) {
			best = i
		}
	}
	delete(m.set, best)
	return best
}

func deepCopy(p *ir.PulseProgram) *ir.PulseProgram {
	np := &ir.PulseProgram{CalibVersion: p.CalibVersion, Ops: make([]*ir.PulseOp, len(p.Ops))}
	for i, op := range p.Ops {
		cp := *op
		cp.Deps = append([]string(nil), op.Deps...)
		res := make([]ir.Resource, len(op.Resources))
		copy(res, op.Resources)
		cp.Resources = res
		params := make(map[string]float64, len(op.Params))
		for k, v := range op.Params {
			params[k] = v
		}
		cp.Params = params
		np.Ops[i] = &cp
	}
	return np
}
