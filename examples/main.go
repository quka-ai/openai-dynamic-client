package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/quka-ai/openai-dynamic-client/odc"
	"github.com/sashabaranov/go-openai"
)

func main() {
	config := odc.NewConfig()

	// 添加多个backend并配置不同计划的权重
	backend1 := config.AddBackend("backend1", "", "", 1.0)
	backend1.SetPlanWeight("premium", 2.0)
	backend1.SetPlanWeight("basic", 0.5)

	backend2 := config.AddBackend("backend2", "", "", 0.8)
	backend2.SetPlanWeight("basic", 1.5)

	clientConfig := &odc.ClientConfig{
		CacheExpiration:   3 * time.Second,
		CacheUpdatePeriod: 2 * time.Second,
		ScoreWorkers:      4,
	}

	client := odc.NewClient(config, clientConfig)
	defer client.Close()

	// 普通请求示例
	request := openai.ChatCompletionRequest{
		Model: "qwen-plus-latest",
		Messages: []openai.ChatCompletionMessage{
			{
				Role:    openai.ChatMessageRoleUser,
				Content: "Hello!",
			},
		},
	}

	resp, err := client.CreateChatCompletionWithPlan(context.Background(), request, "premium")
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}

	fmt.Println(resp.Choices[0].Message.Content)

	// 流式请求示例
	stream, err := client.CreateChatCompletionStreamWithPlan(context.Background(), request, "basic")
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}

	for {
		response, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			fmt.Printf("Stream error: %v\n", err)
			break
		}

		fmt.Print(response.Choices[0].Delta.Content)
	}
}
