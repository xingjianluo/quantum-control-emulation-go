package calibration

import "sort"

// VersionByID 按版本 ID 查找历史版本（含已回滚离开 head 的版本）。
// 续跑兼容性判断需要读取中断点时的版本内容，与当前 head 比较。
func (s *Store) VersionByID(id string) (Version, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.versions {
		if v.ID == id {
			return cloneVersion(v), true
		}
	}
	return Version{}, false
}

// DiffEffective 比较两个版本“合并后的有效参数”与读出保真度，
// 返回发生变化的资源层与比特，供续跑判定受影响范围：
//
//   - globalChanged: 全局层("*")合并结果有变化（影响所有资源，调用方应视为全量受影响）；
//   - resources: 非全局资源层中合并结果发生变化的资源键（排序后返回）；
//   - qubits: 读出保真度发生变化的比特（排序后返回）。
//
// 比较在合并层上进行：资源层继承全局层后再覆盖，因此全局层的变化只会体现在
// globalChanged 与显式存在的资源层上；未显式出现的资源层由 globalChanged 覆盖。
func DiffEffective(a, b Version) (globalChanged bool, resources []string, qubits []int) {
	if !paramsEqual(mergedLayer(a.Parameters, GlobalLayer), mergedLayer(b.Parameters, GlobalLayer)) {
		globalChanged = true
	}
	layers := make(map[string]bool)
	for r := range a.Parameters {
		if r != GlobalLayer {
			layers[r] = true
		}
	}
	for r := range b.Parameters {
		if r != GlobalLayer {
			layers[r] = true
		}
	}
	for r := range layers {
		if !paramsEqual(mergedLayer(a.Parameters, r), mergedLayer(b.Parameters, r)) {
			resources = append(resources, r)
		}
	}
	sort.Strings(resources)
	qs := make(map[int]bool)
	for q := range a.ReadoutFid {
		qs[q] = true
	}
	for q := range b.ReadoutFid {
		qs[q] = true
	}
	for q := range qs {
		if a.ReadoutFid[q] != b.ReadoutFid[q] {
			qubits = append(qubits, q)
		}
	}
	sort.Ints(qubits)
	return globalChanged, resources, qubits
}

func paramsEqual(a, b map[string]float64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}
