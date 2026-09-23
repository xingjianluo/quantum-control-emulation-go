package ir

// GateKindForRate 供噪声层区分错误率基线；脉冲只携带 GateID，
// 但 Params/Kind 足以分类，这里用 OpKind + 资源数量近似归类。
func (o *PulseOp) GateKindForRate() GateKind {
	if o.Kind == OpReadout {
		return GateMeas
	}
	if len(o.Resources) >= 2 {
		return GateCNOT
	}
	return GateX
}
