package stats

import (
	"fmt"
	"log"
	"runtime"
	"sync/atomic"
	"time"
)

type AppStats struct {
	StartTime time.Time

	// Primary upstream connection (renamed from Binance* since cryexc-history is
	// exchange-agnostic at the package level — the concrete upstream is chosen
	// at startup via CLI flags). ExchangeName is set once on boot and drives
	// the log output so operators can tell at a glance which feed is active.
	ExchangeName         string
	TradesReceived       atomic.Uint64
	LiquidationsReceived atomic.Uint64
	PrimaryConnected     atomic.Bool
	PrimaryReconnects    atomic.Uint64

	DBFlushes atomic.Uint64

	HistoryRequests     atomic.Uint64
	HistoryActiveCount  atomic.Int32
	historyLatencySum   atomic.Uint64 // in microseconds
	historyLatencyCount atomic.Uint64
}

var Stats = &AppStats{
	StartTime: time.Now(),
}

func (s *AppStats) RecordHistoryLatency(d time.Duration) {
	s.historyLatencySum.Add(uint64(d.Microseconds()))
	s.historyLatencyCount.Add(1)
}

func (s *AppStats) AvgHistoryLatency() time.Duration {
	count := s.historyLatencyCount.Load()
	if count == 0 {
		return 0
	}
	return time.Duration(s.historyLatencySum.Load()/count) * time.Microsecond
}

func LogStats() {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	uptime := FormatDuration(time.Since(Stats.StartTime))
	memMB := float64(m.Alloc) / 1024 / 1024
	sysMB := float64(m.Sys) / 1024 / 1024
	goroutines := runtime.NumGoroutine()
	trades := Stats.TradesReceived.Load()
	liqs := Stats.LiquidationsReceived.Load()
	historyReqs := Stats.HistoryRequests.Load()
	avgLatency := Stats.AvgHistoryLatency()
	connected := "connected"
	if !Stats.PrimaryConnected.Load() {
		connected = "disconnected"
	}

	exchange := Stats.ExchangeName
	if exchange == "" {
		exchange = "primary"
	}

	log.Printf("[STATS] uptime=%s mem=%.0fMB/%.0fMB goroutines=%d trades=%dk liqs=%d history_reqs=%d (avg %.1fs) %s=%s",
		uptime,
		memMB,
		sysMB,
		goroutines,
		trades/1000,
		liqs,
		historyReqs,
		avgLatency.Seconds(),
		exchange,
		connected,
	)
}

func StartStatsLogger(interval time.Duration, stopCh <-chan struct{}) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				LogStats()
			case <-stopCh:
				return
			}
		}
	}()
}

type TableCounts struct {
	Trades int64 `json:"trades"`
	Liqs   int64 `json:"liquidations"`
}

func FormatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second

	if h > 0 {
		return fmt.Sprintf("%dh%dm", h, m)
	}
	if m > 0 {
		return fmt.Sprintf("%dm%ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}
