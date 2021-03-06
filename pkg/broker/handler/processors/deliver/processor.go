/*
Copyright 2020 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package deliver

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/cloudevents/sdk-go/v2/binding"
	ceclient "github.com/cloudevents/sdk-go/v2/client"
	cecontext "github.com/cloudevents/sdk-go/v2/context"
	"github.com/cloudevents/sdk-go/v2/event"
	cehttp "github.com/cloudevents/sdk-go/v2/protocol/http"
	"go.opencensus.io/trace"
	"go.uber.org/zap"
	"knative.dev/eventing/pkg/logging"

	"github.com/google/knative-gcp/pkg/broker/config"
	"github.com/google/knative-gcp/pkg/broker/eventutil"
	handlerctx "github.com/google/knative-gcp/pkg/broker/handler/context"
	"github.com/google/knative-gcp/pkg/broker/handler/processors"
	"github.com/google/knative-gcp/pkg/metrics"
)

const defaultEventHopsLimit int32 = 255

// Processor delivers events based on the broker/target in the context.
type Processor struct {
	processors.BaseProcessor

	// DeliverClient is the cloudevents client to send events.
	DeliverClient *http.Client

	// Targets is the targets from config.
	Targets config.ReadonlyTargets

	// RetryOnFailure if set to true, the processor will send the event
	// to the retry topic if the delivery fails.
	RetryOnFailure bool

	// DeliverRetryClient is the cloudevents client to send events
	// to the retry topic.
	DeliverRetryClient ceclient.Client

	// DeliverTimeout is the timeout applied to cancel delivery.
	// If zero, not additional timeout is applied.
	DeliverTimeout time.Duration

	// StatsReporter is used to report delivery metrics.
	StatsReporter *metrics.DeliveryReporter
}

var _ processors.Interface = (*Processor)(nil)

// Process delivers the event based on the broker/target in the context.
func (p *Processor) Process(ctx context.Context, event *event.Event) error {
	bk, err := handlerctx.GetBrokerKey(ctx)
	if err != nil {
		return err
	}
	tk, err := handlerctx.GetTargetKey(ctx)
	if err != nil {
		return err
	}
	broker, ok := p.Targets.GetBrokerByKey(bk)
	if !ok {
		// If the broker no longer exists, then there is nothing to process.
		logging.FromContext(ctx).Warn("broker no longer exist in the config", zap.String("broker", bk))
		trace.FromContext(ctx).Annotate(
			ceclient.EventTraceAttributes(event),
			"event dropped: broker config no longer exists",
		)
		return nil
	}
	target, ok := p.Targets.GetTargetByKey(tk)
	if !ok {
		// If the target no longer exists, then there is nothing to process.
		logging.FromContext(ctx).Warn("target no longer exist in the config", zap.String("target", tk))
		trace.FromContext(ctx).Annotate(
			ceclient.EventTraceAttributes(event),
			"event dropped: trigger config no longer exists",
		)
		return nil
	}

	// Hops is a broker local counter so remove any hops value before forwarding.
	// Do not modify the original event as we need to send the original
	// event to retry queue on failure.
	copy := event.Clone()
	// This will decrement the remaining hops if there is an existing value.
	eventutil.UpdateRemainingHops(ctx, &copy, defaultEventHopsLimit)
	hops, _ := eventutil.GetRemainingHops(ctx, &copy)
	eventutil.DeleteRemainingHops(ctx, &copy)

	p.StatsReporter.FinishEventProcessing(ctx)

	dctx := ctx
	if p.DeliverTimeout > 0 {
		var cancel context.CancelFunc
		dctx, cancel = context.WithTimeout(dctx, p.DeliverTimeout)
		defer cancel()
	}

	// Forward the event copy that has hops removed.
	if err := p.deliver(dctx, target, broker, (*binding.EventMessage)(&copy), hops); err != nil {
		if !p.RetryOnFailure {
			return err
		}

		logging.FromContext(ctx).Warn("target delivery failed", zap.String("target", tk), zap.Error(err))
		trace.FromContext(ctx).Annotate(
			[]trace.Attribute{trace.StringAttribute("error_message", err.Error())},
			"enqueueing for retry",
		)
		return p.sendToRetryTopic(ctx, target, event)
	}
	// For post-delivery processing.
	return p.Next().Process(ctx, event)
}

// deliver delivers msg to target and sends the target's reply to the broker ingress.
func (p *Processor) deliver(ctx context.Context, target *config.Target, broker *config.Broker, msg binding.Message, hops int32) error {
	startTime := time.Now()
	resp, err := p.sendMsg(ctx, target.Address, msg)
	if err != nil {
		return err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			logging.FromContext(ctx).Warn("failed to close response body", zap.Error(err))
		}
	}()

	p.StatsReporter.ReportEventDispatchTime(ctx, time.Since(startTime), resp.StatusCode)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("event delivery failed: HTTP status code %d", resp.StatusCode)
	}

	respMsg := cehttp.NewMessageFromHttpResponse(resp)
	if respMsg.ReadEncoding() == binding.EncodingUnknown {
		// No reply
		return nil
	}

	if span := trace.FromContext(ctx); span.IsRecordingEvents() {
		span.Annotate([]trace.Attribute{
			trace.StringAttribute("cloudevents.encoding", respMsg.ReadEncoding().String()),
		}, "event reply received")
	}

	if hops <= 0 {
		e, err := binding.ToEvent(ctx, respMsg)
		if err != nil {
			logging.FromContext(ctx).Error("failed to convert response message to event",
				zap.Error(err),
				zap.Any("response", respMsg),
			)
			return nil
		}
		logging.FromContext(ctx).Warn("event has exhausted allowed hops: dropping reply",
			zap.String("target", target.Name),
			zap.Int32("hops", hops),
			zap.Any("event context", e.Context),
		)
		if span := trace.FromContext(ctx); span.IsRecordingEvents() {
			span.Annotate(
				append(
					ceclient.EventTraceAttributes(e),
					trace.Int64Attribute("remaining_hops", int64(hops)),
				),
				"Event reply dropped due to hop limit",
			)
		}
		return nil
	}

	// Attach the previous hops for the reply.
	replyResp, err := p.sendMsg(ctx, broker.Address, respMsg, eventutil.SetRemainingHopsTransformer(hops))
	if err != nil {
		return err
	}
	if err := replyResp.Body.Close(); err != nil {
		logging.FromContext(ctx).Warn("failed to close reply response body", zap.Error(err))
	}
	return nil
}

func (p *Processor) sendMsg(ctx context.Context, address string, msg binding.Message, transformers ...binding.Transformer) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, address, nil)
	if err != nil {
		return nil, err
	}
	if err := cehttp.WriteRequest(ctx, msg, req, transformers...); err != nil {
		return nil, err
	}
	return p.DeliverClient.Do(req)
}

func (p *Processor) sendToRetryTopic(ctx context.Context, target *config.Target, event *event.Event) error {
	pctx := cecontext.WithTopic(ctx, target.RetryQueue.Topic)
	if err := p.DeliverRetryClient.Send(pctx, *event); err != nil {
		return fmt.Errorf("failed to send event to retry topic: %w", err)
	}
	return nil
}
