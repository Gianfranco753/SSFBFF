//go:build goexperiment.jsonv2

package main

import (
	"context"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type metricUpdateType int

const (
	updateTypeCounterInc metricUpdateType = iota
	updateTypeHistogramObserve
	updateTypeCounterAdd
)

type metricUpdate struct {
	typ     metricUpdateType
	counter prometheus.Counter
	hist    prometheus.Observer
	value   float64
}

type metricsBatcher struct {
	updates      chan metricUpdate
	batchSize    int
	batchInterval time.Duration
	ctx          context.Context
	cancel       context.CancelFunc
	wg           sync.WaitGroup
	enabled      bool
}

var (
	globalBatcher     *metricsBatcher
	batcherInitOnce   sync.Once
	batcherEnabled    bool
	metricsSampleRate float64
)

func initMetricsBatcher() {
	batcherEnabled = getCachedMetricsBatchingEnabled()
	if !batcherEnabled || !metricsEnabled {
		return
	}

	batchSize := getCachedMetricsBatchSize()
	batchInterval := getCachedMetricsBatchInterval()
	metricsSampleRate = getCachedMetricsSampleRate()

	ctx, cancel := context.WithCancel(context.Background())
	batcher := &metricsBatcher{
		updates:       make(chan metricUpdate, batchSize*2),
		batchSize:     batchSize,
		batchInterval: batchInterval,
		ctx:           ctx,
		cancel:        cancel,
		enabled:       true,
	}

	batcher.wg.Add(1)
	go batcher.worker()

	globalBatcher = batcher
}

func (mb *metricsBatcher) worker() {
	defer mb.wg.Done()

	batch := make([]metricUpdate, 0, mb.batchSize)
	ticker := time.NewTicker(mb.batchInterval)
	defer ticker.Stop()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		for _, update := range batch {
			switch update.typ {
			case updateTypeCounterInc:
				update.counter.Inc()
			case updateTypeCounterAdd:
				update.counter.Add(update.value)
			case updateTypeHistogramObserve:
				update.hist.Observe(update.value)
			}
		}
		batch = batch[:0]
		// Update channel size gauge after flush
		updateMetricsBatcherChannelSize(len(mb.updates))
	}

	for {
		select {
		case <-mb.ctx.Done():
			flush()
			return
		case update := <-mb.updates:
			batch = append(batch, update)
			// Only update channel size gauge periodically to avoid hot path overhead
			// Update every 10 items or on flush
			if len(batch)%10 == 0 {
				updateMetricsBatcherChannelSize(len(mb.updates))
			}
			if len(batch) >= mb.batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func (mb *metricsBatcher) recordCounterInc(counter prometheus.Counter) {
	if !mb.enabled {
		counter.Inc()
		return
	}

	select {
	case mb.updates <- metricUpdate{typ: updateTypeCounterInc, counter: counter}:
		// Don't update channel size here - let worker update periodically
	default:
		// Channel full, fallback to synchronous recording
		recordMetricsDropped("batcher_full")
		counter.Inc()
	}
}

func (mb *metricsBatcher) recordHistogramObserve(hist prometheus.Observer, value float64) {
	if !mb.enabled {
		hist.Observe(value)
		return
	}

	select {
	case mb.updates <- metricUpdate{typ: updateTypeHistogramObserve, hist: hist, value: value}:
		// Don't update channel size here - let worker update periodically
	default:
		// Channel full, fallback to synchronous recording
		recordMetricsDropped("batcher_full")
		hist.Observe(value)
	}
}

func shutdownMetricsBatcher() {
	if globalBatcher != nil {
		globalBatcher.cancel()
		globalBatcher.wg.Wait()
	}
}

func shouldSample() bool {
	if metricsSampleRate >= 1.0 {
		return true
	}
	if metricsSampleRate <= 0.0 {
		// Record that metrics were dropped due to sampling
		recordMetricsDropped("sampling")
		return false
	}
	// Use nanosecond timestamp modulo for pseudo-random sampling
	// This provides consistent sampling without requiring a random number generator
	sampled := time.Now().UnixNano()%10000 < int64(metricsSampleRate*10000)
	if !sampled {
		recordMetricsDropped("sampling")
	}
	return sampled
}
