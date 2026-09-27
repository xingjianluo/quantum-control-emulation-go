package sim

import (
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"sort"

	"github.com/xingjianluo/quantum-control-emulation-go/internal/ir"
)

// ShotRecord 是断点中单个 (门, shot) 的记录。
type ShotRecord struct {
	Status Status
	Bit    int // 仅 Status==StatusMeasured 时有意义
	// Final 为 true 表示该 shot 已真正执行出结果（ok/measured/failed），
	// 续跑时直接复用；false 表示因取消/超时未完成，续跑时必须重算。
	// 由此“被打断未开始”与“真正执行失败”在断点中即可区分。
	Final bool
}

// Checkpoint 是一次被中断（或完成）作业的可续跑状态。
//
// 它带回两类信息：已经完成的部分（Final=true 的每门每 shot 结果）与还没
// 开始/未完成的部分（Final=false，续跑时重算）。JobKey 是作业指纹，用于
// 拒绝“拿错作业或对不上的状态来续跑”；CalVersion 记录中断点时的校准版本，
// 供续跑时判断兼容性。
type Checkpoint struct {
	JobKey     string
	CalVersion string
	Shots      int
	GateOrder  []string
	// Records[gate][shot] 给出每个门每个 shot 的断点记录，长度恒为 Shots。
	Records map[string][]ShotRecord
}

// finalShots 统计断点中已完成（可复用）的 shot 数。
func (c *Checkpoint) finalShots() int {
	n := 0
	for _, rs := range c.Records {
		for _, r := range rs {
			if r.Final {
				n++
			}
		}
	}
	return n
}

// reuseOutcomes 把断点中 Final 的记录提取为可直接复用的结果表。
// gates 非空时只提取这些门（其余门即使 Final 也被强制重算，
// 用于校准变化后“重算受影响部分”的场景）。
func (c *Checkpoint) reuseOutcomes(onlyGates map[string]bool) map[string][]shotOutcome {
	out := make(map[string][]shotOutcome)
	for g, rs := range c.Records {
		if onlyGates != nil && !onlyGates[g] {
			continue
		}
		for shot, r := range rs {
			if !r.Final {
				continue
			}
			if out[g] == nil {
				out[g] = make([]shotOutcome, len(rs))
			}
			out[g][shot] = shotOutcome{status: r.Status, bit: r.Bit, reused: true}
		}
	}
	return out
}

// newCheckpoint 从一次执行的原始结果构建断点。
// calVersion 是本次执行开始时的校准版本 ID。
func newCheckpoint(j Job, calVersion string, order []string, per map[string][]shotOutcome) *Checkpoint {
	cp := &Checkpoint{
		JobKey:     jobFingerprint(j),
		CalVersion: calVersion,
		Shots:      j.Shots,
		GateOrder:  append([]string(nil), order...),
		Records:    make(map[string][]ShotRecord, len(order)),
	}
	for _, g := range order {
		rs := make([]ShotRecord, j.Shots)
		ocs := per[g]
		for shot := 0; shot < j.Shots; shot++ {
			if shot < len(ocs) {
				oc := ocs[shot]
				rs[shot] = ShotRecord{Status: oc.status, Bit: oc.bit, Final: isFinalStatus(oc.status)}
			} else {
				rs[shot] = ShotRecord{Status: StatusCancelled, Bit: 0, Final: false}
			}
		}
		cp.Records[g] = rs
	}
	return cp
}

// isFinalStatus 判断一个 shot 状态是否为“已执行出结果”。
// failed 是真正的执行失败（确定性，重跑结论相同），属于已完成；
// cancelled/timed-out 是被打断未完成，续跑时必须重算。
func isFinalStatus(s Status) bool {
	switch s {
	case StatusOK, StatusMeasured, StatusFailed:
		return true
	default:
		return false
	}
}

// jobFingerprint 计算作业指纹：覆盖门集合、依赖边、全部脉冲（含时序与参数）、
// shots、seed 与单门超时。任何一项不同都会得到不同指纹，续跑时据此拒绝
// “拿错作业/状态”的请求。Hooks 是不可比较的函数，按约定属于作业身份的一部分，
// 调用方应使用与原运行相同的 Hooks 续跑。
func jobFingerprint(j Job) string {
	h := fnv.New64a()
	fmt.Fprintf(h, "shots=%d seed=%d timeout-ns=%d\n", j.Shots, j.Seed, j.PerTaskTimeout)
	s := j.Schedule
	if s == nil {
		fmt.Fprintf(h, "nil-schedule\n")
		return hex.EncodeToString(h.Sum(nil))
	}
	for _, id := range s.GateIDs {
		deps := append([]string(nil), s.GateDeps[id]...)
		sort.Strings(deps)
		fmt.Fprintf(h, "gate %s deps=%v\n", id, deps)
	}
	pulses := append([]ir.Pulse(nil), s.Pulses...)
	sort.Slice(pulses, func(a, b int) bool { return pulses[a].ID < pulses[b].ID })
	for _, p := range pulses {
		fmt.Fprintf(h, "pulse %s gate=%s kind=%d res=%s start=%d dur=%d",
			p.ID, p.GateID, p.Kind, p.Resource, p.StartNS, p.DurationNS)
		keys := make([]string, 0, len(p.Params))
		for k := range p.Params {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(h, " %s=%g", k, p.Params[k])
		}
		fmt.Fprintln(h)
	}
	return hex.EncodeToString(h.Sum(nil))
}
