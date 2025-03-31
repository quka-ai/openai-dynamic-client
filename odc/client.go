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

// 在 Client 结构体中添加统计数据收集器
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

	// 统计数据收集
	statsMu      sync.Mutex
	statsBuffers map[string][]LatencyStats
	statsTicker  *time.Ticker
}

// 定义 backendScore 结构体
type backendScore struct {
	backend *Backend
	score   float64
}

// 修改 NewClient 函数
func NewClient(config *Config, clientCfg *ClientConfig) *Client {
	if clientCfg == nil {
		clientCfg = DefaultClientConfig()
	}

	c := &Client{
		config:       config,
		clientCfg:    clientCfg,
		scorer:       NewScorer(),
		clients:      make(map[string]*openai.Client),
		stopChan:     make(chan struct{}),
		statsBuffers: make(map[string][]LatencyStats),
		statsTicker:  time.NewTicker(clientCfg.CacheUpdatePeriod * time.Second), // 每秒更新一次统计数据
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

// 需要先定义 scoreCacheItem 结构体
type scoreCacheItem struct {
	backends []backendScore
	updated  time.Time
}

var (
	BACKEND_SORT_LIMIT = 10
)

func (c *Client) updateScores(planID string) {
	// 使用对象池减少内存分配
	backends := c.scorePool.Get().([]backendScore)
	backends = backends[:0]

	// 减少锁的持有时间，只在读取配置时加锁
	var backendsCopy []*Backend
	c.mu.RLock()
	backendsCopy = make([]*Backend, len(c.config.Backends))
	copy(backendsCopy, c.config.Backends)
	c.mu.RUnlock()

	// 在锁外计算分数
	for _, b := range backendsCopy {
		score := c.scorer.Score(b, planID)
		backends = append(backends, backendScore{
			backend: b,
			score:   score,
		})
	}

	// 只对前N个后端进行排序，而不是全部排序
	// 例如只对前10个或所有后端（取较小值）进行排序
	sortLimit := BACKEND_SORT_LIMIT
	if len(backends) < sortLimit {
		sortLimit = len(backends)
	}

	// 使用部分排序算法，只找出前N个最高分
	// 这里简化为完整排序，实际可以使用更高效的部分排序算法
	sort.Slice(backends, func(i, j int) bool {
		return backends[i].score > backends[j].score
	})

	if oldBackends, exist := c.scoreCache.Load(planID); exist {
		c.scorePool.Put(oldBackends.(*scoreCacheItem).backends)
	}

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

	// 计算总权重
	totalWeight := 0.0
	for i := 0; i < len(backends); i++ {
		totalWeight += backends[i].score
	}

	// 随机选择
	r := rand.Float64() * totalWeight
	cumulativeWeight := 0.0

	for i := 0; i < len(backends); i++ {
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
		// 异步更新错误率
		go backend.updateErrorRate()
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

	// 收集统计数据而不是直接更新
	c.collectStats(backend.Name, LatencyStats{
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
		go backend.updateErrorRate()
		return nil, err
	}

	return &streamWrapper{
		stream:        stream,
		firstByteTime: &firstByteTime,
		tokenCount:    &tokenCount,
		backend:       backend,
		startTime:     start,
		client:        c, // 传入 Client 引用
	}, nil
}

type streamWrapper struct {
	stream        *openai.ChatCompletionStream
	firstByteTime *time.Time
	tokenCount    *int
	backend       *Backend
	startTime     time.Time
	firstByteOnce sync.Once
	client        *Client // 添加对 Client 的引用
}

func (w *streamWrapper) Close() {
	w.stream.Close()
}

// 修改 Recv 方法
func (w *streamWrapper) Recv() (*openai.ChatCompletionStreamResponse, error) {
	resp, err := w.stream.Recv()
	w.firstByteOnce.Do(func() {
		*w.firstByteTime = time.Now()
	})

	if err == nil && len(resp.Choices) > 0 {
		*w.tokenCount += len(resp.Choices[0].Delta.Content)
	} else if err == io.EOF {
		totalTime := time.Since(w.startTime)

		// 获取 Client 实例并收集统计数据
		// 注意：这里需要一种方式让 streamWrapper 访问 Client 实例
		// 可以在创建 streamWrapper 时传入 Client 引用
		go w.client.collectStats(w.backend.Name, LatencyStats{
			FirstByteLatency: w.firstByteTime.Sub(w.startTime),
			TotalLatency:     totalTime,
			TokenCount:       *w.tokenCount,
			TokensPerSecond:  float64(*w.tokenCount) / totalTime.Seconds(),
		})
	}

	return &resp, err
}

// 添加统计数据更新循环
func (c *Client) statsUpdateLoop() {
	defer c.wg.Done()

	for {
		select {
		case <-c.statsTicker.C:
			c.flushStats()
		case <-c.stopChan:
			c.statsTicker.Stop()
			c.flushStats() // 确保关闭前刷新所有统计数据
			return
		}
	}
}

// 添加刷新统计数据的方法
func (c *Client) flushStats() {
	c.statsMu.Lock()
	buffers := c.statsBuffers
	c.statsBuffers = make(map[string][]LatencyStats)
	c.statsMu.Unlock()

	// 批量更新每个后端的统计数据
	for backendName, stats := range buffers {
		if len(stats) == 0 {
			continue
		}

		// 查找对应的后端
		for _, b := range c.config.Backends {
			if b.Name == backendName {
				b.batchUpdateLatencyStats(stats)
				break
			}
		}
	}
}

// 添加收集统计数据的方法
func (c *Client) collectStats(backendName string, stats LatencyStats) {
	c.statsMu.Lock()
	defer c.statsMu.Unlock()

	c.statsBuffers[backendName] = append(c.statsBuffers[backendName], stats)
}
