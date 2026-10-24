package models

import (
	"context"
)

// reasoningEffortKeyType 是思考强度（reasoning effort）的 context key。
// 经 WithReasoningEffort 注入后，OpenAI 兼容 provider 会在请求体携带
// reasoning_effort 与 thinking.enabled（DeepSeek 思考模式契约）。
type reasoningEffortKeyType struct{}

// 支持的思考强度取值（DeepSeek 侧映射：low→low，high→high，max→max）。
const (
	ReasoningEffortLow  = "low"
	ReasoningEffortHigh = "high"
	ReasoningEffortMax  = "max"
)

// ValidReasoningEffort 报告 effort 是否为受支持的取值。
func ValidReasoningEffort(effort string) bool {
	switch effort {
	case ReasoningEffortLow, ReasoningEffortHigh, ReasoningEffortMax:
		return true
	default:
		return false
	}
}

// WithReasoningEffort 返回携带思考强度的 context；空值或非法值等价于不设置。
func WithReasoningEffort(ctx context.Context, effort string) context.Context {
	if !ValidReasoningEffort(effort) {
		return ctx
	}
	return context.WithValue(ctx, reasoningEffortKeyType{}, effort)
}

// ReasoningEffortFrom 读取 context 中的思考强度；未设置或非法时返回空串。
func ReasoningEffortFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	effort, _ := ctx.Value(reasoningEffortKeyType{}).(string)
	if !ValidReasoningEffort(effort) {
		return ""
	}
	return effort
}
