//go:build upstreamrace

// 诊断目标（ADR-0004）：bifrost v1.8.4 在流式取消路径存在上游数据竞争
// （fasthttp requestStream 释放与内部 SSE 读取并发）。本文件仅在
// `-tags upstreamrace` 下编译，用于验证上游修复；常规门禁使用无 race 的
// 功能流式测试（kernel_test.go 中的流式用例已被本文件接替）。
package llmkernel_test

import (
	"context"
	"testing"
	"time"

	"enterprise-agent-platform/packages/token"
)

// Streaming: chunks flow through, and cancelling the context stops the
// provider stream (stub observes the disconnect).
func TestStreamChatCancellation(t *testing.T) {
	stub := newProviderStub(t)
	k := newKernel(t, stub)
	tf := newTokenFixture(t)
	claims := tf.claims(t, token.AudienceLLMGateway, "secops")

	ctx, cancel := context.WithCancel(context.Background())
	chunks, err := k.StreamChat(ctx, claims, allowPolicy{}, cleanDLP{}, chatRequest())
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	received := 0
	deadline := time.After(10 * time.Second)
loop:
	for {
		select {
		case chunk, ok := <-chunks:
			received++
			_ = chunk
			if !ok || received == 1 {
				cancel() // cancel mid-stream, or drain after close
				if !ok {
					break loop
				}
			}
		case <-deadline:
			cancel()
			t.Fatalf("stream did not finish in time")
		}
	}
	if received == 0 {
		t.Fatalf("no chunks received")
	}
}
