package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jose-donato/cryexc-history/internal/binance"
	"github.com/jose-donato/cryexc-history/internal/binanceliq"
	"github.com/jose-donato/cryexc-history/internal/biserver"
	"github.com/jose-donato/cryexc-history/internal/stats"
	"github.com/jose-donato/cryexc-history/internal/store"
)

// cryexc-history: self-hosted BYOD (bring-your-own-data) history backend for
// the Cryexc trading terminal. Speaks the v1 protocol defined in
// docs/byod-spec.md (see cryexc-extension2). Streams live trades +
// liquidations from a configured upstream exchange, persists to DuckDB, and
// serves GET /v1/info, /v1/trades, /v1/liquidations.

const (
	defaultBind          = "127.0.0.1"
	defaultPort          = 8080
	defaultDBPath        = "cryexc-history.duckdb"
	defaultMemoryLimit   = "256MB"
	defaultExchange      = "binancef"
	defaultSymbol        = "BTCUSDT"
	readHeaderTimeout    = 10 * time.Second
	gracefulShutdownWait = 5 * time.Second
	statsInterval        = 60 * time.Second
)

func main() {
	var (
		bind          = flag.String("bind", defaultBind, "Interface to bind HTTP server to. Default 127.0.0.1 (loopback only). Set to 0.0.0.0 to expose on all interfaces — you'll also want --auth-token in that case.")
		port          = flag.Int("port", defaultPort, "HTTP port")
		dbPath        = flag.String("db-path", defaultDBPath, "Path to the DuckDB file. Will be created if it doesn't exist.")
		authToken     = flag.String("auth-token", "", "Bearer token required for /v1/* requests. If unset, auth is disabled — strongly recommended only when --bind is loopback.")
		memoryLimit   = flag.String("db-memory-limit", defaultMemoryLimit, "DuckDB memory_limit setting (e.g. 256MB, 1GB)")
		exchange      = flag.String("exchange", defaultExchange, "Upstream exchange. Currently only 'binancef' (Binance Futures) is supported.")
		symbols       = flag.String("symbols", defaultSymbol, "Comma-separated symbol list. For Phase 4 only the first symbol is ingested; multi-symbol lands in v1.1.")
		showVersion   = flag.Bool("version", false, "Print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(biserver.ServerName)
		return
	}

	symbol := firstSymbol(*symbols)
	if symbol == "" {
		log.Fatal("--symbols must contain at least one symbol")
	}

	printStartupBanner(*bind, *port, *dbPath, symbol, *exchange, *authToken)

	stats.Stats.ExchangeName = *exchange

	duckStore, err := store.New(*dbPath, *memoryLimit)
	if err != nil {
		log.Fatalf("store.New: %v", err)
	}
	defer duckStore.Close()

	binanceCfg := binance.Config{Exchange: *exchange, Symbol: symbol}
	binanceClient, err := binance.NewClient(duckStore, binanceCfg)
	if err != nil {
		log.Fatalf("binance.NewClient: %v", err)
	}
	binanceClient.Start()
	defer binanceClient.Stop()

	liqCfg := binanceliq.Config{Exchange: *exchange, Symbol: symbol}
	liqClient, err := binanceliq.NewClient(liqCfg)
	if err != nil {
		log.Fatalf("binanceliq.NewClient: %v", err)
	}
	liqClient.OnLiquidation = func(sym string, price, qty, notional float64, isLong bool) {
		side := "BUY"
		if isLong {
			side = "SELL"
		}
		duckStore.InsertLiquidation(store.Liquidation{
			Side:        side,
			Price:       price,
			Quantity:    qty,
			TimestampMs: time.Now().UnixMilli(),
			NotionalUSD: notional,
			Symbol:      sym,
			Exchange:    *exchange,
		})
		stats.Stats.LiquidationsReceived.Add(1)
	}
	liqCtx, liqCancel := context.WithCancel(context.Background())
	defer liqCancel()
	go liqClient.Start(liqCtx)

	catalog := biserver.Catalog{
		ExchangeID:  *exchange,
		DisplayName: exchangeDisplayName(*exchange),
		Symbol:      symbol,
	}
	srv := biserver.NewServer(duckStore, *authToken, catalog)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", srv.ServeHealth)
	mux.HandleFunc("/v1/info", srv.ServeV1Info)
	mux.HandleFunc("/v1/trades", srv.ServeV1Trades)
	mux.HandleFunc("/v1/liquidations", srv.ServeV1Liquidations)

	addr := fmt.Sprintf("%s:%d", *bind, *port)
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: readHeaderTimeout,
	}

	statsStopCh := make(chan struct{})
	stats.StartStatsLogger(statsInterval, statsStopCh)

	go func() {
		log.Printf("cryexc-history listening on http://%s", addr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http listen: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	log.Println("shutting down...")

	close(statsStopCh)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), gracefulShutdownWait)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("http shutdown: %v", err)
	}
}

// firstSymbol trims and returns the first entry in a comma-separated list.
// Multi-symbol ingestion is Phase 5 — for now we only feed the first one to
// the single-client ingesters and advertise it in /v1/info.
func firstSymbol(list string) string {
	for _, s := range strings.Split(list, ",") {
		s = strings.TrimSpace(s)
		if s != "" {
			return strings.ToUpper(s)
		}
	}
	return ""
}

func exchangeDisplayName(id string) string {
	switch id {
	case "binancef":
		return "Binance Futures"
	default:
		return id
	}
}

// printStartupBanner loudly surfaces misconfigurations that would otherwise
// silently put the operator in a bad spot — specifically "bound to a public
// interface with no auth token". Loopback + no auth is fine (ordinary
// self-host). Public + auth is fine (operator knows what they're doing).
// Public + no auth is a footgun.
func printStartupBanner(bind string, port int, dbPath, symbol, exchange, authToken string) {
	fmt.Println("==========================================")
	fmt.Println("  cryexc-history — BYOD v1 server")
	fmt.Println("==========================================")
	fmt.Printf("  bind         : %s\n", bind)
	fmt.Printf("  port         : %d\n", port)
	fmt.Printf("  db           : %s\n", dbPath)
	fmt.Printf("  exchange     : %s\n", exchange)
	fmt.Printf("  symbol       : %s\n", symbol)
	if authToken == "" {
		fmt.Println("  auth         : disabled")
	} else {
		fmt.Println("  auth         : bearer token required")
	}
	fmt.Println("==========================================")

	if authToken == "" && bind != "127.0.0.1" && bind != "localhost" && bind != "::1" {
		fmt.Println()
		fmt.Println("  !! WARNING: --bind is non-loopback and --auth-token is unset.")
		fmt.Println("  !! Anyone who can reach this port can read your history.")
		fmt.Println("  !! Either bind to 127.0.0.1 or set --auth-token=<secret>.")
		fmt.Println()
	}
}
