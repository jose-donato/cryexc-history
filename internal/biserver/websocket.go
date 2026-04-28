package biserver

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jose-donato/cryexc-history/internal/stats"
	"github.com/jose-donato/cryexc-history/internal/store"
)

// Catalog advertises the single exchange/symbol this server is serving. It's
// set once at startup from CLI flags and driven verbatim into /v1/info.
type Catalog struct {
	ExchangeID  string // "binancef"
	DisplayName string // "Binance Futures"
	Symbol      string // "BTCUSDT"
}

type Server struct {
	store     *store.Store
	authToken string // empty = no auth required
	catalog   Catalog
}

// NewServer wires up a v1 handler set. An empty authToken disables the
// bearer-token check entirely — the operator is expected to bind the server
// to 127.0.0.1 in that case (see `cryexc-history --help`).
func NewServer(s *store.Store, authToken string, catalog Catalog) *Server {
	return &Server{store: s, authToken: authToken, catalog: catalog}
}

// ServerName is advertised in /v1/info for client diagnostics. Deliberately
// distinct from the hosted backend identifier so frontends can tell at a
// glance which flavor they're talking to.
const ServerName = "cryexc-history/0.1.1"

// CORS is deliberately permissive by default — the threat model here is a
// single user running a binary on their own box, talking to a frontend at
// whatever origin they happen to be using (cryexc.josedonato.com, localhost,
// their own fork). If the user binds to a public interface with no auth
// token, the startup banner already yells at them. Narrowing CORS wouldn't
// protect an unauthed public instance and it would break the simplest use
// case. Operators who want to lock this down further should front with a
// reverse proxy they already understand.
func SetCORS(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		origin = "*"
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Vary", "Origin")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
}

// validateBearerToken returns true when either auth is disabled (empty token)
// or the request carries the correct `Authorization: Bearer <token>` header.
func (s *Server) validateBearerToken(r *http.Request) bool {
	if s.authToken == "" {
		return true
	}
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return false
	}
	return h[len(prefix):] == s.authToken
}

// ============================================================================
// v1 protocol — wire types pinned to byod-spec.md. Local response structs
// intentionally decouple JSON field names from store.* tags.
// ============================================================================

type v1SymbolInfo struct {
	Symbol          string `json:"symbol"`
	HasTrades       bool   `json:"has_trades"`
	HasLiquidations bool   `json:"has_liquidations"`
	DataStartMs     int64  `json:"data_start_ms"`
	DataEndMs       int64  `json:"data_end_ms"`
}

type v1ExchangeInfo struct {
	ID          string         `json:"id"`
	DisplayName string         `json:"display_name"`
	Symbols     []v1SymbolInfo `json:"symbols"`
}

type v1InfoResponse struct {
	ProtocolVersion int              `json:"protocol_version"`
	Server          string           `json:"server"`
	ServerTimeMs    int64            `json:"server_time_ms"`
	Exchanges       []v1ExchangeInfo `json:"exchanges"`
}

type v1Trade struct {
	TradeID      int64   `json:"trade_id"`
	Price        float64 `json:"price"`
	Quantity     float64 `json:"quantity"`
	TimestampMs  int64   `json:"timestamp_ms"`
	IsBuyerMaker bool    `json:"is_buyer_maker"`
}

type v1TradesResponse struct {
	Trades        []v1Trade `json:"trades"`
	Truncated     bool      `json:"truncated"`
	ActualStartMs int64     `json:"actual_start_ms"`
	ActualEndMs   int64     `json:"actual_end_ms"`
}

type v1Liquidation struct {
	Side        string  `json:"side"`
	Price       float64 `json:"price"`
	Quantity    float64 `json:"quantity"`
	TimestampMs int64   `json:"timestamp_ms"`
	NotionalUSD float64 `json:"notional_usd"`
}

type v1LiquidationsResponse struct {
	Liquidations  []v1Liquidation `json:"liquidations"`
	Truncated     bool            `json:"truncated"`
	ActualStartMs int64           `json:"actual_start_ms"`
	ActualEndMs   int64           `json:"actual_end_ms"`
}

type v1ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type v1ErrorEnvelope struct {
	Error v1ErrorBody `json:"error"`
}

func writeV1Error(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v1ErrorEnvelope{Error: v1ErrorBody{Code: code, Message: msg}})
}

// parseRangeParams extracts and validates the shared /v1/trades + /v1/liquidations
// query-param surface.
func parseRangeParams(w http.ResponseWriter, r *http.Request) (string, string, int64, int64, int, bool) {
	q := r.URL.Query()
	exchange := q.Get("exchange")
	symbol := q.Get("symbol")
	if exchange == "" || symbol == "" {
		writeV1Error(w, http.StatusBadRequest, "invalid_range", "exchange and symbol are required")
		return "", "", 0, 0, 0, false
	}

	startMs, err := strconv.ParseInt(q.Get("start_ms"), 10, 64)
	if err != nil {
		writeV1Error(w, http.StatusBadRequest, "invalid_range", "start_ms must be an integer")
		return "", "", 0, 0, 0, false
	}
	endMs, err := strconv.ParseInt(q.Get("end_ms"), 10, 64)
	if err != nil {
		writeV1Error(w, http.StatusBadRequest, "invalid_range", "end_ms must be an integer")
		return "", "", 0, 0, 0, false
	}
	if startMs >= endMs {
		writeV1Error(w, http.StatusBadRequest, "invalid_range", "start_ms must be less than end_ms")
		return "", "", 0, 0, 0, false
	}

	limit := 0
	if l := q.Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	return exchange, symbol, startMs, endMs, limit, true
}

func (s *Server) v1Preflight(w http.ResponseWriter, r *http.Request) bool {
	SetCORS(w, r)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return true
	}
	if r.Method != http.MethodGet {
		writeV1Error(w, http.StatusMethodNotAllowed, "server_error", "method not allowed")
		return true
	}
	if !s.validateBearerToken(r) {
		writeV1Error(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
		return true
	}
	return false
}

// ServeV1Info returns the /v1/info discovery payload. Advertises the single
// exchange/symbol this instance was configured with at startup.
func (s *Server) ServeV1Info(w http.ResponseWriter, r *http.Request) {
	if s.v1Preflight(w, r) {
		return
	}

	now := time.Now()
	dataEnd := now.UnixMilli()
	dataStart := now.Add(-store.RetentionPeriod).UnixMilli()

	resp := v1InfoResponse{
		ProtocolVersion: 1,
		Server:          ServerName,
		ServerTimeMs:    dataEnd,
		Exchanges: []v1ExchangeInfo{
			{
				ID:          s.catalog.ExchangeID,
				DisplayName: s.catalog.DisplayName,
				Symbols: []v1SymbolInfo{
					{
						Symbol:          s.catalog.Symbol,
						HasTrades:       true,
						HasLiquidations: true,
						DataStartMs:     dataStart,
						DataEndMs:       dataEnd,
					},
				},
			},
		},
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// ServeV1Trades returns historical trades for [start_ms, end_ms).
func (s *Server) ServeV1Trades(w http.ResponseWriter, r *http.Request) {
	if s.v1Preflight(w, r) {
		return
	}
	exchange, symbol, startMs, endMs, limit, ok := parseRangeParams(w, r)
	if !ok {
		return
	}

	startTime := time.Now()
	stats.Stats.HistoryRequests.Add(1)
	stats.Stats.HistoryActiveCount.Add(1)
	defer func() {
		stats.Stats.HistoryActiveCount.Add(-1)
		stats.Stats.RecordHistoryLatency(time.Since(startTime))
	}()

	s.store.Flush()

	rows, truncated, actualStartMs, err := s.store.QueryTradesInRange(startMs, endMs, exchange, symbol, limit)
	if err != nil {
		log.Printf("v1/trades query error: %v", err)
		writeV1Error(w, http.StatusInternalServerError, "server_error", "query failed")
		return
	}

	wire := make([]v1Trade, len(rows))
	for i, t := range rows {
		wire[i] = v1Trade{
			TradeID:      t.TradeID,
			Price:        t.Price,
			Quantity:     t.Quantity,
			TimestampMs:  t.TimestampMs,
			IsBuyerMaker: t.IsBuyerMaker,
		}
	}

	actualEndMs := endMs
	if len(wire) > 0 {
		actualEndMs = wire[len(wire)-1].TimestampMs + 1
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v1TradesResponse{
		Trades:        wire,
		Truncated:     truncated,
		ActualStartMs: actualStartMs,
		ActualEndMs:   actualEndMs,
	})

	log.Printf("v1/trades: %d rows (truncated=%v) for %s/%s", len(wire), truncated, exchange, symbol)
}

// ServeV1Liquidations returns historical liquidations for [start_ms, end_ms).
func (s *Server) ServeV1Liquidations(w http.ResponseWriter, r *http.Request) {
	if s.v1Preflight(w, r) {
		return
	}
	exchange, symbol, startMs, endMs, limit, ok := parseRangeParams(w, r)
	if !ok {
		return
	}

	s.store.Flush()

	rows, truncated, actualStartMs, err := s.store.QueryLiquidationsInRange(startMs, endMs, exchange, symbol, limit)
	if err != nil {
		log.Printf("v1/liquidations query error: %v", err)
		writeV1Error(w, http.StatusInternalServerError, "server_error", "query failed")
		return
	}

	wire := make([]v1Liquidation, len(rows))
	for i, l := range rows {
		wire[i] = v1Liquidation{
			Side:        l.Side,
			Price:       l.Price,
			Quantity:    l.Quantity,
			TimestampMs: l.TimestampMs,
			NotionalUSD: l.NotionalUSD,
		}
	}

	actualEndMs := endMs
	if len(wire) > 0 {
		actualEndMs = wire[len(wire)-1].TimestampMs + 1
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v1LiquidationsResponse{
		Liquidations:  wire,
		Truncated:     truncated,
		ActualStartMs: actualStartMs,
		ActualEndMs:   actualEndMs,
	})

	log.Printf("v1/liquidations: %d rows (truncated=%v) for %s/%s", len(wire), truncated, exchange, symbol)
}

// ServeHealth is an unauthenticated liveness endpoint. Doesn't touch the DB —
// probes only need to know the process is up and answering.
func (s *Server) ServeHealth(w http.ResponseWriter, r *http.Request) {
	SetCORS(w, r)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}
