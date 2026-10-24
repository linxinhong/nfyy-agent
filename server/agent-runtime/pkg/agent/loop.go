package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/linxinhong/xingding-agent-runtime/internal/streamtransport"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
)

var (
	// ErrMaxToolRounds reports that the model kept requesting tools past
	// AgentConfig.MaxToolCalls rounds.
	ErrMaxToolRounds = errors.New("agent: exceeded maximum tool-call rounds")
	// ErrOutputTruncated reports that the model stopped at its output token
	// limit. RunOutput.Content keeps the partial text; tool-call arguments
	// from a truncated turn are never executed.
	ErrOutputTruncated = errors.New("agent: model output truncated at max tokens")
	// ErrContentFiltered reports that the provider withheld or cut output.
	ErrContentFiltered = errors.New("agent: model output blocked by content filter")
)

// emitter forwards loop events to a stream writer. The zero value, used by
// synchronous runs, discards them, so one loop serves both modes.
type emitter struct {
	w *streamtransport.Writer[Event]
}

func (e emitter) streaming() bool { return e.w != nil }

func (e emitter) emit(event Event) {
	if e.w != nil {
		emitAgentEvent(e.w, event)
	}
}

type emitterContextKey struct{}

// contextWithEmitter lets tool execution, which is shared with the durable
// resume path, report per-tool progress without threading the writer
// through every signature.
func contextWithEmitter(ctx context.Context, em emitter) context.Context {
	if !em.streaming() {
		return ctx
	}
	return context.WithValue(ctx, emitterContextKey{}, em)
}

func emitterFromContext(ctx context.Context) emitter {
	em, _ := ctx.Value(emitterContextKey{}).(emitter)
	return em
}

// runState is the mutable state of one run across model rounds.
type runState struct {
	sess   *session.AgentSession
	output *session.RunOutput
	// messages is the full model context; runMessages is the durable
	// transcript of this run (persisted as RunOutput.Messages).
	messages    []models.Message
	runMessages []models.Message
	toolMap     map[string]tools.Tool
	opts        models.ModelOptions
	// cancel cancels the run context with a cause (see guardRun).
	cancel context.CancelCauseFunc
}

// start executes a new run. em is the zero emitter for synchronous runs.
func (a *Agent) start(ctx context.Context, input, requestedRunID, parentRunID, nodeID string, metadata map[string]string, em emitter) (*session.RunOutput, error) {
	runStartedAt := time.Now().UTC()
	em.emit(Event{Type: EventTypeRunStarted})

	// Serialize runs on an existing session: the history read below and the
	// transcript written at the end must not interleave with another run.
	ctx, unlock, err := a.lockSession(ctx, a.cfg.SessionID)
	if err != nil {
		em.emit(Event{Type: EventTypeRunError, Err: err})
		return nil, err
	}
	defer unlock()

	sess, err := a.loadOrCreateSessionWithState(ctx)
	if err != nil {
		err = fmt.Errorf("load session: %w", err)
		em.emit(Event{Type: EventTypeRunError, Err: err})
		return nil, err
	}

	runID := requestedRunID
	if runID == "" {
		runID = uuid.NewString()
	}
	output := &session.RunOutput{
		RunID:       runID,
		AgentID:     a.ID(),
		ParentRunID: parentRunID,
		NodeID:      nodeID,
		SessionID:   sess.SessionID,
		UserID:      a.cfg.UserID,
		Metadata:    metadata,
		Input:       session.RunInput{Message: input},
		Status:      session.RunStatusRunning,
		CreatedAt:   runStartedAt,
		UpdatedAt:   runStartedAt,
	}
	if err := applyTrustedRunIdentity(ctx, output); err != nil {
		em.emit(Event{Type: EventTypeRunError, Err: err, RunOutput: output})
		return output, err
	}
	if store, timeout := a.leaseStore(ctx, output); store != nil {
		output.LeaseTimeout = timeout
	}
	if err := a.persistRun(ctx, sess, output); err != nil {
		err = fmt.Errorf("start run: %w", err)
		em.emit(Event{Type: EventTypeRunError, Err: err})
		return output, err
	}
	ctx = contextWithMetrics(ctx, &output.Metrics)

	st := &runState{sess: sess, output: output}
	ctx, release := a.guardRun(ctx, st)
	defer release()
	if err := a.prepareRun(ctx, st); err != nil {
		return a.finishRun(ctx, st, em, err)
	}
	return a.modelLoop(ctx, st, 0, em)
}

// prepareRun runs pre-run hooks and assembles the initial context.
func (a *Agent) prepareRun(ctx context.Context, st *runState) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := a.runPreRunHooks(ctx, &st.output.Input); err != nil {
		return err
	}
	// Pre-run hooks may rewrite the input; build context from the result.
	messages, runMessages, err := a.buildRunMessages(ctx, st.sess, st.output.Input.Message)
	if err != nil {
		return err
	}
	st.messages, st.runMessages = messages, runMessages
	return a.prepareModelOptions(st)
}

// prepareModelOptions resolves tools and per-call model options.
func (a *Agent) prepareModelOptions(st *runState) error {
	toolMap, modelTools, err := a.resolveTools()
	if err != nil {
		return err
	}
	st.toolMap = toolMap
	st.opts = models.ModelOptions{
		Temperature: a.cfg.Temperature,
		MaxTokens:   a.cfg.MaxTokens,
		Tools:       modelTools,
		ToolChoice:  a.cfg.ToolChoice,
	}
	if a.cfg.OutputSchema != nil {
		format, err := a.responseFormatForModel(a.cfg.OutputSchema)
		if err != nil {
			return err
		}
		st.opts.ResponseFormat = format
	}
	return nil
}

// modelLoop is the single model/tool loop shared by Run, RunStream, Resume,
// and ResumeStream. firstRound is 0 for new runs and ModelRound+1 when a
// paused run continues.
func (a *Agent) modelLoop(ctx context.Context, st *runState, firstRound int, em emitter) (*session.RunOutput, error) {
	output := st.output
	for round := firstRound; round <= a.cfg.MaxToolCalls; round++ {
		if round > firstRound {
			messages, err := a.applyPerModelCallContextProviders(ctx, st.messages, len(st.runMessages))
			if err != nil {
				return a.finishRun(ctx, st, em, err)
			}
			st.messages = messages
		}
		if err := ctx.Err(); err != nil {
			return a.finishRun(ctx, st, em, err)
		}
		if err := a.admitModelCall(ctx, st); err != nil {
			return a.finishRun(ctx, st, em, err)
		}

		turn, err := a.modelTurn(ctx, st, em)
		if err != nil {
			return a.finishRun(ctx, st, em, err)
		}
		message := turn.message
		st.messages = append(st.messages, message)
		st.runMessages = append(st.runMessages, message)
		output.ToolCalls = append(output.ToolCalls, message.ToolCalls...)
		output.ModelRound = round

		if err := finishReasonError(turn.finish); err != nil {
			output.Content = message.Content
			return a.finishRun(ctx, st, em, err)
		}
		if len(message.ToolCalls) == 0 {
			output.Content = message.Content
			parsed, err := a.parseStructuredOutput(message.Content, turn.parsed)
			if err != nil {
				return a.finishRun(ctx, st, em, err)
			}
			output.Parsed = parsed
			output.Status = session.RunStatusCompleted
			return a.finishRun(ctx, st, em, nil)
		}
		if round == a.cfg.MaxToolCalls {
			break
		}
		// Write-ahead: record the tool calls before any side effect, so a
		// crash leaves them visible to Recover as calls of unknown outcome.
		if err := a.checkpoint(ctx, st); err != nil {
			return a.finishRun(ctx, st, em, err)
		}

		batch, err := a.processToolBatch(contextWithEmitter(ctx, em), st.sess, output, st.toolMap, message.ToolCalls, st.messages, st.runMessages, round)
		if err != nil {
			return a.finishRun(ctx, st, em, err)
		}
		if batch.paused {
			em.emit(Event{Type: EventTypeRunPaused, RunOutput: output, Requirement: output.ActiveRequirement()})
			return output, nil
		}
		st.messages, st.runMessages = batch.messages, batch.runMessages
		if err := a.checkpoint(ctx, st); err != nil {
			return a.finishRun(ctx, st, em, err)
		}
	}
	return a.finishRun(ctx, st, em, fmt.Errorf("%w (%d)", ErrMaxToolRounds, a.cfg.MaxToolCalls))
}

// finishRun settles a terminal run exactly once: status, transcript,
// post-run hooks, durable persistence, then a single terminal event.
func (a *Agent) finishRun(ctx context.Context, st *runState, em emitter, runErr error) (*session.RunOutput, error) {
	output := st.output
	if runErr != nil {
		if ctx.Err() != nil {
			output.Status = session.RunStatusCancelled
			runErr = context.Cause(ctx)
		} else {
			output.Status = session.RunStatusError
		}
		output.Error = runErr.Error()
	}
	if st.runMessages != nil {
		output.Messages = st.runMessages
	}
	output.ActiveRequirementID = ""

	if err := a.runPostRunHooks(ctx, output); err != nil {
		output.Status = session.RunStatusError
		output.Error = err.Error()
		runErr = errors.Join(runErr, err)
	}
	if err := a.updateRun(ctx, output); err != nil {
		runErr = errors.Join(runErr, fmt.Errorf("save session: %w", err))
	}

	recordRunFinished(ctx, output)
	if runErr != nil {
		em.emit(Event{Type: EventTypeRunError, Err: runErr})
	} else {
		em.emit(Event{Type: EventTypeRunCompleted, RunOutput: output})
	}
	return output, runErr
}

func finishReasonError(reason models.FinishReason) error {
	switch reason {
	case models.FinishReasonLength:
		return ErrOutputTruncated
	case models.FinishReasonContentFilter:
		return ErrContentFiltered
	default:
		return nil
	}
}

// modelTurn is the outcome of one model call.
type modelTurn struct {
	message models.Message
	parsed  any
	finish  models.FinishReason
}

// modelTurn performs one model call: synchronous for sync runs, streamed
// with live deltas otherwise. Usage is recorded even for failed streams.
func (a *Agent) modelTurn(ctx context.Context, st *runState, em emitter) (modelTurn, error) {
	served := &servedModel{}
	ctx = context.WithValue(ctx, servedModelKey{}, served)
	if !em.streaming() {
		resp, err := a.wrappedCallModel(ctx, st.messages, st.opts)
		if err != nil {
			return modelTurn{}, err
		}
		addUsage(&st.output.Metrics, resp.Usage)
		a.recordModelUsage(ctx, st, served.model, resp.Usage)
		a.calibrate(ctx, served.model, st.messages, resp.Usage)
		return modelTurn{
			message: models.Message{
				Role:             models.RoleAssistant,
				Content:          resp.Content,
				ToolCalls:        resp.ToolCalls,
				ReasoningContent: resp.ReasoningContent,
			},
			parsed: resp.Parsed,
			finish: resp.FinishReason,
		}, nil
	}

	// A stream that fails after connecting but before any delta reached the
	// client is retried transparently; once output was forwarded the
	// failure is surfaced, since the client already rendered partial text.
	for retries := 0; ; retries++ {
		turn, established, forwarded, err := a.streamTurn(ctx, st, em)
		if err == nil {
			return turn, nil
		}
		if !established || forwarded || ctx.Err() != nil ||
			retries >= a.cfg.RetryConfig.MaxRetries || !models.IsRetryable(err) {
			return modelTurn{}, err
		}
		if waitErr := a.waitRetry(ctx, retries, err); waitErr != nil {
			return modelTurn{}, waitErr
		}
	}
}

// streamTurn consumes one model stream, forwarding deltas as they arrive.
func (a *Agent) streamTurn(ctx context.Context, st *runState, em emitter) (turn modelTurn, established, forwarded bool, err error) {
	stream, err := a.wrappedCallModelStream(ctx, st.messages, st.opts)
	if err != nil {
		return modelTurn{}, false, false, err
	}
	agg := newStreamAggregator()
	var usage *models.MessageMetrics
	finish := models.FinishReasonUnknown
	var streamErr error
	for ev := range stream {
		if ev.Err != nil || ev.Event == "error" {
			streamErr = ev.Err
			if streamErr == nil {
				streamErr = errors.New("agent: model stream reported an error")
			}
			break
		}
		switch ev.Event {
		case "content_delta":
			if ev.Content == "" {
				continue
			}
			agg.addContent(ev.Content)
			em.emit(Event{Type: EventTypeMessage, Content: ev.Content})
			forwarded = true
		case "tool_call_delta":
			agg.addToolCallDelta(ev.ToolCall)
			em.emit(Event{Type: EventTypeToolCall, ToolCall: ev.ToolCall})
			forwarded = true
		case "reasoning_delta":
			if ev.ReasoningContent == "" {
				continue
			}
			agg.addReasoning(ev.ReasoningContent)
			em.emit(Event{Type: EventTypeReasoning, Content: ev.ReasoningContent})
			forwarded = true
		case "usage":
			if ev.Usage != nil {
				copied := *ev.Usage
				usage = &copied
			}
		case "finish":
			finish = ev.FinishReason
		}
	}
	addUsage(&st.output.Metrics, usage)
	if served, ok := ctx.Value(servedModelKey{}).(*servedModel); ok {
		a.recordModelUsage(ctx, st, served.model, usage)
		a.calibrate(ctx, served.model, st.messages, usage)
	}
	if streamErr != nil {
		// Unblock a producer that does not watch ctx (custom models,
		// middleware) so it cannot leak once we stop reading.
		go func() {
			for range stream {
			}
		}()
		return modelTurn{}, true, forwarded, streamErr
	}
	if err := ctx.Err(); err != nil {
		return modelTurn{}, true, forwarded, err
	}
	return modelTurn{message: agg.message(), finish: finish}, true, forwarded, nil
}
