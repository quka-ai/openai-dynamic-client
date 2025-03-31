package odc

import (
	"runtime"
	"sync"
	"time"
)

type ClientConfig struct {
	CacheExpiration   time.Duration
	CacheUpdatePeriod time.Duration
	ScoreWorkers      int
}

func DefaultClientConfig() *ClientConfig {
	return &ClientConfig{
		CacheExpiration:   5 * time.Second,
		CacheUpdatePeriod: 3 * time.Second,
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
