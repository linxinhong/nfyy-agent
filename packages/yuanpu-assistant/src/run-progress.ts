import type { AgentEvent, EntryRecord, SnapshotEvent } from '@earendil-works/pi-durable';
import { displayToolValue, toolResultText, type RunProgress, type RunToolStep } from '@yuanpu-agent/protocol';

/** Projects only this foreground turn; private reasoning and background sessions never enter the view. */
export class AssistantRunProgress {
  private readonly boundary: number;
  private readonly steps = new Map<string, RunToolStep>();
  private readonly texts = new Map<number, string>();
  private blocks = new Map<number, string>();
  private truncated = false;

  constructor(snapshot: SnapshotEvent) {
    this.boundary = snapshot.entries.reduce((max, entry) => Math.max(max, entry.id), 0);
  }

  private start(id: string, name: string, args?: unknown, at = new Date().toISOString()) {
    if (this.steps.has(id)) return;
    if (this.steps.size >= 200) { this.truncated = true; return; }
    const value = displayToolValue(args);
    this.steps.set(id, { id, name, status: 'running', startedAt: at,
      arguments: value.text, truncated: value.truncated });
  }

  private entry(entry: EntryRecord) {
    if (entry.id <= this.boundary) return;
    for (const message of entry.model ?? []) {
      const at = new Date(message.timestamp).toISOString();
      if (message.role === 'assistant') {
        this.texts.set(entry.id, toolResultText(message.content).slice(0, 64_000));
        this.blocks.clear();
        for (const block of message.content) if (block.type === 'toolCall') {
          this.start(block.id, block.name, block.arguments, at);
        }
      } else if (message.role === 'toolResult') {
        this.start(message.toolCallId, message.toolName, undefined, at);
        const step = this.steps.get(message.toolCallId);
        if (step) {
          const value = displayToolValue(toolResultText(message.content));
          Object.assign(step, { status: message.isError ? 'failed' : 'completed',
            result: value.text, finishedAt: at, truncated: step.truncated || value.truncated });
        }
      }
    }
  }

  apply(event: AgentEvent) {
    if (event.type === 'snapshot') {
      for (const entry of event.entries) this.entry(entry);
      this.blocks.clear();
      event.generation?.message?.content.forEach((block, index) => {
        if (block.type === 'text') this.blocks.set(index, block.text);
      });
      for (const slot of event.tools) {
        const step = this.steps.get(slot.callId);
        if (step && slot.status === 'done' && !slot.entry) step.status = 'interrupted';
      }
    } else if (event.type === 'message_start' && event.message.role === 'assistant') {
      this.blocks.clear();
      event.message.content.forEach((block, index) => { if (block.type === 'text') this.blocks.set(index, block.text); });
    } else if (event.type === 'message_update') {
      for (const change of event.changes) {
        if (change.type === 'text_delta') this.blocks.set(change.contentIndex,
          ((this.blocks.get(change.contentIndex) ?? '') + change.delta).slice(0, 64_000));
        else if ((change.type === 'text_start' || change.type === 'block') && change.block.type === 'text') {
          this.blocks.set(change.contentIndex, change.block.text);
        } else if (change.type === 'message') {
          this.blocks.clear();
          change.message.content.forEach((block, index) => { if (block.type === 'text') this.blocks.set(index, block.text); });
        }
      }
    } else if (event.type === 'message_end' || event.type === 'entry_appended') this.entry(event.entry);
    else if (event.type === 'tool_execution_start') this.start(event.toolCallId, event.toolName, event.args);
    else if (event.type === 'tool_execution_end') {
      if (event.entry) this.entry(event.entry);
      else {
        const step = this.steps.get(event.toolCallId);
        if (step) step.status = 'interrupted';
      }
    }
  }

  value(): RunProgress {
    const text = [...this.texts.entries()].sort(([a], [b]) => a - b).map(([, value]) => value)
      .concat([...this.blocks.entries()].sort(([a], [b]) => a - b).map(([, value]) => value).join('')).filter(Boolean).join('\n\n');
    return { text: text.slice(0, 64_000), steps: [...this.steps.values()].map((step) => ({ ...step })),
      truncated: this.truncated || text.length > 64_000 };
  }
}
