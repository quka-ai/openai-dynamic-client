package odc

import (
	"context"
	"io"
	"math/rand/v2"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sashabaranov/go-openai"
)

type Client struct {
	config    *Config
	clientCfg *ClientConfig
	scorer    *Scorer
	clients   map[string]*openai.Client
	mu        sync.RWMutex

	scoreCache sync.Map
	scorePool  sync.Pool

	stopChan chan struct{}
	wg       sync.WaitGroup
}

type scoreCacheItem struct {
	backends []backendScore
	updated  time.Time
}

type backendScore struct {
	backend *Backend
	score   float64
}

func NewClient(config *Config, clientCfg *ClientConfig) *Client {
	if clientCfg == nil {
		clientCfg = DefaultClientConfig()
	}

	c := &Client{
		config:    config,
		clientCfg: clientCfg,
		scorer:    NewScorer(),
		clients:   make(map[string]*openai.Client),
		stopChan:  make(chan struct{}),
	}

	c.scorePool = sync.Pool{
		New: func() interface{} {
			return make([]backendScore, 0, len(config.Backends))
		},
	}

	for _, backend := range config.Backends {
		config := openai.DefaultConfig(backend.APIKey)
		config.BaseURL = backend.BaseURL
		c.clients[backend.Name] = openai.NewClientWithConfig(config)
	}

	c.startBackgroundUpdate()

	return c
}

func (c *Client) Close() {
	close(c.stopChan)
	c.wg.Wait()
}

func (c *Client) startBackgroundUpdate() {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		ticker := time.NewTicker(c.clientCfg.CacheUpdatePeriod)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				c.updateAllScores()
			case <-c.stopChan:
				return
			}
		}
	}()
}

func (c *Client) updateAllScores() {
	jobs := make(chan string, len(c.config.Backends))
	results := make(chan struct{}, len(c.config.Backends))

	for i := 0; i < c.clientCfg.ScoreWorkers; i++ {
		go func() {
			for planID := range jobs {
				c.updateScores(planID)
				results <- struct{}{}
			}
		}()
	}

	planIDs := make(map[string]struct{})
	c.scoreCache.Range(func(key, value interface{}) bool {
		planIDs[key.(string)] = struct{}{}
		return true
	})

	for planID := range planIDs {
		jobs <- planID
	}
	close(jobs)

	for i := 0; i < len(planIDs); i++ {
		<-results
	}
}

func (c *Client) updateScores(planID string) {
	backends := c.scorePool.Get().([]backendScore)
	backends = backends[:0]

	c.mu.RLock()
	for _, b := range c.config.Backends {
		score := c.scorer.Score(b, planID)
		backends = append(backends, backendScore{
			backend: b,
			score:   score,
		})
	}
	c.mu.RUnlock()

	sort.Slice(backends, func(i, j int) bool {
		return backends[i].score > backends[j].score
	})

	c.scoreCache.Store(planID, &scoreCacheItem{
		backends: backends,
		updated:  time.Now(),
	})
}

func (c *Client) selectBackend(planID string) *Backend {
	if item, ok := c.scoreCache.Load(planID); ok {
		cacheItem := item.(*scoreCacheItem)
		if time.Since(cacheItem.updated) < c.clientCfg.CacheExpiration {
			// 使用加权随机选择而不是总是选择最高分
			return c.weightedRandomSelect(cacheItem.backends)
		}
	}

	c.updateScores(planID)
	item, _ := c.scoreCache.Load(planID)
	return c.weightedRandomSelect(item.(*scoreCacheItem).backends)
}

// 添加加权随机选择方法
func (c *Client) weightedRandomSelect(backends []backendScore) *Backend {
	// 如果只有一个后端，直接返回
	if len(backends) == 1 {
		return backends[0].backend
	}

	// 只考虑前3个或所有后端（取较小值）
	candidateCount := 3
	if len(backends) < candidateCount {
		candidateCount = len(backends)
	}

	// 计算总权重
	totalWeight := 0.0
	for i := 0; i < candidateCount; i++ {
		totalWeight += backends[i].score
	}

	// 随机选择
	r := rand.Float64() * totalWeight
	cumulativeWeight := 0.0

	for i := 0; i < candidateCount; i++ {
		cumulativeWeight += backends[i].score
		if r <= cumulativeWeight {
			return backends[i].backend
		}
	}

	// 默认返回第一个（评分最高的）
	return backends[0].backend
}

func (c *Client) CreateChatCompletionWithPlan(ctx context.Context, request openai.ChatCompletionRequest, planID string) (*openai.ChatCompletionResponse, error) {
	backend := c.selectBackend(planID)
	atomic.AddInt64(&backend.UseCount, 1)

	start := time.Now()
	client := c.clients[backend.Name]
	resp, err := client.CreateChatCompletion(ctx, request)
	latency := time.Since(start)

	if err != nil {
		c.updateBackendError(backend)
		return nil, err
	}

	// 安全检查：确保有返回结果
	var tokenCount int
	if len(resp.Choices) > 0 && resp.Choices[0].Message.Content != "" {
		tokenCount = len(resp.Choices[0].Message.Content)
	}

	backend.updateStreamLatencyStats(LatencyStats{
		FirstByteLatency: latency,
		TotalLatency:     latency,
		TokenCount:       tokenCount,
		TokensPerSecond:  float64(tokenCount) / latency.Seconds(),
	})

	return &resp, nil
}

// 定义我们自己的流接口
type ChatCompletionStream interface {
	Recv() (*openai.ChatCompletionStreamResponse, error)
	Close()
}

// 修改 CreateChatCompletionStreamWithPlan 方法返回我们的接口
func (c *Client) CreateChatCompletionStreamWithPlan(ctx context.Context, request openai.ChatCompletionRequest, planID string) (ChatCompletionStream, error) {
	backend := c.selectBackend(planID)
	atomic.AddInt64(&backend.UseCount, 1)

	start := time.Now()
	var firstByteTime time.Time
	var tokenCount int

	client := c.clients[backend.Name]
	stream, err := client.CreateChatCompletionStream(ctx, request)

	if err != nil {
		c.updateBackendError(backend)
		return nil, err
	}

	return &streamWrapper{
		stream:        stream,
		firstByteTime: &firstByteTime,
		tokenCount:    &tokenCount,
		backend:       backend,
		startTime:     start,
	}, nil
}

type streamWrapper struct {
	stream        *openai.ChatCompletionStream
	firstByteTime *time.Time
	tokenCount    *int
	backend       *Backend
	startTime     time.Time
	firstByteOnce sync.Once
}

func (w *streamWrapper) Recv() (*openai.ChatCompletionStreamResponse, error) {
	resp, err := w.stream.Recv()

	if err == nil {
		w.firstByteOnce.Do(func() {
			*w.firstByteTime = time.Now()
		})

		if len(resp.Choices) > 0 {
			*w.tokenCount += len(resp.Choices[0].Delta.Content)
		}
	}

	if err == io.EOF {
		totalTime := time.Since(w.startTime)
		w.backend.updateStreamLatencyStats(LatencyStats{
			FirstByteLatency: w.firstByteTime.Sub(w.startTime),
			TotalLatency:     totalTime,
			TokenCount:       *w.tokenCount,
			TokensPerSecond:  float64(*w.tokenCount) / totalTime.Seconds(),
		})
	}

	return &resp, err
}

func (w *streamWrapper) Close() {
	if w.stream != nil {
		w.stream.Close()
	}
}

func (c *Client) updateBackendError(backend *Backend) {
	c.mu.Lock()
	useCount := float64(backend.UseCount)
	backend.ErrorRate = (backend.ErrorRate*(useCount-1) + 1) / useCount
	c.mu.Unlock()
}
