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

package pmetric

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.opentelemetry.io/collector/pdata/pcommon"
)

func TestProtoMetricsUnmarshaler_error(t *testing.T) {
	p := NewProtoUnmarshaler()
	_, err := p.UnmarshalMetrics([]byte("+$%"))
	assert.Error(t, err)
}

func TestProtoSizer(t *testing.T) {
	sizer := NewProtoMarshaler().(Sizer)
	marshaler := NewProtoMarshaler()
	md := NewMetrics()
	md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics().AppendEmpty().SetName("foo")

	size := sizer.MetricsSize(md)

	bytes, err := marshaler.MarshalMetrics(md)
	require.NoError(t, err)
	assert.Equal(t, len(bytes), size)
}

func TestProtoSizer_withNil(t *testing.T) {
	sizer := NewProtoMarshaler().(Sizer)

	assert.Equal(t, 0, sizer.MetricsSize(NewMetrics()))
}

func BenchmarkMetricsToProto(b *testing.B) {
	marshaler := NewProtoMarshaler()
	metrics := generateBenchmarkMetrics(128)
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		buf, err := marshaler.MarshalMetrics(metrics)
		require.NoError(b, err)
		assert.NotEqual(b, 0, len(buf))
	}
}

func BenchmarkMetricsFromProto(b *testing.B) {
	marshaler := NewProtoMarshaler()
	unmarshaler := NewProtoUnmarshaler()
	baseMetrics := generateBenchmarkMetrics(128)
	buf, err := marshaler.MarshalMetrics(baseMetrics)
	require.NoError(b, err)
	assert.NotEqual(b, 0, len(buf))
	b.ResetTimer()
	b.ReportAllocs()
	for n := 0; n < b.N; n++ {
		metrics, err := unmarshaler.UnmarshalMetrics(buf)
		require.NoError(b, err)
		assert.Equal(b, baseMetrics.ResourceMetrics().Len(), metrics.ResourceMetrics().Len())
	}
}

func generateBenchmarkMetrics(metricsCount int) Metrics {
	now := time.Now()
	startTime := pcommon.NewTimestampFromTime(now.Add(-10 * time.Second))
	endTime := pcommon.NewTimestampFromTime(now)

	md := NewMetrics()
	ilm := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty()
	ilm.Metrics().EnsureCapacity(metricsCount)
	for i := 0; i < metricsCount; i++ {
		im := ilm.Metrics().AppendEmpty()
		im.SetName("test_name")
		im.SetDataType(MetricDataTypeSum)
		idp := im.Sum().DataPoints().AppendEmpty()
		idp.SetStartTimestamp(startTime)
		idp.SetTimestamp(endTime)
		idp.SetIntVal(123)
	}
	return md
}
