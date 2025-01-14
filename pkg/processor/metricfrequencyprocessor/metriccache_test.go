package metricfrequencyprocessor

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.opentelemetry.io/collector/model/pdata"
)

func TestEmptyRead(t *testing.T) {
	cache := newCache()

	result := cache.List("a")

	assert.Equal(t, emptyResult, result)
}

func TestSingleRegister(t *testing.T) {
	cache := newCache()
	cache.Register("a", newDataPoint(timestamp1, 0.0))

	result := cache.List("a")

	assert.Equal(t, map[pdata.Timestamp]float64{timestamp1: 0.0}, result)
}

func TestTwoRegistersOfSingleMetric(t *testing.T) {
	cache := newCache()
	cache.Register("a", newDataPoint(timestamp1, 0.0))
	cache.Register("a", newDataPoint(timestamp2, 1.0))

	result := cache.List("a")

	assert.Equal(t, map[pdata.Timestamp]float64{timestamp1: 0.0, timestamp2: 1.0}, result)
}

func TestTwoRegistersOnTwoMetrics(t *testing.T) {
	cache := newCache()
	cache.Register("a", newDataPoint(timestamp1, 0.0))
	cache.Register("b", newDataPoint(timestamp2, 1.0))

	result1 := cache.List("a")
	result2 := cache.List("b")

	assert.Equal(t, map[pdata.Timestamp]float64{timestamp1: 0.0}, result1)
	assert.Equal(t, map[pdata.Timestamp]float64{timestamp2: 1.0}, result2)
}

var emptyResult = make(map[pdata.Timestamp]float64)
var timestamp1 = pdata.NewTimestampFromTime(time.Unix(0, 0))
var timestamp2 = pdata.NewTimestampFromTime(time.Unix(1, 0))

func newCache() *metricCache {
	return newMetricCache(createDefaultConfig().(*Config).cacheConfig)
}

func newDataPoint(timestamp pdata.Timestamp, value float64) pdata.NumberDataPoint {
	result := pdata.NewNumberDataPoint()
	result.SetTimestamp(timestamp)
	result.SetDoubleVal(value)
	return result
}
