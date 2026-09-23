// Package sim 对脉冲执行与测量进行确定性噪声仿真与结果聚合。
//
// 确定性保证：所有随机数均来自按 (jobSeed, gateID, shot, 门内脉冲序号)
// 派生出的独立 PRNG 流。顺序执行与并发执行对同一个(门,shot)抽取到的随机
// 数完全相同，因此测量比特、脉冲失败判定与最终聚合结论逐字节一致，与调度
// 时序、goroutine 交错无关。
package sim

import "math/rand/v2"

// NoiseModel 是确定性噪声模型参数。
type NoiseModel struct {
	// DepolPerPulse 按资源给出单个控制脉冲的失败概率[0,1]。
	DepolPerPulse map[string]float64
	// AmpScale 按资源给出幅度标定乘子；与校准有效 amp 相乘后若超过 MaxAmp
	// 则该脉冲判定为失败（非法驱动），不会被计入成功。
	AmpScale map[string]float64
	MaxAmp   float64
}

// DefaultNoise 返回内置的确定性噪声模型。
func DefaultNoise() NoiseModel {
	return NoiseModel{
		DepolPerPulse: map[string]float64{
			"q0.ctrl": 0.02, "q1.ctrl": 0.03, "q2.ctrl": 0.04, "q3.ctrl": 0.05,
		},
		AmpScale: map[string]float64{
			"q0.ctrl": 1.0, "q1.ctrl": 1.02, "q2.ctrl": 0.98, "q3.ctrl": 1.05,
		},
		MaxAmp: 1.0,
	}
}

// gateStream 为指定 (seed, gate, shot) 派生独立、确定的 PRNG 流。
// 派生方式固定且与 map 遍历无关：FNV 风格混合 + rand.New(PCG)。
func gateStream(seed int64, gateID string, shot int) *rand.Rand {
	h := uint64(seed)
	h = mix(h, fnvString(gateID))
	h = mix(h, uint64(shot)+0x9E3779B97F4A7C15)
	return rand.New(rand.NewPCG(h^0x517cc1b727220a95, h^0x6c62272e07bb0142))
}

func fnvString(s string) uint64 {
	const offset = 1469598103934665603
	const prime = 1099511628211
	h := uint64(offset)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime
	}
	return h
}

func mix(a, b uint64) uint64 {
	a ^= b
	a ^= a >> 33
	a *= 0xff51afd7ed558ccd
	a ^= a >> 33
	a *= 0xc4ceb9fe1a85ec53
	a ^= a >> 33
	return a
}
