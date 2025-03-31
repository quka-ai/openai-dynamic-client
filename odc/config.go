package odc

import (
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

type ClientConfig struct {
	CacheExpiration   time.Duration
	CacheUpdatePeriod time.Duration
	ScoreWorkers      int
}

func DefaultClientConfig() *ClientConfig {
	return &ClientConfig{
		CacheExpiration:   10 * time.Second,
		CacheUpdatePeriod: 5 * time.Second,
		ScoreWorkers:      runtime.NumCPU(),
	}
}

type PlanWeight struct {
	PlanID string
	Weight float64
}

type LatencyStats struct {
	FirstByteLatency time.Duration
	TotalLatency     time.Duration
	TokenCount       int
	TokensPerSecond  float64
}

type Backend struct {
	Name        string
	BaseURL     string
	APIKey      string
	Weight      float64
	Score       float64
	UseCount    int64
	ErrorRate   float64
	PlanWeights map[string]float64

	// 延迟统计
	AvgFirstByteLatency time.Duration
	AvgTotalLatency     time.Duration
	AvgTokensPerSecond  float64
	StatsCount          int64
	latencyMu           sync.RWMutex
}

type Config struct {
	Backends []*Backend
}

func NewConfig() *Config {
	return &Config{
		Backends: make([]*Backend, 0),
	}
}

func (c *Config) AddBackend(name, baseURL, apiKey string, weight float64) *Backend {
	backend := &Backend{
		Name:        name,
		BaseURL:     baseURL,
		APIKey:      apiKey,
		Weight:      weight,
		PlanWeights: make(map[string]float64),
	}
	c.Backends = append(c.Backends, backend)
	return backend
}

func (b *Backend) SetPlanWeight(planID string, weight float64) {
	b.PlanWeights[planID] = weight
}

func (b *Backend) updateStreamLatencyStats(stats LatencyStats) {
	b.latencyMu.Lock()
	defer b.latencyMu.Unlock()

	count := float64(b.StatsCount)
	newCount := count + 1

	b.AvgFirstByteLatency = time.Duration(
		(float64(b.AvgFirstByteLatency)*count + float64(stats.FirstByteLatency)) / newCount,
	)

	b.AvgTotalLatency = time.Duration(
		(float64(b.AvgTotalLatency)*count + float64(stats.TotalLatency)) / newCount,
	)

	b.AvgTokensPerSecond = (b.AvgTokensPerSecond*count + stats.TokensPerSecond) / newCount

	b.StatsCount++
}

// 添加批量更新统计数据的方法
func (b *Backend) batchUpdateLatencyStats(statsList []LatencyStats) {
	if len(statsList) == 0 {
		return
	}

	b.latencyMu.Lock()
	defer b.latencyMu.Unlock()

	// 计算所有统计数据的总和
	var totalFirstByte, totalLatency, totalTPS float64

	for _, stats := range statsList {
		totalFirstByte += float64(stats.FirstByteLatency)
		totalLatency += float64(stats.TotalLatency)
		totalTPS += stats.TokensPerSecond
	}

	// 更新后端统计数据
	count := float64(b.StatsCount)
	batchSize := float64(len(statsList))
	newCount := count + batchSize

	b.AvgFirstByteLatency = time.Duration(
		(float64(b.AvgFirstByteLatency)*count + totalFirstByte) / newCount,
	)

	b.AvgTotalLatency = time.Duration(
		(float64(b.AvgTotalLatency)*count + totalLatency) / newCount,
	)

	b.AvgTokensPerSecond = (b.AvgTokensPerSecond*count + totalTPS) / newCount

	b.StatsCount += int64(batchSize)
}

// 添加原子更新错误率的方法
func (b *Backend) updateErrorRate() {
	// 使用原子操作获取当前使用次数
	useCount := float64(atomic.LoadInt64(&b.UseCount))

	// 计算新的错误率
	newErrorRate := (b.ErrorRate*(useCount-1) + 1) / useCount

	// 使用CAS操作更新错误率，避免锁竞争
	// 注意：这里简化处理，实际上浮点数的原子更新需要特殊处理
	b.latencyMu.Lock()
	b.ErrorRate = newErrorRate
	b.latencyMu.Unlock()
}
