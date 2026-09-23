package simulator

import (
	"context"
	"time"

	"github.com/xingjianluo/quantum-control-emulation-go/internal/ir"
)

// Waiter 抽象"执行一条脉冲所消耗的时间"，并知道当前操作身份，
// 便于测试确定性地制造超时与在途取消。
//
// 仿真语义关注并发、取消与超时的因果行为而非真实物理耗时，因此：
//   - RealWaiter 用定时器按脉冲时长真实等待（demo 使用）；
//   - ImmediateWaiter 立即返回，零墙钟耗时，仅尊重 ctx 取消（快仿真默认）；
//   - DeterministicWaiter 按操作时长与上限确定性地产生超时，不睡眠；
//   - BlockingWaiter 对指定操作阻塞至 ctx 结束，用于在途取消测试。
type Waiter interface {
	Wait(ctx context.Context, op *ir.PulseOp) error
}

// RealWaiter 按墙钟时间等待，ctx 取消/超时时立即返回 ctx.Err()。
type RealWaiter struct{}

// Wait 实现 Waiter。
func (RealWaiter) Wait(ctx context.Context, op *ir.PulseOp) error {
	if op.Duration <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(op.Duration)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ImmediateWaiter 不消耗墙钟时间，仅尊重 ctx 的取消/超时状态。
type ImmediateWaiter struct{}

// Wait 实现 Waiter。
func (ImmediateWaiter) Wait(ctx context.Context, _ *ir.PulseOp) error {
	return ctx.Err()
}

// DeterministicWaiter 不睡眠：时长超过 MaxDuration 即确定性地报告超时，
// 其余情况尊重 ctx。用于无墙钟抖动地测试超时语义。
type DeterministicWaiter struct {
	MaxDuration time.Duration // <=0 表示不限
}

// Wait 实现 Waiter。
func (w DeterministicWaiter) Wait(ctx context.Context, op *ir.PulseOp) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if w.MaxDuration > 0 && op.Duration > w.MaxDuration {
		return context.DeadlineExceeded
	}
	return nil
}

// BlockingWaiter 对 Block 集合中的操作一直阻塞到 ctx 结束，
// 其余操作立即成功。Entered 在某操作刚进入阻塞时被非阻塞地通知，
// 用于确定性地触发"在途取消"而不依赖固定 sleep。
type BlockingWaiter struct {
	Block   map[string]bool
	Entered chan<- string
}

// Wait 实现 Waiter。
func (w BlockingWaiter) Wait(ctx context.Context, op *ir.PulseOp) error {
	if !w.Block[op.ID] {
		return ctx.Err()
	}
	if w.Entered != nil {
		select {
		case w.Entered <- op.ID:
		default:
		}
	}
	<-ctx.Done()
	return ctx.Err()
}
