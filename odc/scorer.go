package odc

import (
	"math"
	"sync"
	"time"
)

type Scorer struct {
	mu sync.RWMutex
}

func NewScorer() *Scorer {
	return &Scorer{}
}

func (s *Scorer) Score(backend *Backend, planID string) float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// 基础得分
	score := backend.Weight
	if planWeight, exists := backend.PlanWeights[planID]; exists {
		score = planWeight
	}

	// 错误率惩罚 - 保持较强的惩罚
	errorPenalty := math.Pow(1-backend.ErrorRate, 3)
	score *= errorPenalty

	// 如果有性能统计数据
	if backend.StatsCount > 0 {
		// 首字节延迟得分 - 调整期望值和曲线斜率
		expectedFirstByte := 300 * time.Millisecond // 从500ms调整为300ms，提高对延迟的要求
		firstByteScore := 1.0 / (1.0 + math.Exp(
			2*(float64(backend.AvgFirstByteLatency)/float64(expectedFirstByte)-1), // 增加斜率
		))

		// 吞吐量得分 - 调整期望值和评分曲线
		expectedTPS := 25.0 // 从20提高到25
		tpsScore := math.Min(
			backend.AvgTokensPerSecond/expectedTPS,
			1.5,
		) / 1.5

		// 增加延迟和吞吐量的整体影响
		latencyScore := 0.5 + 0.5*(firstByteScore*0.7+tpsScore*0.3) // 从0.2提高到0.3
		score *= latencyScore
	}

	// 负载均衡因子 - 保持较低影响
	usageBalance := 1.0 / math.Log10(float64(backend.UseCount)+10.0)
	score *= (0.7 + 0.3*usageBalance)

	return score
}
