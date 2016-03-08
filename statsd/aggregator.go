package statsd

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/jtblin/gostatsd/backend"
	"github.com/jtblin/gostatsd/types"

	log "github.com/Sirupsen/logrus"
)

// metricAggregatorStats is a bookkeeping structure for statistics about a MetricAggregator
type metricAggregatorStats struct {
	BadLines       int64
	LastMessage    time.Time
	LastFlush      time.Time
	LastFlushError time.Time
	NumStats       int
	ProcessingTime time.Duration
}

// MetricAggregator is an object that aggregates statsd metrics.
// The function NewMetricAggregator should be used to create the objects.
//
// Incoming metrics should be sent to the MetricChan channel.
type MetricAggregator struct {
	sync.Mutex
	ExpiryInterval    time.Duration     // How often to expire metrics
	FlushInterval     time.Duration     // How often to flush metrics to the sender
	MaxWorkers        int               // Number of workers to metrics queue
	MetricQueue       chan types.Metric // Queue on which metrics are received
	PercentThresholds []float64
	Senders           []backend.MetricSender // The sender to which metrics are flushed
	Stats             metricAggregatorStats
	types.MetricMap
}

// NewMetricAggregator creates a new MetricAggregator object
func NewMetricAggregator(senders []backend.MetricSender, percentThresholds []float64, flushInterval time.Duration, expiryInterval time.Duration, maxWorkers int) *MetricAggregator {
	a := MetricAggregator{}
	a.FlushInterval = flushInterval
	a.ExpiryInterval = expiryInterval
	a.Senders = senders
	a.MetricQueue = make(chan types.Metric, maxQueueSize)
	a.MaxWorkers = maxWorkers
	a.PercentThresholds = percentThresholds
	a.Counters = types.Counters{}
	a.Timers = types.Timers{}
	a.Gauges = types.Gauges{}
	a.Sets = types.Sets{}
	return &a
}

// round rounds a number to its nearest integer value
// poor man's math.Round(x) = math.Floor(x + 0.5)
func round(v float64) float64 {
	return math.Floor(v + 0.5)
}

// flush prepares the contents of a MetricAggregator for sending via the Sender
func (a *MetricAggregator) flush() (metrics types.MetricMap) {
	defer a.Unlock()
	a.Lock()

	numStats := 0
	startTime := time.Now()

	types.EachCounter(a.Counters, func(key, tagsKey string, counter types.Counter) {
		perSecond := float64(counter.Value) / a.FlushInterval.Seconds()
		counter.PerSecond = perSecond
		a.Counters[key][tagsKey] = counter
		numStats += 2
	})

	for _, gauges := range a.Gauges {
		numStats += len(gauges)
	}

	types.EachTimer(a.Timers, func(key, tagsKey string, timer types.Timer) {
		if count := len(timer.Values); count > 0 {
			sort.Float64s(timer.Values)
			timer.Min = timer.Values[0]
			timer.Max = timer.Values[count-1]
			timer.Count = len(timer.Values)
			count := float64(timer.Count)

			cumulativeValues := []float64{timer.Min}
			cumulSumSquaresValues := []float64{timer.Min * timer.Min}
			for i := 1; i < timer.Count; i++ {
				cumulativeValues = append(cumulativeValues, timer.Values[i]+cumulativeValues[i-1])
				cumulSumSquaresValues = append(cumulSumSquaresValues,
					timer.Values[i]*timer.Values[i]+cumulSumSquaresValues[i-1])
			}

			var sumSquares = timer.Min * timer.Min
			var mean = timer.Min
			var sum = timer.Min
			var thresholdBoundary = timer.Max

			for _, pct := range a.PercentThresholds {
				numInThreshold := timer.Count
				if timer.Count > 1 {
					numInThreshold = int(round(math.Abs(pct) / 100 * count))
					if numInThreshold == 0 {
						continue
					}
					if pct > 0 {
						thresholdBoundary = timer.Values[numInThreshold-1]
						sum = cumulativeValues[numInThreshold-1]
						sumSquares = cumulSumSquaresValues[numInThreshold-1]
					} else {
						thresholdBoundary = timer.Values[timer.Count-numInThreshold]
						sum = cumulativeValues[timer.Count-1] - cumulativeValues[timer.Count-numInThreshold-1]
						sumSquares = cumulSumSquaresValues[timer.Count-1] - cumulSumSquaresValues[timer.Count-numInThreshold-1]
					}
					mean = sum / float64(numInThreshold)
				}

				sPct := fmt.Sprintf("%d", int(pct))
				timer.Percentiles.Set(fmt.Sprintf("count_%s", sPct), float64(numInThreshold))
				timer.Percentiles.Set(fmt.Sprintf("mean_%s", sPct), mean)
				timer.Percentiles.Set(fmt.Sprintf("sum_%s", sPct), sum)
				timer.Percentiles.Set(fmt.Sprintf("sum_squares_%s", sPct), sumSquares)
				if pct > 0 {
					timer.Percentiles.Set(fmt.Sprintf("upper_%s", sPct), thresholdBoundary)
				} else {
					timer.Percentiles.Set(fmt.Sprintf("lower_%s", sPct), thresholdBoundary)
				}
			}

			sum = cumulativeValues[timer.Count-1]
			sumSquares = cumulSumSquaresValues[timer.Count-1]
			mean = sum / count

			var sumOfDiffs = float64(0)
			for i := 0; i < timer.Count; i++ {
				sumOfDiffs += (timer.Values[i] - mean) * (timer.Values[i] - mean)
			}

			mid := int(math.Floor(count / 2))
			if math.Mod(count, float64(2)) == 0 {
				timer.Median = (timer.Values[mid-1] + timer.Values[mid]) / 2
			} else {
				timer.Median = timer.Values[mid]
			}

			timer.Mean = mean
			timer.StdDev = math.Sqrt(sumOfDiffs / count)
			timer.Sum = sum
			timer.SumSquares = sumSquares
			timer.PerSecond = count / a.FlushInterval.Seconds()

			a.Timers[key][tagsKey] = timer
			numStats += 9 + len(a.Timers[key][tagsKey].Percentiles)
		} else {
			timer.Count = 0
			timer.PerSecond = float64(0)
		}
	})

	for _, sets := range a.Sets {
		numStats += len(sets)
	}

	// TODO: stats with default tag
	// TODO: add bad lines to stats
	a.Stats.NumStats = numStats
	a.Stats.ProcessingTime = time.Now().Sub(startTime)
	if badLines, ok := a.Counters["statsd.bad_lines_seen"][""]; ok {
		a.Stats.BadLines += badLines.Value
	}

	return types.MetricMap{
		NumStats:       numStats,
		ProcessingTime: a.Stats.ProcessingTime,
		FlushInterval:  a.FlushInterval,
		Counters:       types.CopyCounters(a.Counters),
		Timers:         types.CopyTimers(a.Timers),
		Gauges:         types.CopyGauges(a.Gauges),
		Sets:           types.CopySets(a.Sets),
	}
}

func (a *MetricAggregator) isExpired(now, ts time.Time) bool {
	return a.ExpiryInterval != time.Duration(0) && now.Sub(ts) > a.ExpiryInterval
}

// Reset clears the contents of a MetricAggregator
func (a *MetricAggregator) Reset(now time.Time) {
	defer a.Unlock()
	a.Lock()
	a.NumStats = 0

	types.EachCounter(a.Counters, func(key, tagsKey string, counter types.Counter) {
		if a.isExpired(now, counter.Timestamp) {
			delete(a.Counters[key], tagsKey)
			if len(a.Counters[key]) == 0 {
				delete(a.Counters, key)
			}
		} else {
			interval := counter.Interval
			a.Counters[key][tagsKey] = types.Counter{Interval: interval}
		}
	})

	types.EachTimer(a.Timers, func(key, tagsKey string, timer types.Timer) {
		if a.isExpired(now, timer.Timestamp) {
			delete(a.Timers[key], tagsKey)
			if len(a.Timers[key]) == 0 {
				delete(a.Timers, key)
			}
		} else {
			interval := timer.Interval
			a.Timers[key][tagsKey] = types.Timer{Interval: interval}
		}
	})

	types.EachSet(a.Sets, func(key, tagsKey string, set types.Set) {
		if a.isExpired(now, set.Timestamp) {
			delete(a.Sets[key], tagsKey)
			if len(a.Sets[key]) == 0 {
				delete(a.Sets, key)
			}
		} else {
			interval := set.Interval
			a.Sets[key][tagsKey] = types.Set{Interval: interval, Values: make(map[string]int64)}
		}
	})

	types.EachGauge(a.Gauges, func(key, tagsKey string, gauge types.Gauge) {
		if a.isExpired(now, gauge.Timestamp) {
			delete(a.Gauges[key], tagsKey)
			if len(a.Gauges[key]) == 0 {
				delete(a.Gauges, key)
			}
		}
		// No reset for gauges, they keep the last value until expiration
	})
}

// receiveMetric is called for each incoming metric on MetricChan
func (a *MetricAggregator) receiveMetric(m types.Metric, now time.Time) {
	defer a.Unlock()
	a.Lock()

	tagsKey := m.Tags.String()

	switch m.Type {
	case types.COUNTER:
		v, ok := a.Counters[m.Name]
		if ok {
			c, ok := v[tagsKey]
			if ok {
				c.Value = c.Value + int64(m.Value)
				a.Counters[m.Name][tagsKey] = c
			} else {
				a.Counters[m.Name][tagsKey] = types.NewCounter(now, a.FlushInterval, int64(m.Value))
			}
		} else {
			a.Counters[m.Name] = make(map[string]types.Counter)
			a.Counters[m.Name][tagsKey] = types.NewCounter(now, a.FlushInterval, int64(m.Value))
		}
	case types.GAUGE:
		// TODO: handle +/-
		v, ok := a.Gauges[m.Name]
		if ok {
			g, ok := v[tagsKey]
			if ok {
				g.Value = m.Value
				a.Gauges[m.Name][tagsKey] = g
			} else {
				a.Gauges[m.Name][tagsKey] = types.NewGauge(now, a.FlushInterval, m.Value)
			}
		} else {
			a.Gauges[m.Name] = make(map[string]types.Gauge)
			a.Gauges[m.Name][tagsKey] = types.NewGauge(now, a.FlushInterval, m.Value)
		}
	case types.TIMER:
		v, ok := a.Timers[m.Name]
		if ok {
			t, ok := v[tagsKey]
			if ok {
				t.Values = append(t.Values, m.Value)
				a.Timers[m.Name][tagsKey] = t
			} else {
				a.Timers[m.Name][tagsKey] = types.NewTimer(now, a.FlushInterval, []float64{m.Value})
			}
		} else {
			a.Timers[m.Name] = make(map[string]types.Timer)
			a.Timers[m.Name][tagsKey] = types.NewTimer(now, a.FlushInterval, []float64{m.Value})
		}
	case types.SET:
		v, ok := a.Sets[m.Name]
		if ok {
			s, ok := v[tagsKey]
			if ok {
				_, ok := s.Values[m.StringValue]
				if ok {
					s.Values[m.StringValue]++
				} else {
					s.Values[m.StringValue] = 1
				}
				a.Sets[m.Name][tagsKey] = s
			} else {
				unique := make(map[string]int64)
				unique[m.StringValue] = 1
				a.Sets[m.Name][tagsKey] = types.NewSet(now, a.FlushInterval, unique)
			}
		} else {
			a.Sets[m.Name] = make(map[string]types.Set)
			unique := make(map[string]int64)
			unique[m.StringValue] = 1
			a.Sets[m.Name][tagsKey] = types.NewSet(now, a.FlushInterval, unique)
		}
	default:
		log.Errorf("Unknow metric type %s for %s", m.Type, m.Name)
	}

	a.Stats.LastMessage = time.Now()
}

func (a *MetricAggregator) processQueue() {
	for metric := range a.MetricQueue {
		a.receiveMetric(metric, time.Now())
	}
}

// Aggregate starts the MetricAggregator so it begins consuming metrics from MetricChan
// and flushing them periodically via its Sender
func (a *MetricAggregator) Aggregate() {
	flushChan := make(chan error)
	flushTimer := time.NewTimer(a.FlushInterval)

	for i := 0; i < a.MaxWorkers; i++ {
		go a.processQueue()
	}

	for {
		select {
		case <-flushTimer.C: // Time to flush to the backends
			flushed := a.flush()
			a.Reset(time.Now())
			for _, sender := range a.Senders {
				s := sender
				go func() {
					log.Debugf("Send metrics to backend %s", s.BackendName())
					flushChan <- s.SendMetrics(flushed)
				}()
			}
			flushTimer = time.NewTimer(a.FlushInterval)
		case flushResult := <-flushChan:
			a.Lock()
			if flushResult != nil {
				log.Errorf("Sending metrics to backend failed: %s", flushResult)
				a.Stats.LastFlushError = time.Now()
			} else {
				a.Stats.LastFlush = time.Now()
			}
			a.Unlock()
		}
	}
}