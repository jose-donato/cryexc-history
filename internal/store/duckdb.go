package store

import (
	"database/sql"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/jose-donato/cryexc-history/internal/stats"

	_ "github.com/marcboeker/go-duckdb"
)

const (
	RetentionPeriod   = 24 * time.Hour
	RetentionPeriod7d = 7 * 24 * time.Hour
	CleanupInterval   = 5 * time.Minute
	BatchSize         = 100
	BatchTimeout      = time.Second
)

type Trade struct {
	TradeID      int64
	Price        float64
	Quantity     float64
	TimestampMs  int64
	IsBuyerMaker bool
	Symbol       string
	Exchange     string
}

type Liquidation struct {
	Side        string
	Price       float64
	Quantity    float64
	TimestampMs int64
	NotionalUSD float64
	Symbol      string
	Exchange    string
}

type Store struct {
	db          *sql.DB
	tradeBuf    []Trade
	tradeMu     sync.Mutex
	tradeTimer  *time.Timer
	liquidBuf   []Liquidation
	liquidMu    sync.Mutex
	liquidTimer *time.Timer
	stopCh      chan struct{}
	wg          sync.WaitGroup
}

// New opens (or creates) a DuckDB file at dbPath. memoryLimit is passed through
// as the DuckDB `memory_limit` PRAGMA — empty string means "don't set it".
func New(dbPath string, memoryLimit string) (*Store, error) {
	db, err := sql.Open("duckdb", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open duckdb: %w", err)
	}

	if memoryLimit != "" {
		if _, err := db.Exec(fmt.Sprintf("SET memory_limit='%s'", memoryLimit)); err != nil {
			log.Printf("warning: failed to set memory_limit=%s: %v", memoryLimit, err)
		}
	}

	if err := createTables(db); err != nil {
		db.Close()
		return nil, err
	}

	s := &Store{
		db:        db,
		tradeBuf:  make([]Trade, 0, BatchSize),
		liquidBuf: make([]Liquidation, 0, BatchSize),
		stopCh:    make(chan struct{}),
	}

	s.wg.Add(1)
	go s.cleanupLoop()

	return s, nil
}

func createTables(db *sql.DB) error {
	schema := `
		CREATE TABLE IF NOT EXISTS trades (
			trade_id BIGINT NOT NULL,
			price DOUBLE NOT NULL,
			quantity DOUBLE NOT NULL,
			timestamp_ms BIGINT NOT NULL,
			is_buyer_maker BOOLEAN NOT NULL,
			symbol VARCHAR NOT NULL DEFAULT 'BTCUSDT',
			exchange VARCHAR NOT NULL DEFAULT 'binancef'
		);

		CREATE TABLE IF NOT EXISTS liquidations (
			side VARCHAR NOT NULL,
			price DOUBLE NOT NULL,
			quantity DOUBLE NOT NULL,
			timestamp_ms BIGINT NOT NULL,
			notional_usd DOUBLE NOT NULL,
			symbol VARCHAR NOT NULL DEFAULT 'BTCUSDT',
			exchange VARCHAR NOT NULL DEFAULT 'binancef'
		);
	`
	if _, err := db.Exec(schema); err != nil {
		return err
	}

	// Forward-compat: additive ALTERs for existing databases that predate the
	// symbol/exchange columns. DuckDB rejects NOT NULL in ADD COLUMN — the DEFAULT
	// alone still backfills existing rows, and new INSERTs always pass concrete values.
	alters := []string{
		"ALTER TABLE trades ADD COLUMN IF NOT EXISTS symbol VARCHAR DEFAULT 'BTCUSDT'",
		"ALTER TABLE trades ADD COLUMN IF NOT EXISTS exchange VARCHAR DEFAULT 'binancef'",
		"ALTER TABLE liquidations ADD COLUMN IF NOT EXISTS symbol VARCHAR DEFAULT 'BTCUSDT'",
		"ALTER TABLE liquidations ADD COLUMN IF NOT EXISTS exchange VARCHAR DEFAULT 'binancef'",
	}
	for _, q := range alters {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("alter: %w", err)
		}
	}

	indexes := []string{
		"CREATE INDEX IF NOT EXISTS idx_trades_symbol_ts ON trades(symbol, timestamp_ms)",
		"CREATE INDEX IF NOT EXISTS idx_liquidations_symbol_ts ON liquidations(symbol, timestamp_ms)",
	}
	for _, q := range indexes {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("index: %w", err)
		}
	}

	return nil
}

func (s *Store) InsertTrade(t Trade) {
	s.tradeMu.Lock()
	s.tradeBuf = append(s.tradeBuf, t)
	shouldFlush := len(s.tradeBuf) >= BatchSize

	if s.tradeTimer == nil {
		s.tradeTimer = time.AfterFunc(BatchTimeout, func() {
			s.flushTrades()
		})
	}
	s.tradeMu.Unlock()

	if shouldFlush {
		s.flushTrades()
	}
}

func (s *Store) flushTrades() {
	s.tradeMu.Lock()
	if len(s.tradeBuf) == 0 {
		s.tradeMu.Unlock()
		return
	}
	trades := s.tradeBuf
	s.tradeBuf = make([]Trade, 0, BatchSize)
	if s.tradeTimer != nil {
		s.tradeTimer.Stop()
		s.tradeTimer = nil
	}
	s.tradeMu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		log.Printf("trades tx begin: %v", err)
		return
	}

	stmt, err := tx.Prepare("INSERT INTO trades VALUES (?, ?, ?, ?, ?, ?, ?)")
	if err != nil {
		tx.Rollback()
		log.Printf("trades prepare: %v", err)
		return
	}
	defer stmt.Close()

	for _, t := range trades {
		_, err := stmt.Exec(t.TradeID, t.Price, t.Quantity, t.TimestampMs, t.IsBuyerMaker, t.Symbol, t.Exchange)
		if err != nil {
			log.Printf("trades insert: %v", err)
		}
	}

	if err := tx.Commit(); err != nil {
		log.Printf("trades commit: %v", err)
	} else {
		stats.Stats.DBFlushes.Add(1)
	}
}

func (s *Store) InsertLiquidation(l Liquidation) {
	s.liquidMu.Lock()
	s.liquidBuf = append(s.liquidBuf, l)
	shouldFlush := len(s.liquidBuf) >= BatchSize

	if s.liquidTimer == nil {
		s.liquidTimer = time.AfterFunc(BatchTimeout, func() {
			s.flushLiquidations()
		})
	}
	s.liquidMu.Unlock()

	if shouldFlush {
		s.flushLiquidations()
	}
}

func (s *Store) flushLiquidations() {
	s.liquidMu.Lock()
	if len(s.liquidBuf) == 0 {
		s.liquidMu.Unlock()
		return
	}
	liqs := s.liquidBuf
	s.liquidBuf = make([]Liquidation, 0, BatchSize)
	if s.liquidTimer != nil {
		s.liquidTimer.Stop()
		s.liquidTimer = nil
	}
	s.liquidMu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		log.Printf("liqs tx begin: %v", err)
		return
	}

	stmt, err := tx.Prepare("INSERT INTO liquidations VALUES (?, ?, ?, ?, ?, ?, ?)")
	if err != nil {
		tx.Rollback()
		log.Printf("liqs prepare: %v", err)
		return
	}
	defer stmt.Close()

	for _, l := range liqs {
		_, err := stmt.Exec(l.Side, l.Price, l.Quantity, l.TimestampMs, l.NotionalUSD, l.Symbol, l.Exchange)
		if err != nil {
			log.Printf("liqs insert: %v", err)
		}
	}

	if err := tx.Commit(); err != nil {
		log.Printf("liqs commit: %v", err)
	} else {
		stats.Stats.DBFlushes.Add(1)
	}
}

func (s *Store) Flush() {
	s.flushTrades()
	s.flushLiquidations()
}

const HardRowCeiling = 1_000_000

func (s *Store) QueryTradesInRange(startMs, endMs int64, exchange, symbol string, limit int) ([]Trade, bool, int64, error) {
	rowLimit := HardRowCeiling
	if limit > 0 && limit < rowLimit {
		rowLimit = limit
	}

	var total int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM trades WHERE symbol = ? AND exchange = ? AND timestamp_ms >= ? AND timestamp_ms < ?`,
		symbol, exchange, startMs, endMs,
	).Scan(&total)
	if err != nil {
		return nil, false, startMs, err
	}

	truncated := total > rowLimit
	query := `SELECT trade_id, price, quantity, timestamp_ms, is_buyer_maker, symbol, exchange
		FROM trades
		WHERE symbol = ? AND exchange = ? AND timestamp_ms >= ? AND timestamp_ms < ?
		ORDER BY timestamp_ms DESC
		LIMIT ?`
	rows, err := s.db.Query(query, symbol, exchange, startMs, endMs, rowLimit)
	if err != nil {
		return nil, false, startMs, err
	}
	defer rows.Close()

	buf := make([]Trade, 0, rowLimit)
	for rows.Next() {
		var t Trade
		if err := rows.Scan(&t.TradeID, &t.Price, &t.Quantity, &t.TimestampMs, &t.IsBuyerMaker, &t.Symbol, &t.Exchange); err != nil {
			return nil, false, startMs, err
		}
		buf = append(buf, t)
	}
	if err := rows.Err(); err != nil {
		return nil, false, startMs, err
	}

	for i, j := 0, len(buf)-1; i < j; i, j = i+1, j-1 {
		buf[i], buf[j] = buf[j], buf[i]
	}

	actualStartMs := startMs
	if len(buf) > 0 {
		actualStartMs = buf[0].TimestampMs
	}
	return buf, truncated, actualStartMs, nil
}

func (s *Store) QueryLiquidationsInRange(startMs, endMs int64, exchange, symbol string, limit int) ([]Liquidation, bool, int64, error) {
	rowLimit := HardRowCeiling
	if limit > 0 && limit < rowLimit {
		rowLimit = limit
	}

	var total int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM liquidations WHERE symbol = ? AND exchange = ? AND timestamp_ms >= ? AND timestamp_ms < ?`,
		symbol, exchange, startMs, endMs,
	).Scan(&total)
	if err != nil {
		return nil, false, startMs, err
	}

	truncated := total > rowLimit
	query := `SELECT side, price, quantity, timestamp_ms, notional_usd, symbol, exchange
		FROM liquidations
		WHERE symbol = ? AND exchange = ? AND timestamp_ms >= ? AND timestamp_ms < ?
		ORDER BY timestamp_ms DESC
		LIMIT ?`
	rows, err := s.db.Query(query, symbol, exchange, startMs, endMs, rowLimit)
	if err != nil {
		return nil, false, startMs, err
	}
	defer rows.Close()

	buf := make([]Liquidation, 0, rowLimit)
	for rows.Next() {
		var l Liquidation
		if err := rows.Scan(&l.Side, &l.Price, &l.Quantity, &l.TimestampMs, &l.NotionalUSD, &l.Symbol, &l.Exchange); err != nil {
			return nil, false, startMs, err
		}
		buf = append(buf, l)
	}
	if err := rows.Err(); err != nil {
		return nil, false, startMs, err
	}

	for i, j := 0, len(buf)-1; i < j; i, j = i+1, j-1 {
		buf[i], buf[j] = buf[j], buf[i]
	}

	actualStartMs := startMs
	if len(buf) > 0 {
		actualStartMs = buf[0].TimestampMs
	}
	return buf, truncated, actualStartMs, nil
}

func (s *Store) cleanupLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(CleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.cleanup()
		case <-s.stopCh:
			return
		}
	}
}

func (s *Store) cleanup() {
	now := time.Now()
	cutoff24h := now.Add(-RetentionPeriod).UnixMilli()

	jobs := []struct {
		table  string
		cutoff int64
	}{
		{"trades", cutoff24h},
		{"liquidations", cutoff24h},
	}
	for _, j := range jobs {
		res, err := s.db.Exec(fmt.Sprintf("DELETE FROM %s WHERE timestamp_ms < ?", j.table), j.cutoff)
		if err != nil {
			log.Printf("cleanup %s: %v", j.table, err)
			continue
		}
		if n, _ := res.RowsAffected(); n > 0 {
			log.Printf("cleaned %d rows from %s", n, j.table)
		}
	}
}

func (s *Store) GetTableCounts() (stats.TableCounts, error) {
	var counts stats.TableCounts
	row := s.db.QueryRow("SELECT COUNT(*) FROM trades")
	if err := row.Scan(&counts.Trades); err != nil {
		return counts, err
	}
	row = s.db.QueryRow("SELECT COUNT(*) FROM liquidations")
	if err := row.Scan(&counts.Liqs); err != nil {
		return counts, err
	}
	return counts, nil
}

func (s *Store) Close() error {
	close(s.stopCh)
	s.wg.Wait()

	s.tradeMu.Lock()
	if s.tradeTimer != nil {
		s.tradeTimer.Stop()
	}
	s.tradeMu.Unlock()

	s.liquidMu.Lock()
	if s.liquidTimer != nil {
		s.liquidTimer.Stop()
	}
	s.liquidMu.Unlock()

	s.flushTrades()
	s.flushLiquidations()
	return s.db.Close()
}
