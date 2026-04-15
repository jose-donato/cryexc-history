package binanceliq

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	minDelay  = 1 * time.Second
	maxDelay  = 30 * time.Second
	pongWait  = 10 * time.Minute
	pingEvery = 3 * time.Minute
)

// Config mirrors binance.Config — single-symbol, binancef-only for Phase 4.
// The liquidations WS uses the symbol-specific endpoint so we don't have to
// filter the firehose in-process.
type Config struct {
	Exchange string
	Symbol   string
}

type liquidationEvent struct {
	Order struct {
		Symbol       string `json:"s"`
		Side         string `json:"S"`
		Price        string `json:"p"`
		AvgPrice     string `json:"ap"`
		OrigQty      string `json:"q"`
		CumFilledQty string `json:"z"`
		TradeTime    int64  `json:"T"`
	} `json:"o"`
}

type Client struct {
	cfg   Config
	wsURL string

	OnLiquidation func(symbol string, price, qty, notionalUSD float64, isLong bool)

	conn   *websocket.Conn
	connMu sync.Mutex
}

func NewClient(cfg Config) (*Client, error) {
	url, err := buildWSURL(cfg)
	if err != nil {
		return nil, err
	}
	return &Client{cfg: cfg, wsURL: url}, nil
}

func buildWSURL(cfg Config) (string, error) {
	if cfg.Symbol == "" {
		return "", fmt.Errorf("binanceliq: symbol is required")
	}
	sym := strings.ToLower(cfg.Symbol)
	switch cfg.Exchange {
	case "binancef":
		return fmt.Sprintf("wss://fstream.binance.com/ws/%s@forceOrder", sym), nil
	default:
		return "", fmt.Errorf("binanceliq: unsupported exchange %q", cfg.Exchange)
	}
}

func (c *Client) Start(ctx context.Context) {
	delay := minDelay
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		err := c.connect(ctx)
		if err != nil {
			log.Printf("binanceliq: disconnected: %v", err)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}

		delay = time.Duration(math.Min(float64(delay)*2, float64(maxDelay)))
	}
}

func (c *Client) connect(ctx context.Context) error {
	conn, _, err := websocket.DefaultDialer.Dial(c.wsURL, nil)
	if err != nil {
		return err
	}

	c.connMu.Lock()
	c.conn = conn
	c.connMu.Unlock()

	log.Printf("binanceliq: connected to %s forceOrder", c.cfg.Symbol)

	conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	pingDone := make(chan struct{})
	go func() {
		defer close(pingDone)
		ticker := time.NewTicker(pingEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				c.connMu.Lock()
				err := conn.WriteMessage(websocket.PingMessage, nil)
				c.connMu.Unlock()
				if err != nil {
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	defer func() {
		conn.Close()
		<-pingDone
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		_, msg, err := conn.ReadMessage()
		if err != nil {
			return err
		}

		c.handleMessage(msg)
	}
}

func (c *Client) handleMessage(msg []byte) {
	var evt liquidationEvent
	if err := json.Unmarshal(msg, &evt); err != nil {
		return
	}

	o := evt.Order
	price := parseFloat(o.AvgPrice)
	if price == 0 {
		price = parseFloat(o.Price)
	}
	qty := parseFloat(o.CumFilledQty)
	if qty == 0 {
		qty = parseFloat(o.OrigQty)
	}
	notional := price * qty

	if c.OnLiquidation != nil {
		isLong := o.Side == "SELL"
		c.OnLiquidation(o.Symbol, price, qty, notional, isLong)
	}
}

func parseFloat(s string) float64 {
	var f float64
	json.Unmarshal([]byte(s), &f)
	return f
}
