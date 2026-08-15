package service

import (
	"sort"
	"time"
)

// 首 Token 时延（TTFT）质量信号。
//
// 现有健康分的扣分触发器全是「失败」类事件（4xx/5xx/超时/流中断），
// 一个账号慢但不报错时健康分永远是 100。本文件补上「慢」这一维：
// 按「账号 × 模型」聚合 TTFT，与同模型其余账号的基线比较，明显变慢的账号扣健康分。
//
// 判据是**相对倍率**而非绝对阈值：thinking / 长上下文模型的 TTFT 天然就高，
// 绝对阈值会系统性误杀它们（OpenAI 侧既有的 sticky_escape_ttft_ms 正是绝对阈值，
// 默认值为 0 即从未启用，多半就是因为这个）。

// OpsAccountModelTTFTRow 是「账号 × 分组 × 实际上游模型」维度的 TTFT 聚合结果。
type OpsAccountModelTTFTRow struct {
	AccountID   int64
	GroupID     int64
	AccountName string
	Platform    string
	Model       string
	TTFTP50Ms   float64
	TTFTP95Ms   float64
	Samples     int64
}

// AccountModelTTFT 是快照里的单模型明细，供 UI 展示「慢在哪个模型上」。
type AccountModelTTFT struct {
	GroupID  int64   `json:"group_id"`
	Model    string  `json:"model"`
	P50Ms    float64 `json:"p50_ms"`
	P95Ms    float64 `json:"p95_ms"`
	Samples  int64   `json:"samples"`
	Baseline float64 `json:"baseline_ms"` // 同模型基线；0 表示样本不足以建立基线
	Ratio    float64 `json:"ratio"`       // P50Ms / Baseline；0 表示无基线可比
}

// AccountTTFTSnapshot 是单账号一轮评估的结果，写入 Redis 供账号列表接口读取。
type AccountTTFTSnapshot struct {
	AccountID int64 `json:"account_id"`
	// P50Ms / P95Ms 为跨模型的样本加权均值：账号可能同时服务快慢差异极大的模型，
	// 直接取算术平均会被低频慢模型带偏，按样本数加权才反映该账号的实际体感。
	P50Ms   float64 `json:"p50_ms"`
	P95Ms   float64 `json:"p95_ms"`
	Samples int64   `json:"samples"`
	// Ratio 为样本加权的相对基线倍率；0 表示本轮没有任何模型能建立基线。
	Ratio float64 `json:"ratio"`
	// WorstModel / WorstRatio 记录倍率最高的模型，让「整体看着还行但某个模型很慢」
	// 也能在 UI 上暴露出来，而不是被加权平均抹平。
	WorstGroupID int64   `json:"worst_group_id,omitempty"`
	WorstModel   string  `json:"worst_model,omitempty"`
	WorstRatio   float64 `json:"worst_ratio"`
	// Degraded 表示本轮判定为相对退化（会触发健康分扣分，前提是扣分已启用）。
	Degraded  bool               `json:"degraded"`
	Models    []AccountModelTTFT `json:"models,omitempty"`
	UpdatedAt time.Time          `json:"updated_at"`
}

// AccountTTFTEvalConfig 是一轮评估的判定参数。
type AccountTTFTEvalConfig struct {
	// MinSamples 单个「账号 × 模型」参与判定所需的最小样本数。
	MinSamples int
	// MinBaselineAccounts 建立某模型基线所需的最少账号数。
	// 只有一两个账号在跑的模型没有「同行」可比，此时不做判定（fail-open）。
	MinBaselineAccounts int
	// DegradeRatio 相对基线的退化倍率阈值（如 2.0 表示慢一倍即判退化）。
	DegradeRatio float64
	// MaxModelsPerAccount 快照里保留的模型明细条数上限，防止快照无界增长。
	MaxModelsPerAccount int
}

// AssessAccountTTFT 把「账号 × 分组 × 模型」聚合行换算成每账号的 TTFT 快照。
//
// 基线取「同分组 + 同模型下各账号 P50 的中位数」而非均值：中位数不会被一两个极慢账号拉高，
// 否则出现「大家都被一个坏账号抬高了基线，于是谁都不算退化」的自我掩盖。
//
// 返回值包含所有有样本的账号（哪怕无法判定退化），因为 UI 需要展示 TTFT 本身；
// 无法建立基线时 Ratio 为 0、Degraded 为 false。
func AssessAccountTTFT(rows []OpsAccountModelTTFTRow, cfg AccountTTFTEvalConfig, now time.Time) map[int64]*AccountTTFTSnapshot {
	if len(rows) == 0 {
		return nil
	}
	normalizeTTFTEvalConfig(&cfg)

	baselines := buildTTFTBaselines(rows, cfg)

	out := make(map[int64]*AccountTTFTSnapshot, len(rows))
	// 加权累加器：按样本数对 p50 / p95 / ratio 加权。
	type accumulator struct {
		weightedP50   float64
		weightedP95   float64
		weightedRatio float64
		ratioSamples  int64
	}
	acc := make(map[int64]*accumulator, len(rows))

	for _, row := range rows {
		if row.AccountID <= 0 || row.Samples <= 0 {
			continue
		}
		snapshot := out[row.AccountID]
		if snapshot == nil {
			snapshot = &AccountTTFTSnapshot{AccountID: row.AccountID, UpdatedAt: now}
			out[row.AccountID] = snapshot
			acc[row.AccountID] = &accumulator{}
		}
		a := acc[row.AccountID]

		baseline := baselines[ttftBaselineKey{groupID: row.GroupID, model: row.Model}]
		ratio := 0.0
		if baseline > 0 && row.TTFTP50Ms > 0 {
			ratio = row.TTFTP50Ms / baseline
		}

		snapshot.Models = append(snapshot.Models, AccountModelTTFT{
			GroupID:  row.GroupID,
			Model:    row.Model,
			P50Ms:    row.TTFTP50Ms,
			P95Ms:    row.TTFTP95Ms,
			Samples:  row.Samples,
			Baseline: baseline,
			Ratio:    ratio,
		})
		snapshot.Samples += row.Samples

		weight := float64(row.Samples)
		a.weightedP50 += row.TTFTP50Ms * weight
		a.weightedP95 += row.TTFTP95Ms * weight
		if ratio > 0 {
			a.weightedRatio += ratio * weight
			a.ratioSamples += row.Samples
			if ratio > snapshot.WorstRatio {
				snapshot.WorstRatio = ratio
				snapshot.WorstGroupID = row.GroupID
				snapshot.WorstModel = row.Model
			}
		}
	}

	for accountID, snapshot := range out {
		a := acc[accountID]
		if snapshot.Samples > 0 {
			snapshot.P50Ms = a.weightedP50 / float64(snapshot.Samples)
			snapshot.P95Ms = a.weightedP95 / float64(snapshot.Samples)
		}
		if a.ratioSamples > 0 {
			snapshot.Ratio = a.weightedRatio / float64(a.ratioSamples)
		}
		// 判定退化用加权倍率而非最差模型：某个低频模型偶发变慢不该让整个账号降级，
		// 而账号主力模型变慢会因样本占比高而主导加权值。
		snapshot.Degraded = snapshot.Ratio > cfg.DegradeRatio && a.ratioSamples >= int64(cfg.MinSamples)

		sort.Slice(snapshot.Models, func(i, j int) bool {
			if snapshot.Models[i].Samples != snapshot.Models[j].Samples {
				return snapshot.Models[i].Samples > snapshot.Models[j].Samples
			}
			if snapshot.Models[i].Model != snapshot.Models[j].Model {
				return snapshot.Models[i].Model < snapshot.Models[j].Model
			}
			return snapshot.Models[i].GroupID < snapshot.Models[j].GroupID
		})
		if len(snapshot.Models) > cfg.MaxModelsPerAccount {
			snapshot.Models = snapshot.Models[:cfg.MaxModelsPerAccount]
		}
	}

	return out
}

// buildTTFTBaselines 为每个「分组 + 模型」算出基线（各账号 P50 的中位数）。
// 参与账号数不足 MinBaselineAccounts 的模型不产出基线 —— 样本太少时
// 「中位数」本身就是噪声，宁可不判定也不要误伤。
type ttftBaselineKey struct {
	groupID int64
	model   string
}

func buildTTFTBaselines(rows []OpsAccountModelTTFTRow, cfg AccountTTFTEvalConfig) map[ttftBaselineKey]float64 {
	perCohort := make(map[ttftBaselineKey][]float64)
	for _, row := range rows {
		if row.Model == "" || row.TTFTP50Ms <= 0 || row.Samples < int64(cfg.MinSamples) {
			continue
		}
		key := ttftBaselineKey{groupID: row.GroupID, model: row.Model}
		perCohort[key] = append(perCohort[key], row.TTFTP50Ms)
	}

	baselines := make(map[ttftBaselineKey]float64, len(perCohort))
	for key, values := range perCohort {
		if len(values) < cfg.MinBaselineAccounts {
			continue
		}
		baselines[key] = medianFloat64(values)
	}
	return baselines
}

// medianFloat64 就地排序后取中位数；偶数个取中间两者的均值。调用方传入的切片会被重排。
func medianFloat64(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sort.Float64s(values)
	mid := len(values) / 2
	if len(values)%2 == 1 {
		return values[mid]
	}
	return (values[mid-1] + values[mid]) / 2
}

func normalizeTTFTEvalConfig(cfg *AccountTTFTEvalConfig) {
	if cfg.MinSamples < 1 {
		cfg.MinSamples = 1
	}
	if cfg.MinBaselineAccounts < 2 {
		cfg.MinBaselineAccounts = 2
	}
	if cfg.DegradeRatio <= 1 {
		cfg.DegradeRatio = 2.0
	}
	if cfg.MaxModelsPerAccount < 1 {
		cfg.MaxModelsPerAccount = 5
	}
}
