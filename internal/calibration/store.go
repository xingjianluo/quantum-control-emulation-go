// Package calibration 提供校准参数与读出保真度的分层管理。
//
// 分层规则：
//   - 参数有两个图层：全局层(资源键 "*") 与资源层(如 "q0.ctrl")。
//     资源层同名键覆盖全局层，合并结果是执行时生效的有效参数。
//   - 读出保真度按量子比特独立分层管理。
//
// 生命周期规则：
//   - 每个版本带 CreatedAt；距当前时间超过 TTL 即判为“过期”，Current/
//     CurrentFor 一律拒绝，禁止静默沿用旧值，必须重新校准(Commit)。
//   - Commit 对候选做完整校验：非法值(FailBadValue)、缺键(FailMissingKey)、
//     相对最近稳定版本漂移超差(FailOutOfTolerance)都会被拒绝，不产生新版本。
//   - 新版本提交后为非稳定；MarkStable 将其提升为新的稳定锚点。
//   - Rollback 把 head 恢复到上一稳定版本；与该版本绑定的历史结论随之可读，
//     仿真据此可与历史记录逐字节对齐。
package calibration

import (
	"math"
	"sort"
	"sync"
	"time"
)

// GlobalLayer 是参数分层中的全局层资源键。
const GlobalLayer = "*"

// Version 描述一次校准版本。
type Version struct {
	ID        string
	CreatedAt time.Time
	Stable    bool
	// Parameters: 第一层为资源层(GlobalLayer 为全局层)，第二层为参数键。
	Parameters map[string]map[string]float64
	ReadoutFid map[int]float64
	Note       string
}

// Candidate 是一次待提交的校准结果。
type Candidate struct {
	Parameters map[string]map[string]float64
	ReadoutFid map[int]float64
	Note       string
}

// HistorySummary 是与版本绑定的、用于复现比对的历史结论（不透明但可比较）。
type HistorySummary struct {
	VersionID string
	Summary   string
}

// Store 管理校准参数的分层、过期、提交、回滚与历史结论。
type Store struct {
	mu         sync.Mutex
	versions   []Version
	head       int
	stableAt   int // head 最近一次指向稳定版本时的下标；-1 表示无稳定版本
	ttl        time.Duration
	tolerances map[string]float64
	required   map[string]bool
	history    map[string]HistorySummary
	counter    int
	qubits     int
}

// NewStore 创建校准存储。
//
// tolerances 给出参数键允许的绝对漂移量；required 给出候选必须提供的参数键
// （至少在全局层或某资源层出现）。
func NewStore(ttl time.Duration, qubits int, tolerances map[string]float64, required []string) *Store {
	req := make(map[string]bool, len(required))
	for _, k := range required {
		req[k] = true
	}
	return &Store{
		head:       -1,
		stableAt:   -1,
		ttl:        ttl,
		tolerances: tolerances,
		required:   req,
		history:    make(map[string]HistorySummary),
		qubits:     qubits,
	}
}

// Seed 写入初始稳定版本（用于上电加载工厂校准）。
func (s *Store) Seed(v Version) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v.ID == "" {
		return calErr(FailBadValue, "seed version has empty id")
	}
	if err := validateValues(v.Parameters, v.ReadoutFid, s.qubits); err != nil {
		return err
	}
	v.Stable = true
	s.versions = append(s.versions, cloneVersion(v))
	s.head = len(s.versions) - 1
	s.stableAt = s.head
	return nil
}

// CurrentID 返回当前 head 版本 ID（不过期检查），便于日志与事件溯源。
func (s *Store) CurrentID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.head < 0 {
		return ""
	}
	return s.versions[s.head].ID
}

// Current 返回当前版本；若已过期则返回 FailExpired。
func (s *Store) Current(now time.Time) (Version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.head < 0 {
		return Version{}, calErr(FailNoStable, "no calibration version exists")
	}
	v := s.versions[s.head]
	if s.isExpired(v, now) {
		return Version{}, calErr(FailExpired, "version %s created at %s expired at %s (ttl=%s)",
			v.ID, v.CreatedAt.Format(time.RFC3339), now.Format(time.RFC3339), s.ttl)
	}
	return cloneVersion(v), nil
}

// CurrentFor 合并全局层与资源层，返回某资源的有效参数与该比特读出保真度。
// 当前版本过期时拒绝。返回的 map 是拷贝，调用方修改不影响存储。
func (s *Store) CurrentFor(resource string, qubit int, now time.Time) (map[string]float64, float64, error) {
	v, err := s.Current(now)
	if err != nil {
		return nil, 0, err
	}
	merged := make(map[string]float64)
	for k, val := range v.Parameters[GlobalLayer] {
		merged[k] = val
	}
	for k, val := range v.Parameters[resource] {
		merged[k] = val
	}
	fid, ok := v.ReadoutFid[qubit]
	if !ok {
		return nil, 0, calErr(FailMissingKey, "no readout fidelity for qubit %d in version %s", qubit, v.ID)
	}
	return merged, fid, nil
}

// Commit 校验并提交候选版本。
//
// 校验相对“最近稳定版本”进行（head 过期也允许重新校准，但漂移基准仍是稳定版）。
// 任何一项校验失败都返回可区分的 *Error，且不写入新版本——调用方能明确看到
// “校准失败、旧值未被沿用、新版本未生效”。
func (s *Store) Commit(c Candidate, now time.Time) (Version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateValues(c.Parameters, c.ReadoutFid, s.qubits); err != nil {
		return Version{}, err
	}
	if err := s.checkRequired(c.Parameters); err != nil {
		return Version{}, err
	}
	if s.stableAt >= 0 {
		if err := checkTolerance(s.versions[s.stableAt], c, s.tolerances); err != nil {
			return Version{}, err
		}
	}
	s.counter++
	v := Version{
		ID:         versionID(s.counter),
		CreatedAt:  now,
		Stable:     false,
		Parameters: cloneParams(c.Parameters),
		ReadoutFid: cloneFid(c.ReadoutFid),
		Note:       c.Note,
	}
	s.versions = append(s.versions, v)
	s.head = len(s.versions) - 1
	return cloneVersion(v), nil
}

// MarkStable 将当前 head 版本标记为稳定锚点。
func (s *Store) MarkStable(now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.head < 0 {
		return calErr(FailNoStable, "cannot mark stable: no current version")
	}
	v := s.versions[s.head]
	if s.isExpired(v, now) {
		return calErr(FailExpired, "refuse to mark expired version %s as stable", v.ID)
	}
	s.versions[s.head].Stable = true
	s.stableAt = s.head
	return nil
}

// Rollback 回滚到上一稳定版本，返回回滚后的版本。
// 非稳定的后续版本保留在历史中但不再是 head，可审计。
func (s *Store) Rollback(now time.Time) (Version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stableAt < 0 {
		return Version{}, calErr(FailNoStable, "no stable version to roll back to")
	}
	s.head = s.stableAt
	v := s.versions[s.head]
	if s.isExpired(v, now) {
		// 回滚本身仍完成（锚点不变），但调用方必须知道该稳定版也已过期。
		return cloneVersion(v), calErr(FailExpired, "rolled back to %s but it is expired at %s", v.ID, now.Format(time.RFC3339))
	}
	return cloneVersion(v), nil
}

// AttachHistory 把仿真结论与指定版本绑定（版本必须存在）。
func (s *Store) AttachHistory(versionID string, h HistorySummary) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.versions {
		if v.ID == versionID {
			h.VersionID = versionID
			s.history[versionID] = h
			return nil
		}
	}
	return calErr(FailBadValue, "cannot attach history: unknown version %q", versionID)
}

// HistoryAt 读取与版本绑定的历史结论。
func (s *Store) HistoryAt(versionID string) (HistorySummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.history[versionID]
	if !ok {
		return HistorySummary{}, calErr(FailMissingKey, "no history attached to version %q", versionID)
	}
	return h, nil
}

// Len 返回版本总数（含已被回滚离开 head 的版本），便于测试审计。
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.versions)
}

func (s *Store) isExpired(v Version, now time.Time) bool {
	if s.ttl <= 0 {
		return false
	}
	return now.Sub(v.CreatedAt) > s.ttl
}

func (s *Store) checkRequired(params map[string]map[string]float64) error {
	have := make(map[string]bool)
	for _, layer := range params {
		for k := range layer {
			have[k] = true
		}
	}
	missing := make([]string, 0)
	for k := range s.required {
		if !have[k] {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return calErr(FailMissingKey, "candidate missing required param keys: %v", missing)
	}
	return nil
}

func validateValues(params map[string]map[string]float64, fids map[int]float64, qubits int) error {
	layers := make([]string, 0, len(params))
	for l := range params {
		layers = append(layers, l)
	}
	sort.Strings(layers)
	for _, l := range layers {
		keys := make([]string, 0, len(params[l]))
		for k := range params[l] {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			val := params[l][k]
			if math.IsNaN(val) || math.IsInf(val, 0) {
				return calErr(FailBadValue, "param %s/%s is non-finite", l, k)
			}
			if val < 0 {
				return calErr(FailBadValue, "param %s/%s=%v must be non-negative", l, k, val)
			}
			if k == "amp" && (val <= 0 || val > 1.0) {
				return calErr(FailBadValue, "param %s/amp=%v out of (0,1]", l, val)
			}
		}
	}
	qs := make([]int, 0, len(fids))
	for q := range fids {
		qs = append(qs, q)
	}
	sort.Ints(qs)
	for _, q := range qs {
		if q < 0 || q >= qubits {
			return calErr(FailBadValue, "readout fidelity for qubit %d outside [0,%d)", q, qubits)
		}
		f := fids[q]
		if math.IsNaN(f) || f < 0 || f > 1 {
			return calErr(FailBadValue, "readout fidelity for qubit %d = %v out of [0,1]", q, f)
		}
	}
	return nil
}

func checkTolerance(base Version, c Candidate, tol map[string]float64) error {
	drifts := make([]string, 0)
	// 容差在“合并后的有效参数”上比较：资源层先继承全局层再覆盖。
	// 需要比较的资源集合 = 基线与候选出现过的所有资源层。
	resources := make(map[string]bool)
	for r := range base.Parameters {
		if r != GlobalLayer {
			resources[r] = true
		}
	}
	for r := range c.Parameters {
		if r != GlobalLayer {
			resources[r] = true
		}
	}
	// 全局层本身也要比较。
	rs := make([]string, 0, len(resources)+1)
	rs = append(rs, GlobalLayer)
	for r := range resources {
		rs = append(rs, r)
	}
	sort.Strings(rs[1:])
	for _, r := range rs {
		oldLayer := mergedLayer(base.Parameters, r)
		newLayer := mergedLayer(c.Parameters, r)
		keys := make(map[string]bool)
		for k := range oldLayer {
			keys[k] = true
		}
		for k := range newLayer {
			keys[k] = true
		}
		ks := make([]string, 0, len(keys))
		for k := range keys {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		for _, k := range ks {
			limit, watched := tol[k]
			if !watched {
				continue
			}
			oldv, oldOK := oldLayer[k]
			newv, newOK := newLayer[k]
			if oldOK && !newOK {
				drifts = append(drifts, r+"/"+k+" removed (baseline="+floatStr(oldv)+")")
				continue
			}
			if d := math.Abs(newv - oldv); d > limit {
				drifts = append(drifts, r+"/"+k+" drift="+floatStr(d)+" > tol="+floatStr(limit))
			}
		}
	}
	if len(drifts) > 0 {
		sort.Strings(drifts)
		return calErr(FailOutOfTolerance, "candidate rejected vs stable %s: %v", base.ID, drifts)
	}
	return nil
}

// mergedLayer 返回某资源层合并全局层后的有效参数（拷贝）。
func mergedLayer(params map[string]map[string]float64, resource string) map[string]float64 {
	out := make(map[string]float64)
	for k, v := range params[GlobalLayer] {
		out[k] = v
	}
	if resource != GlobalLayer {
		for k, v := range params[resource] {
			out[k] = v
		}
	}
	return out
}

func versionID(n int) string {
	return "v" + itoa(n)
}

func cloneVersion(v Version) Version {
	return Version{
		ID:         v.ID,
		CreatedAt:  v.CreatedAt,
		Stable:     v.Stable,
		Parameters: cloneParams(v.Parameters),
		ReadoutFid: cloneFid(v.ReadoutFid),
		Note:       v.Note,
	}
}

func cloneParams(in map[string]map[string]float64) map[string]map[string]float64 {
	out := make(map[string]map[string]float64, len(in))
	for r, m := range in {
		cp := make(map[string]float64, len(m))
		for k, v := range m {
			cp[k] = v
		}
		out[r] = cp
	}
	return out
}

func cloneFid(in map[int]float64) map[int]float64 {
	out := make(map[int]float64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
