package anthropic_test

import (
	"testing"

	"enterprise-agent-platform/internal/golden"
	"enterprise-agent-platform/packages/protocol-anthropic"
)

const fixtureDir = "../../contracts/schemas/v1/anthropic"

func TestMessagesRequestFixture(t *testing.T) {
	var req anthropic.MessagesRequest
	golden.Unmarshal(t, fixtureDir+"/messages.request.json", &req)
	if req.MaxTokens != 1024 || len(req.Messages) != 1 {
		t.Fatalf("request decoded wrong: %+v", req)
	}
	golden.Check(t, fixtureDir+"/messages.request.json", req)
}

func TestMessagesResponseFixture(t *testing.T) {
	var resp anthropic.MessageResponse
	golden.Unmarshal(t, fixtureDir+"/messages.response.json", &resp)
	if resp.Type != "message" || resp.StopReason != "end_turn" {
		t.Fatalf("response decoded wrong: %+v", resp)
	}
	if resp.StopSequence != nil {
		t.Fatalf("stop_sequence must decode null")
	}
	golden.Check(t, fixtureDir+"/messages.response.json", resp)
}

func TestMessagesStreamEventFixture(t *testing.T) {
	var ev anthropic.StreamEvent
	golden.Unmarshal(t, fixtureDir+"/messages.stream-event.json", &ev)
	if ev.Type != "content_block_delta" || ev.Delta == nil || ev.Delta.Type != "text_delta" {
		t.Fatalf("stream event decoded wrong: %+v", ev)
	}
	golden.Check(t, fixtureDir+"/messages.stream-event.json", ev)
}

func TestContractVersionFrozen(t *testing.T) {
	if anthropic.ContractVersion != "v1" {
		t.Fatalf("contract version changed: %s", anthropic.ContractVersion)
	}
}
