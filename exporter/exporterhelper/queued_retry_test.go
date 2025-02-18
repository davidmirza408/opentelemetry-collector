// Copyright The OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//       http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package exporterhelper

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opencensus.io/metric/metricdata"
	"go.opencensus.io/metric/metricproducer"
	"go.opencensus.io/tag"
	"go.uber.org/atomic"

	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/consumer/consumererror"
	"go.opentelemetry.io/collector/exporter/exporterhelper/internal"
	"go.opentelemetry.io/collector/internal/testdata"
	"go.opentelemetry.io/collector/obsreport/obsreporttest"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

func mockRequestUnmarshaler(mr *mockRequest) internal.RequestUnmarshaler {
	return func(bytes []byte) (internal.PersistentRequest, error) {
		return mr, nil
	}
}

func TestQueuedRetry_DropOnPermanentError(t *testing.T) {
	qCfg := NewDefaultQueueSettings()
	rCfg := NewDefaultRetrySettings()
	mockR := newMockRequest(context.Background(), 2, consumererror.NewPermanent(errors.New("bad data")))
	be := newBaseExporter(&defaultExporterCfg, componenttest.NewNopExporterCreateSettings(), fromOptions(WithRetry(rCfg), WithQueue(qCfg)), "", mockRequestUnmarshaler(mockR))
	ocs := newObservabilityConsumerSender(be.qrSender.consumerSender)
	be.qrSender.consumerSender = ocs
	require.NoError(t, be.Start(context.Background(), componenttest.NewNopHost()))
	t.Cleanup(func() {
		assert.NoError(t, be.Shutdown(context.Background()))
	})

	ocs.run(func() {
		// This is asynchronous so it should just enqueue, no errors expected.
		require.NoError(t, be.sender.send(mockR))
	})
	ocs.awaitAsyncProcessing()
	// In the newMockConcurrentExporter we count requests and items even for failed requests
	mockR.checkNumRequests(t, 1)
	ocs.checkSendItemsCount(t, 0)
	ocs.checkDroppedItemsCount(t, 2)
}

func TestQueuedRetry_DropOnNoRetry(t *testing.T) {
	qCfg := NewDefaultQueueSettings()
	rCfg := NewDefaultRetrySettings()
	rCfg.Enabled = false
	be := newBaseExporter(&defaultExporterCfg, componenttest.NewNopExporterCreateSettings(), fromOptions(WithRetry(rCfg), WithQueue(qCfg)), "", nopRequestUnmarshaler())
	ocs := newObservabilityConsumerSender(be.qrSender.consumerSender)
	be.qrSender.consumerSender = ocs
	require.NoError(t, be.Start(context.Background(), componenttest.NewNopHost()))
	t.Cleanup(func() {
		assert.NoError(t, be.Shutdown(context.Background()))
	})

	mockR := newMockRequest(context.Background(), 2, errors.New("transient error"))
	ocs.run(func() {
		// This is asynchronous so it should just enqueue, no errors expected.
		require.NoError(t, be.sender.send(mockR))
	})
	ocs.awaitAsyncProcessing()
	// In the newMockConcurrentExporter we count requests and items even for failed requests
	mockR.checkNumRequests(t, 1)
	ocs.checkSendItemsCount(t, 0)
	ocs.checkDroppedItemsCount(t, 2)
}

func TestQueuedRetry_OnError(t *testing.T) {
	qCfg := NewDefaultQueueSettings()
	qCfg.NumConsumers = 1
	rCfg := NewDefaultRetrySettings()
	rCfg.InitialInterval = 0
	be := newBaseExporter(&defaultExporterCfg, componenttest.NewNopExporterCreateSettings(), fromOptions(WithRetry(rCfg), WithQueue(qCfg)), "", nopRequestUnmarshaler())
	ocs := newObservabilityConsumerSender(be.qrSender.consumerSender)
	be.qrSender.consumerSender = ocs
	require.NoError(t, be.Start(context.Background(), componenttest.NewNopHost()))
	t.Cleanup(func() {
		assert.NoError(t, be.Shutdown(context.Background()))
	})

	traceErr := consumererror.NewTraces(errors.New("some error"), testdata.GenerateTraces(1))
	mockR := newMockRequest(context.Background(), 2, traceErr)
	ocs.run(func() {
		// This is asynchronous so it should just enqueue, no errors expected.
		require.NoError(t, be.sender.send(mockR))
	})
	ocs.awaitAsyncProcessing()

	// In the newMockConcurrentExporter we count requests and items even for failed requests
	mockR.checkNumRequests(t, 2)
	ocs.checkSendItemsCount(t, 2)
	ocs.checkDroppedItemsCount(t, 0)
}

func TestQueuedRetry_StopWhileWaiting(t *testing.T) {
	qCfg := NewDefaultQueueSettings()
	qCfg.NumConsumers = 1
	rCfg := NewDefaultRetrySettings()
	be := newBaseExporter(&defaultExporterCfg, componenttest.NewNopExporterCreateSettings(), fromOptions(WithRetry(rCfg), WithQueue(qCfg)), "", nopRequestUnmarshaler())
	ocs := newObservabilityConsumerSender(be.qrSender.consumerSender)
	be.qrSender.consumerSender = ocs
	require.NoError(t, be.Start(context.Background(), componenttest.NewNopHost()))

	firstMockR := newMockRequest(context.Background(), 2, errors.New("transient error"))
	ocs.run(func() {
		// This is asynchronous so it should just enqueue, no errors expected.
		require.NoError(t, be.sender.send(firstMockR))
	})

	// Enqueue another request to ensure when calling shutdown we drain the queue.
	secondMockR := newMockRequest(context.Background(), 3, nil)
	ocs.run(func() {
		// This is asynchronous so it should just enqueue, no errors expected.
		require.NoError(t, be.sender.send(secondMockR))
	})

	assert.NoError(t, be.Shutdown(context.Background()))

	// TODO: Ensure that queue is drained, and uncomment the next 3 lines.
	//  https://github.com/jaegertracing/jaeger/pull/2349
	firstMockR.checkNumRequests(t, 1)
	// secondMockR.checkNumRequests(t, 1)
	// ocs.checkSendItemsCount(t, 3)
	ocs.checkDroppedItemsCount(t, 2)
	// require.Zero(t, be.qrSender.queue.OtlpProtoSize())
}

func TestQueuedRetry_DoNotPreserveCancellation(t *testing.T) {
	qCfg := NewDefaultQueueSettings()
	qCfg.NumConsumers = 1
	rCfg := NewDefaultRetrySettings()
	be := newBaseExporter(&defaultExporterCfg, componenttest.NewNopExporterCreateSettings(), fromOptions(WithRetry(rCfg), WithQueue(qCfg)), "", nopRequestUnmarshaler())
	ocs := newObservabilityConsumerSender(be.qrSender.consumerSender)
	be.qrSender.consumerSender = ocs
	require.NoError(t, be.Start(context.Background(), componenttest.NewNopHost()))
	t.Cleanup(func() {
		assert.NoError(t, be.Shutdown(context.Background()))
	})

	ctx, cancelFunc := context.WithCancel(context.Background())
	cancelFunc()
	mockR := newMockRequest(ctx, 2, nil)
	ocs.run(func() {
		// This is asynchronous so it should just enqueue, no errors expected.
		require.NoError(t, be.sender.send(mockR))
	})
	ocs.awaitAsyncProcessing()

	mockR.checkNumRequests(t, 1)
	ocs.checkSendItemsCount(t, 2)
	ocs.checkDroppedItemsCount(t, 0)
	require.Zero(t, be.qrSender.queue.Size())
}

func TestQueuedRetry_MaxElapsedTime(t *testing.T) {
	qCfg := NewDefaultQueueSettings()
	qCfg.NumConsumers = 1
	rCfg := NewDefaultRetrySettings()
	rCfg.InitialInterval = time.Millisecond
	rCfg.MaxElapsedTime = 100 * time.Millisecond
	be := newBaseExporter(&defaultExporterCfg, componenttest.NewNopExporterCreateSettings(), fromOptions(WithRetry(rCfg), WithQueue(qCfg)), "", nopRequestUnmarshaler())
	ocs := newObservabilityConsumerSender(be.qrSender.consumerSender)
	be.qrSender.consumerSender = ocs
	require.NoError(t, be.Start(context.Background(), componenttest.NewNopHost()))
	t.Cleanup(func() {
		assert.NoError(t, be.Shutdown(context.Background()))
	})

	ocs.run(func() {
		// Add an item that will always fail.
		require.NoError(t, be.sender.send(newErrorRequest(context.Background())))
	})

	mockR := newMockRequest(context.Background(), 2, nil)
	start := time.Now()
	ocs.run(func() {
		// This is asynchronous so it should just enqueue, no errors expected.
		require.NoError(t, be.sender.send(mockR))
	})
	ocs.awaitAsyncProcessing()

	// We should ensure that we wait for more than 50ms but less than 150ms (50% less and 50% more than max elapsed).
	waitingTime := time.Since(start)
	assert.Less(t, 50*time.Millisecond, waitingTime)
	assert.Less(t, waitingTime, 150*time.Millisecond)

	// In the newMockConcurrentExporter we count requests and items even for failed requests.
	mockR.checkNumRequests(t, 1)
	ocs.checkSendItemsCount(t, 2)
	ocs.checkDroppedItemsCount(t, 7)
	require.Zero(t, be.qrSender.queue.Size())
}

type wrappedError struct {
	error
}

func (e wrappedError) Unwrap() error {
	return e.error
}

func TestQueuedRetry_ThrottleError(t *testing.T) {
	qCfg := NewDefaultQueueSettings()
	qCfg.NumConsumers = 1
	rCfg := NewDefaultRetrySettings()
	rCfg.InitialInterval = 10 * time.Millisecond
	be := newBaseExporter(&defaultExporterCfg, componenttest.NewNopExporterCreateSettings(), fromOptions(WithRetry(rCfg), WithQueue(qCfg)), "", nopRequestUnmarshaler())
	ocs := newObservabilityConsumerSender(be.qrSender.consumerSender)
	be.qrSender.consumerSender = ocs
	require.NoError(t, be.Start(context.Background(), componenttest.NewNopHost()))
	t.Cleanup(func() {
		assert.NoError(t, be.Shutdown(context.Background()))
	})

	retry := NewThrottleRetry(errors.New("throttle error"), 100*time.Millisecond)
	mockR := newMockRequest(context.Background(), 2, wrappedError{retry})
	start := time.Now()
	ocs.run(func() {
		// This is asynchronous so it should just enqueue, no errors expected.
		require.NoError(t, be.sender.send(mockR))
	})
	ocs.awaitAsyncProcessing()

	// The initial backoff is 10ms, but because of the throttle this should wait at least 100ms.
	assert.True(t, 100*time.Millisecond < time.Since(start))

	mockR.checkNumRequests(t, 2)
	ocs.checkSendItemsCount(t, 2)
	ocs.checkDroppedItemsCount(t, 0)
	require.Zero(t, be.qrSender.queue.Size())
}

func TestQueuedRetry_RetryOnError(t *testing.T) {
	qCfg := NewDefaultQueueSettings()
	qCfg.NumConsumers = 1
	qCfg.QueueSize = 1
	rCfg := NewDefaultRetrySettings()
	rCfg.InitialInterval = 0
	be := newBaseExporter(&defaultExporterCfg, componenttest.NewNopExporterCreateSettings(), fromOptions(WithRetry(rCfg), WithQueue(qCfg)), "", nopRequestUnmarshaler())
	ocs := newObservabilityConsumerSender(be.qrSender.consumerSender)
	be.qrSender.consumerSender = ocs
	require.NoError(t, be.Start(context.Background(), componenttest.NewNopHost()))
	t.Cleanup(func() {
		assert.NoError(t, be.Shutdown(context.Background()))
	})

	mockR := newMockRequest(context.Background(), 2, errors.New("transient error"))
	ocs.run(func() {
		// This is asynchronous so it should just enqueue, no errors expected.
		require.NoError(t, be.sender.send(mockR))
	})
	ocs.awaitAsyncProcessing()

	// In the newMockConcurrentExporter we count requests and items even for failed requests
	mockR.checkNumRequests(t, 2)
	ocs.checkSendItemsCount(t, 2)
	ocs.checkDroppedItemsCount(t, 0)
	require.Zero(t, be.qrSender.queue.Size())
}

func TestQueuedRetry_DropOnFull(t *testing.T) {
	qCfg := NewDefaultQueueSettings()
	qCfg.QueueSize = 0
	rCfg := NewDefaultRetrySettings()
	be := newBaseExporter(&defaultExporterCfg, componenttest.NewNopExporterCreateSettings(), fromOptions(WithRetry(rCfg), WithQueue(qCfg)), "", nopRequestUnmarshaler())
	ocs := newObservabilityConsumerSender(be.qrSender.consumerSender)
	be.qrSender.consumerSender = ocs
	require.NoError(t, be.Start(context.Background(), componenttest.NewNopHost()))
	t.Cleanup(func() {
		assert.NoError(t, be.Shutdown(context.Background()))
	})
	err := be.sender.send(newMockRequest(context.Background(), 2, errors.New("transient error")))
	require.Error(t, err)
}

func TestQueuedRetryHappyPath(t *testing.T) {
	tt, err := obsreporttest.SetupTelemetry()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tt.Shutdown(context.Background())) })

	qCfg := NewDefaultQueueSettings()
	rCfg := NewDefaultRetrySettings()
	be := newBaseExporter(&defaultExporterCfg, tt.ToExporterCreateSettings(), fromOptions(WithRetry(rCfg), WithQueue(qCfg)), "", nopRequestUnmarshaler())
	ocs := newObservabilityConsumerSender(be.qrSender.consumerSender)
	be.qrSender.consumerSender = ocs
	require.NoError(t, be.Start(context.Background(), componenttest.NewNopHost()))
	t.Cleanup(func() {
		assert.NoError(t, be.Shutdown(context.Background()))
	})

	wantRequests := 10
	reqs := make([]*mockRequest, 0, 10)
	for i := 0; i < wantRequests; i++ {
		ocs.run(func() {
			req := newMockRequest(context.Background(), 2, nil)
			reqs = append(reqs, req)
			require.NoError(t, be.sender.send(req))
		})
	}

	// Wait until all batches received
	ocs.awaitAsyncProcessing()

	require.Len(t, reqs, wantRequests)
	for _, req := range reqs {
		req.checkNumRequests(t, 1)
	}

	ocs.checkSendItemsCount(t, 2*wantRequests)
	ocs.checkDroppedItemsCount(t, 0)
}

func TestQueuedRetry_QueueMetricsReported(t *testing.T) {
	qCfg := NewDefaultQueueSettings()
	qCfg.NumConsumers = 0 // to make every request go straight to the queue
	rCfg := NewDefaultRetrySettings()
	be := newBaseExporter(&defaultExporterCfg, componenttest.NewNopExporterCreateSettings(), fromOptions(WithRetry(rCfg), WithQueue(qCfg)), "", nopRequestUnmarshaler())
	require.NoError(t, be.Start(context.Background(), componenttest.NewNopHost()))

	checkValueForGlobalManager(t, defaultExporterTags, int64(5000), "exporter/queue_capacity")
	for i := 0; i < 7; i++ {
		require.NoError(t, be.sender.send(newErrorRequest(context.Background())))
	}
	checkValueForGlobalManager(t, defaultExporterTags, int64(7), "exporter/queue_size")

	assert.NoError(t, be.Shutdown(context.Background()))
	checkValueForGlobalManager(t, defaultExporterTags, int64(0), "exporter/queue_size")
}

func TestNoCancellationContext(t *testing.T) {
	deadline := time.Now().Add(1 * time.Second)
	ctx, cancelFunc := context.WithDeadline(context.Background(), deadline)
	cancelFunc()
	require.Error(t, ctx.Err())
	d, ok := ctx.Deadline()
	require.True(t, ok)
	require.Equal(t, deadline, d)

	nctx := noCancellationContext{Context: ctx}
	assert.NoError(t, nctx.Err())
	d, ok = nctx.Deadline()
	assert.False(t, ok)
	assert.True(t, d.IsZero())
}

func TestQueueSettings_Validate(t *testing.T) {
	qCfg := NewDefaultQueueSettings()
	assert.NoError(t, qCfg.Validate())

	qCfg.QueueSize = 0
	assert.EqualError(t, qCfg.Validate(), "queue size must be positive")

	// Confirm Validate doesn't return error with invalid config when feature is disabled
	qCfg.Enabled = false
	assert.NoError(t, qCfg.Validate())
}

type mockErrorRequest struct {
	baseRequest
}

func (mer *mockErrorRequest) export(_ context.Context) error {
	return errors.New("transient error")
}

func (mer *mockErrorRequest) onError(error) request {
	return mer
}

func (mer *mockErrorRequest) Marshal() ([]byte, error) {
	return nil, nil
}

func (mer *mockErrorRequest) count() int {
	return 7
}

func newErrorRequest(ctx context.Context) request {
	return &mockErrorRequest{
		baseRequest: baseRequest{ctx: ctx},
	}
}

type mockRequest struct {
	baseRequest
	cnt          int
	mu           sync.Mutex
	consumeError error
	requestCount *atomic.Int64
}

func (m *mockRequest) export(ctx context.Context) error {
	m.requestCount.Inc()
	m.mu.Lock()
	defer m.mu.Unlock()
	err := m.consumeError
	m.consumeError = nil
	if err != nil {
		return err
	}
	// Respond like gRPC/HTTP, if context is cancelled, return error
	return ctx.Err()
}

func (m *mockRequest) Marshal() ([]byte, error) {
	return ptrace.NewProtoMarshaler().MarshalTraces(ptrace.NewTraces())
}

func (m *mockRequest) onError(error) request {
	return &mockRequest{
		baseRequest:  m.baseRequest,
		cnt:          1,
		consumeError: nil,
		requestCount: m.requestCount,
	}
}

func (m *mockRequest) checkNumRequests(t *testing.T, want int) {
	assert.Eventually(t, func() bool {
		return int64(want) == m.requestCount.Load()
	}, time.Second, 1*time.Millisecond)
}

func (m *mockRequest) count() int {
	return m.cnt
}

func newMockRequest(ctx context.Context, cnt int, consumeError error) *mockRequest {
	return &mockRequest{
		baseRequest:  baseRequest{ctx: ctx},
		cnt:          cnt,
		consumeError: consumeError,
		requestCount: atomic.NewInt64(0),
	}
}

type observabilityConsumerSender struct {
	waitGroup         *sync.WaitGroup
	sentItemsCount    *atomic.Int64
	droppedItemsCount *atomic.Int64
	nextSender        requestSender
}

func newObservabilityConsumerSender(nextSender requestSender) *observabilityConsumerSender {
	return &observabilityConsumerSender{
		waitGroup:         new(sync.WaitGroup),
		nextSender:        nextSender,
		droppedItemsCount: atomic.NewInt64(0),
		sentItemsCount:    atomic.NewInt64(0),
	}
}

func (ocs *observabilityConsumerSender) send(req request) error {
	err := ocs.nextSender.send(req)
	if err != nil {
		ocs.droppedItemsCount.Add(int64(req.count()))
	} else {
		ocs.sentItemsCount.Add(int64(req.count()))
	}
	ocs.waitGroup.Done()
	return err
}

func (ocs *observabilityConsumerSender) run(fn func()) {
	ocs.waitGroup.Add(1)
	fn()
}

func (ocs *observabilityConsumerSender) awaitAsyncProcessing() {
	ocs.waitGroup.Wait()
}

func (ocs *observabilityConsumerSender) checkSendItemsCount(t *testing.T, want int) {
	assert.EqualValues(t, want, ocs.sentItemsCount.Load())
}

func (ocs *observabilityConsumerSender) checkDroppedItemsCount(t *testing.T, want int) {
	assert.EqualValues(t, want, ocs.droppedItemsCount.Load())
}

// checkValueForGlobalManager checks that the given metrics with wantTags is reported by one of the
// metric producers
func checkValueForGlobalManager(t *testing.T, wantTags []tag.Tag, value int64, vName string) {
	producers := metricproducer.GlobalManager().GetAll()
	for _, producer := range producers {
		if checkValueForProducer(t, producer, wantTags, value, vName) {
			return
		}
	}
	require.Fail(t, fmt.Sprintf("could not find metric %v with tags %s reported", vName, wantTags))
}

// checkValueForProducer checks that the given metrics with wantTags is reported by the metric producer
func checkValueForProducer(t *testing.T, producer metricproducer.Producer, wantTags []tag.Tag, value int64, vName string) bool {
	for _, metric := range producer.Read() {
		if metric.Descriptor.Name == vName && len(metric.TimeSeries) > 0 {
			lastValue := metric.TimeSeries[len(metric.TimeSeries)-1]
			if tagsMatchLabelKeys(wantTags, metric.Descriptor.LabelKeys, lastValue.LabelValues) {
				require.Equal(t, value, lastValue.Points[len(lastValue.Points)-1].Value.(int64))
				return true
			}
		}
	}
	return false
}

// tagsMatchLabelKeys returns true if provided tags match keys and values
func tagsMatchLabelKeys(tags []tag.Tag, keys []metricdata.LabelKey, labels []metricdata.LabelValue) bool {
	if len(tags) != len(keys) {
		return false
	}
	for i := 0; i < len(tags); i++ {
		var labelVal string
		if labels[i].Present {
			labelVal = labels[i].Value
		}
		if tags[i].Key.Name() != keys[i].Key || tags[i].Value != labelVal {
			return false
		}
	}
	return true
}
