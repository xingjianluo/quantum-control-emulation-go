package compile

import (
	"sort"

	"github.com/xingjianluo/quantum-control-emulation-go/internal/ir"
)

// Compiler 把门任务编译为脉冲计划。
//
// 调度模型（ASAP 列表调度）：
//   - 门之间存在由 DependsOn 给出的偏序；门的首个脉冲不得早于任一前驱门的
//     最后一个脉冲结束（完成-开始语义）。
//   - 同一资源上的脉冲互斥，端点相接([a,b) 与 [b,c))允许。
//   - 就绪集合按“输入下标”做确定性破紧，相同输入永远产出相同计划。
//   - 门内脉冲可以按各自资源的最早空闲槽并行放置（例如 CNOT 同时占用两端）。
type Compiler struct {
	protocol *Protocol
}

// NewCompiler 创建编译器并校验硬件协议。
func NewCompiler(p *Protocol) (*Compiler, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &Compiler{protocol: p}, nil
}

// Compile 编译门任务序列为合法计划。
//
// 返回的 *Error 可通过 Reason 区分：非法参数 / 依赖不可满足。
// 自由编译下资源冲突不可能出现（调度器总是绕开占用），冲突只会在
// BindFixed 校验用户显式时序时出现。
func (c *Compiler) Compile(gates []ir.GateTask) (*ir.Schedule, error) {
	order, deps, err := c.validate(gates)
	if err != nil {
		return nil, err
	}
	built, err := c.expand(gates)
	if err != nil {
		return nil, err
	}
	pulses := c.scheduleASAP(gates, order, deps, built)
	return c.assemble(gates, deps, pulses), nil
}

// BindFixed 校验用户显式给定的时序绑定（脉冲必须已带 StartNS）。
//
// 拒绝原因：
//   - ReasonIllegalParam:     脉冲集合与门规格不符 / 参数越界。
//   - ReasonConflict:        同资源时间重叠（即使两门有依赖，重叠也非法，
//     因为依赖是完成-开始关系）。
//   - ReasonUnsatisfiableDep:给定时间违反完成-开始的依赖契约。
func (c *Compiler) BindFixed(gates []ir.GateTask, pulses []ir.Pulse) (*ir.Schedule, error) {
	_, deps, err := c.validate(gates)
	if err != nil {
		return nil, err
	}
	built, err := c.expand(gates)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]ir.Pulse, len(pulses))
	for _, p := range pulses {
		if _, ok := byID[p.ID]; ok {
			return nil, newError(ReasonIllegalParam, "duplicate pulse id %q", p.ID)
		}
		byID[p.ID] = p
	}
	// 1) 脉冲集合必须与规格展开结果完全一致（时序字段除外）。
	for id, want := range built {
		got, ok := byID[id]
		if !ok {
			return nil, newError(ReasonIllegalParam, "pulse %q required by gate spec is missing", id)
		}
		if !samePulseShape(want, got) {
			return nil, newError(ReasonIllegalParam, "pulse %q shape mismatch: want resource=%s dur=%d params=%v, got resource=%s dur=%d params=%v",
				id, want.Resource, want.DurationNS, want.Params, got.Resource, got.DurationNS, got.Params)
		}
		if got.StartNS < 0 {
			return nil, newError(ReasonIllegalParam, "pulse %q has negative start %d", id, got.StartNS)
		}
	}
	if len(byID) != len(built) {
		return nil, newError(ReasonIllegalParam, "got %d pulses but spec expands to %d", len(byID), len(built))
	}
	// 2) 任意同资源重叠都是冲突。
	for i := 0; i < len(pulses); i++ {
		for j := i + 1; j < len(pulses); j++ {
			if pulses[i].Overlaps(pulses[j]) {
				return nil, newError(ReasonConflict, "pulses %q (gate %s, [%d,%d)) and %q (gate %s, [%d,%d)) overlap on resource %q",
					pulses[i].ID, pulses[i].GateID, pulses[i].StartNS, pulses[i].EndNS(),
					pulses[j].ID, pulses[j].GateID, pulses[j].StartNS, pulses[j].EndNS(),
					pulses[i].Resource)
			}
		}
	}
	// 3) 完成-开始依赖契约：前驱门最晚结束 <= 后继门最早开始。
	earliest := make(map[string]int64)
	latest := make(map[string]int64)
	for _, p := range pulses {
		if s, ok := earliest[p.GateID]; !ok || p.StartNS < s {
			earliest[p.GateID] = p.StartNS
		}
		if p.EndNS() > latest[p.GateID] {
			latest[p.GateID] = p.EndNS()
		}
	}
	for _, g := range gates {
		for _, dep := range deps[g.ID] {
			if latest[dep] > earliest[g.ID] {
				return nil, newError(ReasonUnsatisfiableDep, "gate %q starts at %d before dependency %q ends at %d",
					g.ID, earliest[g.ID], dep, latest[dep])
			}
		}
	}
	sched := c.assemble(gates, deps, sortedPulses(pulses))
	return sched, nil
}

// Reorder 对已有计划做保语义重排，使用与 Compile 相同的确定性 ASAP 调度。
//
// 重排只重置 StartNS，门集合、脉冲形状(资源/时长/参数)与依赖边均保持不变，
// 因而不改变语义；ASAP 会压缩资源上的空隙以减少总空闲等待。
// 相同输入计划在任何机器上重排结果都一致。
func Reorder(s *ir.Schedule) *ir.Schedule {
	if s == nil {
		return nil
	}
	// 重建门的拓扑顺序：以 GateIDs 作为确定性破紧次序做 Kahn 排序。
	pred := make(map[string][]string, len(s.GateIDs))
	indeg := make(map[string]int, len(s.GateIDs))
	for g, ds := range s.GateDeps {
		for _, d := range ds {
			pred[d] = append(pred[d], g)
			indeg[g]++
		}
	}
	ready := make([]string, 0)
	for _, id := range s.GateIDs {
		if indeg[id] == 0 {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)
	order := make([]string, 0, len(s.GateIDs))
	for len(ready) > 0 {
		g := ready[0]
		ready = ready[1:]
		order = append(order, g)
		ns := append([]string(nil), pred[g]...)
		sort.Strings(ns)
		for _, n := range ns {
			indeg[n]--
			if indeg[n] == 0 {
				ready = insertSorted(ready, n)
			}
		}
	}

	// 按门收集脉冲（保持计划内脉冲出现顺序，保证门内布局确定）。
	gatePulses := make(map[string][]ir.Pulse)
	for _, p := range s.Pulses {
		gatePulses[p.GateID] = append(gatePulses[p.GateID], p)
	}
	busy := make(map[string][]ir.Pulse)
	gateEnd := make(map[string]int64)
	out := make([]ir.Pulse, 0, len(s.Pulses))
	for _, g := range order {
		var lower int64
		for _, d := range s.GateDeps[g] {
			if gateEnd[d] > lower {
				lower = gateEnd[d]
			}
		}
		placed := make([]ir.Pulse, 0, len(gatePulses[g]))
		localResEnd := make(map[string]int64)
		for _, p := range gatePulses[g] {
			lb := lower
			if e := localResEnd[p.Resource]; e > lb {
				lb = e
			}
			start := earliestSlot(busy[p.Resource], lb, p.DurationNS)
			p.StartNS = start
			busy[p.Resource] = insertInterval(busy[p.Resource], p)
			localResEnd[p.Resource] = p.EndNS()
			if p.EndNS() > gateEnd[g] {
				gateEnd[g] = p.EndNS()
			}
			placed = append(placed, p)
		}
		out = append(out, placed...)
	}
	return &ir.Schedule{
		GateIDs:  append([]string(nil), s.GateIDs...),
		Pulses:   out,
		GateDeps: cloneDeps(s.GateDeps),
	}
}

// SameSemantics 判断两个计划是否语义等价：
// 门集合、依赖边集合、每个门脉冲的形状(资源/时长/参数，顺序无关)一致。
// 时序差异不影响语义判定。
func SameSemantics(a, b *ir.Schedule) bool {
	if a == nil || b == nil {
		return a == b
	}
	if len(a.GateIDs) != len(b.GateIDs) || len(a.Pulses) != len(b.Pulses) {
		return false
	}
	as := append([]string(nil), a.GateIDs...)
	bs := append([]string(nil), b.GateIDs...)
	sort.Strings(as)
	sort.Strings(bs)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	if !sameDeps(a.GateDeps, b.GateDeps) {
		return false
	}
	shapeKey := func(p ir.Pulse) string {
		keys := make([]string, 0, len(p.Params))
		for k := range p.Params {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		s := p.ID + "|" + p.Resource + "|" + p.Kind.String()
		for _, k := range keys {
			s += "|" + k + "=" + formatFloat(p.Params[k])
		}
		return s
	}
	counts := func(sc *ir.Schedule) map[string]map[string]int {
		m := make(map[string]map[string]int)
		for _, p := range sc.Pulses {
			if m[p.GateID] == nil {
				m[p.GateID] = make(map[string]int)
			}
			m[p.GateID][shapeKey(p)]++
		}
		return m
	}
	ca, cb := counts(a), counts(b)
	for g, ma := range ca {
		mb := cb[g]
		if len(ma) != len(mb) {
			return false
		}
		for k, v := range ma {
			if mb[k] != v {
				return false
			}
		}
	}
	return true
}

// ---- 内部实现 ----

func (c *Compiler) validate(gates []ir.GateTask) ([]string, map[string][]string, error) {
	index := make(map[string]int, len(gates))
	for i, g := range gates {
		if g.ID == "" {
			return nil, nil, newError(ReasonIllegalParam, "gate at index %d has empty id", i)
		}
		if j, ok := index[g.ID]; ok {
			return nil, nil, newError(ReasonIllegalParam, "duplicate gate id %q at indices %d and %d", g.ID, j, i)
		}
		index[g.ID] = i
		spec, ok := c.protocol.GateSpecs[g.Kind]
		if !ok {
			return nil, nil, newError(ReasonIllegalParam, "gate %q uses unsupported kind %s", g.ID, g.Kind)
		}
		if len(g.Qubits) != spec.Arity {
			return nil, nil, newError(ReasonIllegalParam, "gate %q(%s) needs %d qubits, got %d", g.ID, g.Kind, spec.Arity, len(g.Qubits))
		}
		seen := make(map[int]bool)
		for _, q := range g.Qubits {
			if q < 0 || q >= c.protocol.Qubits {
				return nil, nil, newError(ReasonIllegalParam, "gate %q references qubit %d outside [0,%d)", g.ID, q, c.protocol.Qubits)
			}
			if seen[q] {
				return nil, nil, newError(ReasonIllegalParam, "gate %q references qubit %d more than once", g.ID, q)
			}
			seen[q] = true
		}
	}
	deps := make(map[string][]string, len(gates))
	indeg := make(map[string]int, len(gates))
	for _, g := range gates {
		seen := make(map[string]bool)
		for _, d := range g.DependsOn {
			if d == g.ID {
				return nil, nil, newError(ReasonUnsatisfiableDep, "gate %q depends on itself", g.ID)
			}
			if _, ok := index[d]; !ok {
				return nil, nil, newError(ReasonUnsatisfiableDep, "gate %q depends on unknown gate %q", g.ID, d)
			}
			if seen[d] {
				continue
			}
			seen[d] = true
			deps[g.ID] = append(deps[g.ID], d)
			indeg[g.ID]++
		}
	}
	// Kahn 拓扑排序，就绪集合按输入下标破紧。
	ready := make([]int, 0)
	for _, g := range gates {
		if indeg[g.ID] == 0 {
			ready = append(ready, index[g.ID])
		}
	}
	sort.Ints(ready)
	order := make([]string, 0, len(gates))
	for len(ready) > 0 {
		i := ready[0]
		ready = ready[1:]
		g := gates[i]
		order = append(order, g.ID)
		// 找出所有以 g 为前驱的门，确定性地遍历。
		for j, h := range gates {
			if !contains(deps[h.ID], g.ID) {
				continue
			}
			indeg[h.ID]--
			if indeg[h.ID] == 0 {
				ready = insertIndexSorted(ready, j)
			}
		}
	}
	if len(order) != len(gates) {
		return nil, nil, newError(ReasonUnsatisfiableDep, "dependency cycle detected among %d gates", len(gates)-len(order))
	}
	return order, deps, nil
}

// expand 按门规格展开脉冲，返回以脉冲 ID 为键的表，并做参数越界检查。
func (c *Compiler) expand(gates []ir.GateTask) (map[string]ir.Pulse, error) {
	out := make(map[string]ir.Pulse)
	for _, g := range gates {
		spec := c.protocol.GateSpecs[g.Kind]
		ps := spec.Build(g)
		if len(ps) == 0 {
			return nil, newError(ReasonIllegalParam, "gate %q(%s) expands to no pulses", g.ID, g.Kind)
		}
		for _, p := range ps {
			if p.ID == "" {
				return nil, newError(ReasonIllegalParam, "gate %q produced pulse with empty id", g.ID)
			}
			if _, dup := out[p.ID]; dup {
				return nil, newError(ReasonIllegalParam, "duplicate pulse id %q from gate %q", p.ID, g.ID)
			}
			if p.GateID != g.ID {
				return nil, newError(ReasonIllegalParam, "pulse %q gate ownership %q != %q", p.ID, p.GateID, g.ID)
			}
			p.DurationNS = spec.DurationNS
			if p.Params["amp"] <= 0 || p.Params["amp"] > 1.0 {
				return nil, newError(ReasonIllegalParam, "pulse %q amp %v out of (0,1]", p.ID, p.Params["amp"])
			}
			out[p.ID] = p
		}
	}
	return out, nil
}

func (c *Compiler) scheduleASAP(gates []ir.GateTask, order []string, deps map[string][]string, built map[string]ir.Pulse) []ir.Pulse {
	byGate := make(map[string][]ir.Pulse)
	for _, g := range gates {
		// gates 输入顺序决定门内脉冲顺序。
		for _, p := range built {
			if p.GateID == g.ID {
				byGate[g.ID] = append(byGate[g.ID], p)
			}
		}
		sort.Slice(byGate[g.ID], func(i, j int) bool { return byGate[g.ID][i].ID < byGate[g.ID][j].ID })
	}
	busy := make(map[string][]ir.Pulse)
	gateEnd := make(map[string]int64)
	out := make([]ir.Pulse, 0, len(built))
	for _, gID := range order {
		var lower int64
		for _, d := range deps[gID] {
			if gateEnd[d] > lower {
				lower = gateEnd[d]
			}
		}
		localResEnd := make(map[string]int64)
		for _, p := range byGate[gID] {
			lb := lower
			if e := localResEnd[p.Resource]; e > lb {
				lb = e
			}
			p.StartNS = earliestSlot(busy[p.Resource], lb, p.DurationNS)
			busy[p.Resource] = insertInterval(busy[p.Resource], p)
			localResEnd[p.Resource] = p.EndNS()
			if p.EndNS() > gateEnd[gID] {
				gateEnd[gID] = p.EndNS()
			}
			out = append(out, p)
		}
	}
	return sortedPulses(out)
}

func (c *Compiler) assemble(gates []ir.GateTask, deps map[string][]string, pulses []ir.Pulse) *ir.Schedule {
	s := &ir.Schedule{
		GateIDs:  make([]string, len(gates)),
		Pulses:   pulses,
		GateDeps: cloneDeps(deps),
	}
	for i, g := range gates {
		s.GateIDs[i] = g.ID
		ids := make([]string, 0)
		for _, p := range pulses {
			if p.GateID == g.ID {
				ids = append(ids, p.ID)
			}
		}
		s.SetGatePulseIDs(g.ID, ids)
	}
	return s
}

// earliestSlot 在某资源已按起始时间排序的占用区间中，找 >= lower 的最早可容纳时长 dur 的槽位。
func earliestSlot(occupied []ir.Pulse, lower int64, dur int64) int64 {
	t := lower
	for _, p := range occupied {
		if p.EndNS() <= t {
			continue
		}
		if p.StartNS >= t+dur {
			return t
		}
		t = p.EndNS()
	}
	return t
}

func insertInterval(intervals []ir.Pulse, p ir.Pulse) []ir.Pulse {
	i := sort.Search(len(intervals), func(i int) bool { return intervals[i].StartNS >= p.StartNS })
	intervals = append(intervals, ir.Pulse{})
	copy(intervals[i+1:], intervals[i:])
	intervals[i] = p
	return intervals
}

func insertSorted(xs []string, x string) []string {
	i := sort.SearchStrings(xs, x)
	xs = append(xs, "")
	copy(xs[i+1:], xs[i:])
	xs[i] = x
	return xs
}

func insertIndexSorted(xs []int, x int) []int {
	i := sort.SearchInts(xs, x)
	xs = append(xs, 0)
	copy(xs[i+1:], xs[i:])
	xs[i] = x
	return xs
}

func sortedPulses(ps []ir.Pulse) []ir.Pulse {
	out := append([]ir.Pulse(nil), ps...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartNS != out[j].StartNS {
			return out[i].StartNS < out[j].StartNS
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func samePulseShape(a, b ir.Pulse) bool {
	if a.Resource != b.Resource || a.DurationNS != b.DurationNS || a.Kind != b.Kind {
		return false
	}
	if len(a.Params) != len(b.Params) {
		return false
	}
	for k, v := range a.Params {
		if b.Params[k] != v {
			return false
		}
	}
	return true
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func cloneDeps(d map[string][]string) map[string][]string {
	out := make(map[string][]string, len(d))
	for k, v := range d {
		cp := append([]string(nil), v...)
		sort.Strings(cp)
		out[k] = cp
	}
	return out
}

func sameDeps(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	norm := func(d map[string][]string) map[string]bool {
		m := make(map[string]bool)
		for g, ds := range d {
			for _, dep := range ds {
				m[g+"->"+dep] = true
			}
		}
		return m
	}
	na, nb := norm(a), norm(b)
	if len(na) != len(nb) {
		return false
	}
	for k := range na {
		if !nb[k] {
			return false
		}
	}
	return true
}

func formatFloat(f float64) string {
	// 用固定精度，避免 map 遍历导致的字符串抖动。
	return sortFloat(f)
}
