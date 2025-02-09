// Copyright 2024 The Cockroach Authors.
//
// Licensed as a CockroachDB Enterprise file under the Cockroach Community
// License (the "License"); you may not use this file except in compliance with
// the License. You may obtain a copy of the License at
//
//     https://github.com/cockroachdb/cockroach/blob/master/licenses/CCL.txt

package logical

import (
	"context"
	"time"

	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/repstream/streampb"
	"github.com/cockroachdb/cockroach/pkg/settings"
	"github.com/cockroachdb/cockroach/pkg/util/metric"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
)

var retryQueueAgeLimit = settings.RegisterDurationSetting(
	settings.ApplicationLevel,
	"logical_replication.consumer.retry_queue_duration",
	"maximum time an incoming update can be retried before it is sent to the DLQ",
	time.Minute,
)

var retryQueueBackoff = settings.RegisterDurationSetting(
	settings.ApplicationLevel,
	"logical_replication.consumer.retry_queue_backoff",
	"minimum delay between retries of items in the retry queue",
	time.Minute,
)

var retryQueueSizeLimit = settings.RegisterByteSizeSetting(
	settings.ApplicationLevel,
	"logical_replication.consumer.retry_queue_partition_size_limit",
	"byte size of retry queue per partition of replication stream above which events are sent to DLQ on failure to apply",
	4<<20,
)

// purgatory is an ordered list of purgatory levels, each consisting of some
// number of events that need to be durably processed to finish that level and
// an optional checkpoint that can be applied when it is fully processed. If
// purgatory is non-empty, incoming checkpoints must go to purgatory instead
// of being emitted, and will be emitted when the purgatory level is processed
// instead.
type purgatory struct {
	// configuration provided at construction.
	delay      func() time.Duration // delay to wait between attempts of a level.
	deadline   func() time.Duration // age of a level after which drain is mandatory.
	byteLimit  func() int64
	flush      func(context.Context, []streampb.StreamEvent_KV, bool, retryEligibility) ([]streampb.StreamEvent_KV, int64, error)
	checkpoint func(context.Context, []jobspb.ResolvedSpan) error

	// internally managed state.
	bytes                   int64
	levels                  []purgatoryLevel
	eventsGauge, bytesGauge *metric.Gauge
}

type purgatoryLevel struct {
	bytes                   int64
	events                  []streampb.StreamEvent_KV
	willResolve             []jobspb.ResolvedSpan
	closedAt, lastAttempted time.Time
}

func (p *purgatory) Checkpoint(ctx context.Context, checkpoint []jobspb.ResolvedSpan) {
	if len(p.levels) == 0 || p.levels[len(p.levels)-1].willResolve != nil {
		// If the current purgatory level is already closed, make a new one.
		p.levels = append(p.levels, purgatoryLevel{})
	}
	// Close the current layer and mark it as resolving this checkpoint.
	p.levels[len(p.levels)-1].willResolve = checkpoint
	p.levels[len(p.levels)-1].closedAt = timeutil.Now()
}

func (p *purgatory) Store(
	ctx context.Context, events []streampb.StreamEvent_KV, byteSize int64,
) error {
	if len(events) == 0 {
		return nil
	}

	if p.full() {
		if err := p.Drain(ctx); err != nil {
			return err
		}
	}

	p.levels = append(p.levels, purgatoryLevel{events: events, bytes: byteSize})
	p.bytes += byteSize
	p.bytesGauge.Inc(byteSize)
	p.eventsGauge.Inc(int64(len(events)))
	return nil
}

func (p *purgatory) Drain(ctx context.Context) error {
	var resolved int

	for i, lvl := range p.levels {
		// If we need to make space, or if the events have been in purgatory for too
		// long, we will tell flush that it *must* process events.
		allowRetry := retryAllowed
		if p.full() {
			allowRetry = noSpace
		} else if p.deadline != nil && !lvl.closedAt.IsZero() && timeutil.Since(lvl.closedAt) > p.deadline() {
			allowRetry = tooOld
		}

		// If tried to flush this purgatory recently and it isn't required to flush
		// now, wait a until next time to try again.
		if p.delay != nil && allowRetry == retryAllowed && timeutil.Since(lvl.lastAttempted) < p.delay() {
			break
		}
		p.levels[i].lastAttempted = timeutil.Now()

		const isRetry = true
		levelBytes, levelCount := p.levels[i].bytes, len(p.levels[i].events)
		remaining, remainingSize, err := p.flush(ctx, p.levels[i].events, isRetry, allowRetry)
		if err != nil {
			return err
		}
		if len(remaining) > 0 {
			p.levels[i].events, p.levels[i].bytes = remaining, remainingSize
			p.bytes -= levelBytes - p.levels[i].bytes
		} else {
			p.levels[i].events, p.levels[i].bytes = nil, 0
			p.bytes -= levelBytes
		}

		p.bytesGauge.Dec(levelBytes - p.levels[i].bytes)
		p.eventsGauge.Dec(int64(levelCount - len(remaining)))

		// If we have resolved every prior level and all events in this level were
		// handled, we can resolve this level and emit its checkpoint, if any.
		if resolved == i && len(remaining) == 0 {
			resolved++
			if p.levels[i].willResolve != nil {
				if err := p.checkpoint(ctx, p.levels[i].willResolve); err != nil {
					return err
				}
			}
		}
	}

	// Remove all levels that were resolved.
	p.levels = p.levels[resolved:]
	return nil
}

func (p purgatory) Empty() bool {
	return len(p.levels) == 0
}

func (p *purgatory) full() bool {
	if p.byteLimit == nil {
		return false
	}
	return p.bytes >= p.byteLimit()
}
