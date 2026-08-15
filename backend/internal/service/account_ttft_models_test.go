package service

import (
	"testing"
	"time"
)

func ttftTestConfig() AccountTTFTEvalConfig {
	return AccountTTFTEvalConfig{
		MinSamples:          10,
		MinBaselineAccounts: 3,
		DegradeRatio:        2.0,
		MaxModelsPerAccount: 5,
	}
}

func row(accountID int64, model string, p50 float64, samples int64) OpsAccountModelTTFTRow {
	return OpsAccountModelTTFTRow{
		AccountID: accountID,
		Model:     model,
		TTFTP50Ms: p50,
		TTFTP95Ms: p50 * 2,
		Samples:   samples,
	}
}

func TestAssessAccountTTFT_FlagsAccountSlowerThanPeers(t *testing.T) {
	now := time.Now()
	rows := []OpsAccountModelTTFTRow{
		row(1, "sonnet", 800, 100),
		row(2, "sonnet", 900, 100),
		row(3, "sonnet", 850, 100),
		row(4, "sonnet", 2600, 100), // 约 3 倍于中位数
	}

	got := AssessAccountTTFT(rows, ttftTestConfig(), now)

	if len(got) != 4 {
		t.Fatalf("expected 4 account snapshots, got %d", len(got))
	}
	if got[4] == nil || !got[4].Degraded {
		t.Fatalf("account 4 should be degraded, got %+v", got[4])
	}
	// 基线 = 中位数(800,900,850,2600) = (850+900)/2 = 875
	if want := 2600.0 / 875.0; got[4].Ratio < want-0.01 || got[4].Ratio > want+0.01 {
		t.Fatalf("account 4 ratio = %v, want ~%v", got[4].Ratio, want)
	}
	for _, id := range []int64{1, 2, 3} {
		if got[id].Degraded {
			t.Fatalf("account %d should not be degraded, ratio=%v", id, got[id].Ratio)
		}
	}
}

func TestAssessAccountTTFT_MedianBaselineResistsOutliers(t *testing.T) {
	now := time.Now()
	// 两个账号极慢。若基线取均值会被抬到约 1633ms，谁都不到 2 倍从而全部漏判；
	// 取中位数则基线仍是 900，两个慢账号都会被抓出来。
	rows := []OpsAccountModelTTFTRow{
		row(1, "sonnet", 800, 100),
		row(2, "sonnet", 900, 100),
		row(3, "sonnet", 1000, 100),
		row(4, "sonnet", 3000, 100),
		row(5, "sonnet", 3100, 100),
	}

	got := AssessAccountTTFT(rows, ttftTestConfig(), now)

	if !got[4].Degraded || !got[5].Degraded {
		t.Fatalf("outlier accounts should be degraded: 4=%+v 5=%+v", got[4], got[5])
	}
	if got[1].Degraded || got[2].Degraded || got[3].Degraded {
		t.Fatal("normal accounts should not be degraded")
	}
}

func TestAssessAccountTTFT_NoBaselineWhenTooFewAccounts(t *testing.T) {
	now := time.Now()
	// 只有两个账号在跑这个模型，低于 MinBaselineAccounts=3，不做判定（fail-open）。
	rows := []OpsAccountModelTTFTRow{
		row(1, "rare-model", 500, 100),
		row(2, "rare-model", 5000, 100),
	}

	got := AssessAccountTTFT(rows, ttftTestConfig(), now)

	if got[2].Degraded {
		t.Fatal("should not judge degradation without enough peers for a baseline")
	}
	if got[2].Ratio != 0 {
		t.Fatalf("ratio should be 0 when no baseline exists, got %v", got[2].Ratio)
	}
	// 无基线也要保留 TTFT 本身，UI 仍需展示。
	if got[2].P50Ms != 5000 {
		t.Fatalf("p50 should still be reported, got %v", got[2].P50Ms)
	}
}

func TestAssessAccountTTFT_SmallSamplesExcludedFromBaseline(t *testing.T) {
	now := time.Now()
	// 样本不足的行不参与基线构建：否则一个只跑了 2 次的慢账号会污染基线。
	rows := []OpsAccountModelTTFTRow{
		row(1, "sonnet", 800, 100),
		row(2, "sonnet", 900, 100),
		row(3, "sonnet", 850, 100),
		row(4, "sonnet", 9000, 2), // 小样本
	}

	got := AssessAccountTTFT(rows, ttftTestConfig(), now)

	// 基线只由三个大样本账号决定 = 850。
	if got[1].Degraded || got[2].Degraded || got[3].Degraded {
		t.Fatal("baseline should not be polluted by the small-sample outlier")
	}
	// 小样本账号本身倍率很高，但样本数不足 MinSamples，不判退化。
	if got[4].Degraded {
		t.Fatalf("small-sample account should not be judged, ratio=%v samples=%d", got[4].Ratio, got[4].Samples)
	}
}

func TestAssessAccountTTFT_WeightsRatioBySampleCount(t *testing.T) {
	now := time.Now()
	// 账号 4 在主力模型上正常（900 次请求），只在一个低频模型上很慢（10 次）。
	// 加权后整体倍率接近 1，不应判退化 —— 但 WorstModel 要把这个问题暴露出来。
	rows := []OpsAccountModelTTFTRow{
		row(1, "sonnet", 800, 500),
		row(2, "sonnet", 900, 500),
		row(3, "sonnet", 850, 500),
		row(4, "sonnet", 860, 900),

		row(1, "haiku", 300, 500),
		row(2, "haiku", 320, 500),
		row(3, "haiku", 310, 500),
		row(4, "haiku", 3100, 10),
	}

	got := AssessAccountTTFT(rows, ttftTestConfig(), now)

	if got[4].Degraded {
		t.Fatalf("account dominated by a healthy model should not be degraded, ratio=%v", got[4].Ratio)
	}
	if got[4].WorstModel != "haiku" {
		t.Fatalf("worst model should surface the slow low-traffic model, got %q", got[4].WorstModel)
	}
	if got[4].WorstRatio < 9 {
		t.Fatalf("worst ratio should be ~10x, got %v", got[4].WorstRatio)
	}
}

func TestAssessAccountTTFT_DegradesWhenDominantModelIsSlow(t *testing.T) {
	now := time.Now()
	// 与上一个用例对称：慢的是主力模型，加权倍率就该主导判定。
	rows := []OpsAccountModelTTFTRow{
		row(1, "sonnet", 800, 500),
		row(2, "sonnet", 900, 500),
		row(3, "sonnet", 850, 500),
		row(4, "sonnet", 2600, 900),

		row(1, "haiku", 300, 500),
		row(2, "haiku", 320, 500),
		row(3, "haiku", 310, 500),
		row(4, "haiku", 305, 10),
	}

	got := AssessAccountTTFT(rows, ttftTestConfig(), now)

	if !got[4].Degraded {
		t.Fatalf("account slow on its dominant model should be degraded, ratio=%v", got[4].Ratio)
	}
}

func TestAssessAccountTTFT_TruncatesModelBreakdown(t *testing.T) {
	now := time.Now()
	cfg := ttftTestConfig()
	cfg.MaxModelsPerAccount = 2

	var rows []OpsAccountModelTTFTRow
	for _, model := range []string{"m1", "m2", "m3", "m4"} {
		// 每个模型都要有足够账号才建得起基线，这里只关心明细截断。
		rows = append(rows,
			row(1, model, 500, 100),
			row(2, model, 500, 100),
			row(3, model, 500, 100),
		)
	}
	// 让账号 1 的模型样本数各不相同，验证按样本数降序保留。
	rows = append(rows, row(1, "m5", 500, 999), row(2, "m5", 500, 100), row(3, "m5", 500, 100))

	got := AssessAccountTTFT(rows, cfg, now)

	if len(got[1].Models) != 2 {
		t.Fatalf("model breakdown should be truncated to 2, got %d", len(got[1].Models))
	}
	if got[1].Models[0].Model != "m5" {
		t.Fatalf("highest-sample model should be kept first, got %q", got[1].Models[0].Model)
	}
}

func TestAssessAccountTTFT_EmptyInput(t *testing.T) {
	if got := AssessAccountTTFT(nil, ttftTestConfig(), time.Now()); got != nil {
		t.Fatalf("expected nil for empty input, got %v", got)
	}
}

func TestMedianFloat64(t *testing.T) {
	cases := []struct {
		name   string
		values []float64
		want   float64
	}{
		{"odd", []float64{3, 1, 2}, 2},
		{"even", []float64{4, 1, 3, 2}, 2.5},
		{"single", []float64{7}, 7},
		{"empty", nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := medianFloat64(tc.values); got != tc.want {
				t.Fatalf("medianFloat64(%v) = %v, want %v", tc.values, got, tc.want)
			}
		})
	}
}
