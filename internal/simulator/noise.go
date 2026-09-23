package simulator

import (
	"hash/fnv"

	"github.com/xingjianluo/quantum-control-emulation-go/internal/ir"
)

// noiseModel 是确定性噪声模型：所有"随机"判定都来自 (seed, 计划版本, opID, 用途标签)
// 的 FNV-1a 哈希，而不是全局随机源。因此同一输入在顺序/并发模式、不同机器、
// 不同 Go 版本上结论完全一致且可复现。
type noiseModel struct {
	seed int64
}

// hashProb 返回 [0,1) 的确定性均匀数。tag 用于区分同一操作上的不同随机用途
// （如门错误与读出错误是独立判定）。
func (m *noiseModel) hashProb(version int, opID, tag string) float64 {
	h := fnv.New64a()
	var buf [8]byte
	putInt64(buf[:], m.seed)
	h.Write(buf[:])
	putInt64(buf[:], int64(version))
	h.Write(buf[:])
	h.Write([]byte(opID))
	h.Write([]byte{0})
	h.Write([]byte(tag))
	v := h.Sum64()
	// 取 53 位尾数，映射到 [0,1)
	return float64(v>>11) / float64(int64(1)<<53)
}

// gateErrorRate 是脉冲层门错误的确定性概率：
// 与操作参数（幅度）和固定门类别基线相关，但完全由计划内容决定。
func gateErrorRate(op *ir.PulseOp) float64 {
	switch op.GateKindForRate() {
	case ir.GateCNOT:
		return 0.02 // 两比特门基线 2%
	case ir.GateMeas:
		return 0.0 // 测量错误在读出环节建模
	default:
		return 0.005 // 单比特门基线 0.5%
	}
}

// gateFailed 判定某操作是否发生门错误（确定性）。
func (m *noiseModel) gateFailed(version int, op *ir.PulseOp) bool {
	return m.hashProb(version, op.ID, "gate") < gateErrorRate(op)
}

// readoutBit 在已知理想比特 ideal 与保真度 f 下产出确定性读出结果。
func (m *noiseModel) readoutBit(version int, op *ir.PulseOp, f float64, ideal int) int {
	p := m.hashProb(version, op.ID, "readout")
	if p < f {
		return ideal // 正确读出
	}
	return 1 - ideal // 确定性翻转
}

func putInt64(b []byte, v int64) {
	for i := 0; i < 8; i++ {
		b[i] = byte(v >> (8 * i))
	}
}
