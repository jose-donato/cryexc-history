package binance

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/jose-donato/cryexc-history/internal/stats"
	"github.com/jose-donato/cryexc-history/internal/store"

	"github.com/gorilla/websocket"
)

const (
	reconnectDelay = 5 * time.Second
	pingInterval   = 30 * time.Second
)

// Config identifies which upstream feed a Client ingests from. For Phase 4
// cryexc-history supports a single symbol on binancef (Binance Futures) —
// multi-symbol and multi-exchange are Phase 5 territory.
type Config struct {
	Exchange string // "binancef" (Binance Futures) — only supported for now
	Symbol   string // e.g. "BTCUSDT"
}

type Client struct {
	cfg    Config
	wsURL  string
	store  *store.Store
	conn   *websocket.Conn
	connMu sync.Mutex
	stopCh chan struct{}
	wg     sync.WaitGroup

	pingCancel context.CancelFunc
	pingWg     sync.WaitGroup
}

type tradeMsg struct {
	TradeID      int64  `json:"t"`
	Price        string `json:"p"`
	Quantity     string `json:"q"`
	Timestamp    int64  `json:"T"`
	IsBuyerMaker bool   `json:"m"`
}

// NewClient validates the config and returns a Client ready to Start().
// Returns an error if the exchange isn't supported or the symbol is empty.
func NewClient(s *store.Store, cfg Config) (*Client, error) {
	url, err := buildWSURL(cfg)
	if err != nil {
		return nil, err
	}
	return &Client{
		cfg:    cfg,
		wsURL:  url,
		store:  s,
		stopCh: make(chan struct{}),
	}, nil
}

// buildWSURL composes the Binance combined-stream URL for the configured
// exchange + symbol. Only binancef is wired up for Phase 4. Adding spot is
// mechanically trivial — add another case when a user actually asks.
func buildWSURL(cfg Config) (string, error) {
	if cfg.Symbol == "" {
		return "", fmt.Errorf("binance: symbol is required")
	}
	sym := strings.ToLower(cfg.Symbol)
	switch cfg.Exchange {
	case "binancef":
		return fmt.Sprintf("wss://fstream.binance.com/stream?streams=%s@trade/%s@depth20@100ms", sym, sym), nil
	default:
		return "", fmt.Errorf("binance: unsupported exchange %q (supported: binancef)", cfg.Exchange)
	}
}

func (c *Client) Start() {
	c.wg.Add(1)
	go c.connectLoop()
}

func (c *Client) Stop() {
	close(c.stopCh)

	if c.pingCancel != nil {
		c.pingCancel()
		c.pingWg.Wait()
	}

	c.connMu.Lock()
	if c.conn != nil {
		c.conn.Close()
	}
	c.connMu.Unlock()
	c.wg.Wait()
}

func (c *Client) connectLoop() {
	defer c.wg.Done()
	for {
		select {
		case <-c.stopCh:
			return
		default:
		}

		if err := c.connect(); err != nil {
			log.Printf("binance ws connect: %v", err)
			stats.Stats.PrimaryReconnects.Add(1)
		}

		select {
		case <-c.stopCh:
			return
		case <-time.After(reconnectDelay):
		}
	}
}

func (c *Client) connect() error {
	if c.pingCancel != nil {
		c.pingCancel()
		c.pingWg.Wait()
	}

	conn, _, err := websocket.DefaultDialer.Dial(c.wsURL, nil)
	if err != nil {
		return err
	}

	c.connMu.Lock()
	c.conn = conn
	c.connMu.Unlock()

	stats.Stats.PrimaryConnected.Store(true)
	log.Printf("connected to %s WebSocket (%s)", c.cfg.Exchange, c.cfg.Symbol)

	pingCtx, pingCancel := context.WithCancel(context.Background())
	c.pingCancel = pingCancel
	c.pingWg.Add(1)
	go func() {
		defer c.pingWg.Done()
		ticker := time.NewTicker(pingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				c.connMu.Lock()
				if c.conn != nil {
					c.conn.WriteMessage(websocket.PingMessage, nil)
				}
				c.connMu.Unlock()
			case <-pingCtx.Done():
				return
			case <-c.stopCh:
				return
			}
		}
	}()

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			pingCancel()
			stats.Stats.PrimaryConnected.Store(false)
			log.Printf("binance: disconnected: %v", err)
			return err
		}
		c.handleMessage(msg)
	}
}

func (c *Client) handleMessage(msg []byte) {
	// Combined stream format wraps data: {"stream":"...", "data":{...}}
	var wrapper struct {
		Stream string          `json:"stream"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(msg, &wrapper); err != nil {
		return
	}

	if strings.Contains(wrapper.Stream, "@trade") {
		c.handleTrade(wrapper.Data)
	}
	// Depth stream is consumed by the WS ping mechanism for keepalive but we
	// don't currently persist depth in cryexc-history — v1 history is trades
	// + liquidations only. Leaving the subscription live keeps the stream
	// selector identical to the hosted backend for forward compatibility.
}

func (c *Client) handleTrade(msg []byte) {
	var m tradeMsg
	if err := json.Unmarshal(msg, &m); err != nil {
		log.Printf("trade unmarshal error: %v", err)
		return
	}

	price := parseFloat(m.Price)
	qty := parseFloat(m.Quantity)

	if price <= 0 {
		return
	}

	t := store.Trade{
		TradeID:      m.TradeID,
		Price:        price,
		Quantity:     qty,
		TimestampMs:  m.Timestamp,
		IsBuyerMaker: m.IsBuyerMaker,
		Symbol:       c.cfg.Symbol,
		Exchange:     c.cfg.Exchange,
	}

	c.store.InsertTrade(t)
	stats.Stats.TradesReceived.Add(1)
}

func parseFloat(s string) float64 {
	var f float64
	json.Unmarshal([]byte(s), &f)
	return f
}
