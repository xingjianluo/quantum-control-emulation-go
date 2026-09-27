package sim

import (
	"fmt"
	"sort"
)

// Status 是单任务(门)在单个 shot 上的执行结果状态。
type Status int

const (
	StatusOK Status = iota
	StatusMeasured
	StatusFailed
	StatusCancelled
	StatusTimedOut
)

// String 返回状态可读名称。
func (s Status) String() string {
	switch s {
	case StatusOK:
		return "ok"
	case StatusMeasured:
		return "measured"
	case StatusFailed:
		return "failed"
	case StatusCancelled:
		return "cancelled"
	case StatusTimedOut:
		return "timed-out"
	default:
		return fmt.Sprintf("status-%d", int(s))
	}
}

// TaskReport 是单个门任务在全部 shot 上的聚合报告。
type TaskReport struct {
	GateID   string
	Status   Status // 聚合状态：任一 shot 非成功则按严重度升级（见 aggregate）
	Bit      int    // 测量门：多数票读出比特；非测量门为 -1
	Attempts int    // 实际执行的 shot 数（取消/超时后可能少于请求数）
	Detail   string
	// PerShot 记录每个 shot 的状态，便于解释与审计。
	PerShot []Status
}

// Event 是带全局因果顺序的执行事件。
type Event struct {
	Seq    int
	GateID string
	Kind   string
	Note   string
}

// JobResult 是一次作业的聚合结果。
type JobResult struct {
	Reports []TaskReport
	Events  []Event
	// Reused 是本次执行中从检查点复用的 (门, shot) 终态结果数；
	// Recomputed 是本次实际重新执行的 (门, shot) 数。
	// 非续跑执行时 Reused=0、Recomputed=门数×shot数。
	Reused     int
	Recomputed int
}

// shotOutcome 是单个门在单个 shot 上的原始结果。
type shotOutcome struct {
	status Status
	bit    int // 仅 status==StatusMeasured 时有意义
}

// Counts 返回按门聚合的状态计数。
func (r *JobResult) Counts() map[string]map[Status]int {
	out := make(map[string]map[Status]int)
	for _, rep := range r.Reports {
		m := make(map[Status]int)
		for _, st := range rep.PerShot {
			m[st]++
		}
		if len(rep.PerShot) == 0 {
			m[rep.Status]++
		}
		out[rep.GateID] = m
	}
	return out
}

// Equivalent 判断两个聚合结论是否等价：
// 每个门的 shot 数、各状态计数、多数票比特一致（事件文本允许不同，因为
// 并发交错顺序天然不同，但报告中的因果结论必须一致）。
func (r *JobResult) Equivalent(o *JobResult) bool {
	if len(r.Reports) != len(o.Reports) {
		return false
	}
	ob := make(map[string]TaskReport, len(o.Reports))
	for _, rep := range o.Reports {
		ob[rep.GateID] = rep
	}
	for _, rep := range r.Reports {
		other, ok := ob[rep.GateID]
		if !ok {
			return false
		}
		if rep.Attempts != other.Attempts || rep.Bit != other.Bit || rep.Status != other.Status {
			return false
		}
		if len(rep.PerShot) != len(other.PerShot) {
			return false
		}
		for i := range rep.PerShot {
			if rep.PerShot[i] != other.PerShot[i] {
				return false
			}
		}
	}
	return true
}

// sortedReports 返回按 GateID 排序的报告拷贝，供稳定日志使用。
func (r *JobResult) sortedReports() []TaskReport {
	out := append([]TaskReport(nil), r.Reports...)
	sort.Slice(out, func(i, j int) bool { return out[i].GateID < out[j].GateID })
	return out
}

// severity 给出状态严重度，用于跨 shot 聚合升级。
func severity(s Status) int {
	switch s {
	case StatusOK, StatusMeasured:
		return 0
	case StatusFailed:
		return 1
	case StatusTimedOut:
		return 2
	case StatusCancelled:
		return 3
	default:
		return 4
	}
}

// aggregateReports 把每 shot 结果汇总为每门一个 TaskReport。
func aggregateReports(gateOrder []string, per map[string][]shotOutcome) []TaskReport {
	out := make([]TaskReport, 0, len(gateOrder))
	for _, g := range gateOrder {
		ocs := per[g]
		rep := TaskReport{GateID: g, Bit: -1, Attempts: len(ocs), PerShot: make([]Status, len(ocs))}
		ones, zeros, worst := 0, 0, StatusOK
		hasMeasured := false
		for i, oc := range ocs {
			rep.PerShot[i] = oc.status
			if severity(oc.status) > severity(worst) {
				worst = oc.status
			}
			switch oc.status {
			case StatusMeasured:
				hasMeasured = true
				if oc.bit == 1 {
					ones++
				} else {
					zeros++
				}
			}
		}
		if hasMeasured {
			rep.Bit = boolToInt(ones >= zeros)
			if worst == StatusOK {
				worst = StatusMeasured
			}
		}
		rep.Status = worst
		rep.Detail = fmt.Sprintf("shots=%d ones=%d zeros=%d worst=%s", len(ocs), ones, zeros, worst)
		out = append(out, rep)
	}
	return out
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
