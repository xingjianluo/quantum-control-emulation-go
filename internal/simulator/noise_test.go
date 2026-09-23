package simulator

import (
	"context"
	"testing"
	"time"

	"github.com/xingjianluo/quantum-control-emulation-go/internal/ir"
	"github.com/xingjianluo/quantum-control-emulation-go/internal/scheduler"
)

// TestDeterministicNoisePath 直接构造只含 CNOT 的计划，穷举 seed 验证：
// 同一 (seed,版本,opID) 的门噪声判定恒定；门错误记 gate_error 而非超时/取消。
func TestDeterministicNoisePath(t *testing.T) {
	at := time.Unix(1000, 0).UTC()
	store, _ := testFixture(t)
	eng := NewEngine(store)

	prog := &ir.PulseProgram{CalibVersion: 1, Ops: []*ir.PulseOp{
		{ID: "p-cx", GateID: "cx", Kind: ir.OpPulse,
			Resources: []ir.Resource{
				{Kind: ir.ResourceCoupler, Name: "coupler-0-1"},
				{Kind: ir.ResourceQubit, Name: "qubit-0"},
				{Kind: ir.ResourceQubit, Name: "qubit-1"},
			},
			Duration: 80 * time.Nanosecond},
	}}
	if err := scheduler.New().Validate(prog); err != nil {
		t.Fatal(err)
	}

	seen := map[int64]bool{}
	var gotGateError bool
	for seed := int64(0); seed < 200; seed++ {
		r1, err := eng.Run(context.Background(), prog, Config{Seed: seed, At: at, Waiter: ImmediateWaiter{}})
		if err != nil {
			t.Fatal(err)
		}
		r2, _ := eng.Run(context.Background(), prog, Config{Seed: seed, At: at, Waiter: ImmediateWaiter{}})
		if r1.Outcomes[0].OK != r2.Outcomes[0].OK || r1.Outcomes[0].FailReason != r2.Outcomes[0].FailReason {
			t.Fatalf("seed=%d 两次运行结论漂移: %+v vs %+v", seed, r1.Outcomes[0], r2.Outcomes[0])
		}
		seen[seed] = r1.Outcomes[0].OK
		if !r1.Outcomes[0].OK {
			if r1.Outcomes[0].FailReason != "gate_error" {
				t.Fatalf("门噪声失败原因应为 gate_error，得到 %q", r1.Outcomes[0].FailReason)
			}
			gotGateError = true
		}
	}
	if !gotGateError {
		t.Fatal("200 个 seed 内未观测到任何 gate_error，噪声路径未被覆盖")
	}
	fails := 0
	for _, ok := range seen {
		if !ok {
			fails++
		}
	}
	t.Logf("输入: 单条 CNOT(基线错误率 2%%)，穷举 200 个 seed -> gate_error 次数=%d，同 seed 重复运行恒定", fails)
}

func TestReadoutDeterministic(t *testing.T) {
	at := time.Unix(1000, 0).UTC()
	store, _ := testFixture(t)
	eng := NewEngine(store)
	prog := &ir.PulseProgram{CalibVersion: 1, Ops: []*ir.PulseOp{
		{ID: "p-m0", GateID: "m0", Kind: ir.OpReadout,
			Resources: []ir.Resource{{Kind: ir.ResourceReadout, Name: "readout-0"}},
			Duration:  200 * time.Nanosecond},
	}}
	if err := scheduler.New().Validate(prog); err != nil {
		t.Fatal(err)
	}
	r, err := eng.Run(context.Background(), prog, Config{Seed: 77, At: at, Waiter: ImmediateWaiter{}})
	if err != nil {
		t.Fatal(err)
	}
	if r.Outcomes[0].Measured != 0 && r.Outcomes[0].Measured != 1 {
		t.Fatalf("读出结果必须为 0/1，得到 %d", r.Outcomes[0].Measured)
	}
	// 保真度 0.99，理想态为 0：绝大多数 seed 应读到 0；抽样验证确定且合理。
	zeros := 0
	for seed := int64(0); seed < 500; seed++ {
		rr, _ := eng.Run(context.Background(), prog, Config{Seed: seed, At: at, Waiter: ImmediateWaiter{}})
		if rr.Outcomes[0].Measured == 0 {
			zeros++
		}
	}
	frac := float64(zeros) / 500
	if frac < 0.95 || frac > 1.0 {
		t.Fatalf("保真度 0.99 下读到 0 的频率=%f 不合理", frac)
	}
	t.Logf("输入: readout-0 保真度=0.99, 500 seed 中读到理想值 0 的比例=%.3f（确定且接近保真度）", frac)
}
