package odc

import (
	"fmt"
	"math"
	"testing"
	"time"
)

func TestScorerWithMultipleBackends(t *testing.T) {
	// 创建多个后端，模拟真实场景
	backends := []*Backend{
		{
			Name:                "backend-1",
			Weight:              1.0,
			ErrorRate:           0.01, // 1%错误率
			AvgFirstByteLatency: 300 * time.Millisecond,
			AvgTokensPerSecond:  25.0,
			StatsCount:          100,
			UseCount:            10,
		},
		{
			Name:                "backend-2",
			Weight:              1.0,
			ErrorRate:           0.05, // 5%错误率
			AvgFirstByteLatency: 200 * time.Millisecond,
			AvgTokensPerSecond:  30.0,
			StatsCount:          100,
			UseCount:            10,
		},
		{
			Name:                "backend-3",
			Weight:              1.0,
			ErrorRate:           0.02, // 2%错误率
			AvgFirstByteLatency: 400 * time.Millisecond,
			AvgTokensPerSecond:  22.0,
			StatsCount:          100,
			UseCount:            10,
		},
	}

	scorer := NewScorer()
	planID := "default"

	fmt.Println("初始评分情况:")
	fmt.Println("后端\t错误率\t首字节延迟\t吞吐量\t使用次数\t评分")
	fmt.Println("------------------------------------------------------")

	// 打印初始评分
	for _, b := range backends {
		score := scorer.Score(b, planID)
		fmt.Printf("%s\t%.2f%%\t%dms\t%.1f\t%d\t%.4f\n",
			b.Name,
			b.ErrorRate*100,
			b.AvgFirstByteLatency/time.Millisecond,
			b.AvgTokensPerSecond,
			b.UseCount,
			score)
	}

	// 模拟请求分配
	fmt.Println("\n模拟200次请求后的评分变化:")
	client := NewClient(&Config{
		Backends: backends,
	}, DefaultClientConfig())
	// 根据评分选择后端并更新使用次数
	for i := 0; i < 200; i++ {
		// 找出评分最高的后端
		var bestBackend *Backend
		var bestScore float64
		time.Sleep(100 * time.Millisecond)
		b := client.selectBackend(planID)

		score := scorer.Score(b, planID)
		if score > bestScore {
			bestScore = score
			bestBackend = b
		}

		// 增加使用次数
		bestBackend.UseCount++
	}

	// 打印最终评分
	fmt.Println("后端\t错误率\t首字节延迟\t吞吐量\t使用次数\t评分")
	fmt.Println("------------------------------------------------------")
	for _, b := range backends {
		score := scorer.Score(b, planID)
		fmt.Printf("%s\t%.2f%%\t%dms\t%.1f\t%d\t%.4f\n",
			b.Name,
			b.ErrorRate*100,
			b.AvgFirstByteLatency/time.Millisecond,
			b.AvgTokensPerSecond,
			b.UseCount,
			score)
	}
}

func TestErrorPenalty(t *testing.T) {
	backends := []*Backend{
		{
			Name:      "backend-1",
			Weight:    1.0,
			ErrorRate: 0.01, // 1%错误率
		},
		{
			Name:      "backend-2",
			Weight:    1.0,
			ErrorRate: 0.05, // 5%错误率
		},
		{
			Name:      "backend-3",
			Weight:    1.0,
			ErrorRate: 0.12, // 2%错误率
		},
	}

	for _, b := range backends {
		// 计算错误惩罚
		errorPenalty := math.Pow(1-b.ErrorRate*1.2, 3)
		// 计算最终得分
		score := b.Weight * errorPenalty
		// 打印结果
		fmt.Printf("Backend: %s, Penalty: %.4f\n Score: %.4f\n", b.Name, errorPenalty, score)
	}
}
