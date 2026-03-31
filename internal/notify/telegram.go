package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

// AlertLevel represents the severity of an alert
type AlertLevel string

const (
	AlertInfo     AlertLevel = "INFO"
	AlertWarning  AlertLevel = "WARNING"
	AlertCritical AlertLevel = "CRITICAL"
)

// TelegramNotifier sends notifications to Telegram
type TelegramNotifier struct {
	botToken   string
	chatID     string
	httpClient *http.Client
	enabled    bool

	// Rate limiting
	mu              sync.Mutex
	lastSent        map[string]time.Time // key -> last sent time
	cooldowns       map[string]time.Duration
	defaultCooldown time.Duration

	// Bot info for messages
	exchange string
	symbol   string
	botID    string
}

// TelegramConfig holds configuration for Telegram notifier
type TelegramConfig struct {
	BotToken string
	ChatID   string
	Exchange string
	Symbol   string
	BotID    string
}

// NewTelegramNotifier creates a new Telegram notifier
func NewTelegramNotifier(cfg TelegramConfig) *TelegramNotifier {
	enabled := cfg.BotToken != "" && cfg.ChatID != ""

	n := &TelegramNotifier{
		botToken: cfg.BotToken,
		chatID:   cfg.ChatID,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		enabled:         enabled,
		lastSent:        make(map[string]time.Time),
		cooldowns:       make(map[string]time.Duration),
		defaultCooldown: 1 * time.Minute,
		exchange:        cfg.Exchange,
		symbol:          cfg.Symbol,
		botID:           cfg.BotID,
	}

	// Set custom cooldowns for different alert types
	n.cooldowns["fill"] = 0 // No cooldown for fills
	n.cooldowns["drawdown_warning"] = 5 * time.Minute
	n.cooldowns["drawdown_critical"] = 1 * time.Minute
	n.cooldowns["mode_change"] = 0 // No cooldown for mode changes
	n.cooldowns["balance_low"] = 10 * time.Minute
	n.cooldowns["connection_lost"] = 2 * time.Minute
	n.cooldowns["connection_restored"] = 0
	n.cooldowns["daily_summary"] = 0

	if enabled {
		log.Printf("[Telegram] Notifier enabled for chat %s", cfg.ChatID)
	} else {
		log.Println("[Telegram] Notifier disabled (no bot token or chat ID)")
	}

	return n
}

// shouldSend checks if we should send this alert (rate limiting)
func (t *TelegramNotifier) shouldSend(alertKey string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	cooldown := t.defaultCooldown
	if cd, ok := t.cooldowns[alertKey]; ok {
		cooldown = cd
	}

	// No cooldown - always send
	if cooldown == 0 {
		return true
	}

	lastSent, ok := t.lastSent[alertKey]
	if !ok || time.Since(lastSent) >= cooldown {
		t.lastSent[alertKey] = time.Now()
		return true
	}

	return false
}

// send sends a message to Telegram
func (t *TelegramNotifier) send(text string) error {
	if !t.enabled {
		return nil
	}

	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", t.botToken)

	payload := map[string]interface{}{
		"chat_id":    t.chatID,
		"text":       text,
		"parse_mode": "HTML",
	}

	body, _ := json.Marshal(payload)
	resp, err := t.httpClient.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("telegram send failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram returned status %d", resp.StatusCode)
	}

	return nil
}

// formatHeader creates message header with bot info
func (t *TelegramNotifier) formatHeader(level AlertLevel) string {
	emoji := ""
	switch level {
	case AlertInfo:
		emoji = "ℹ️"
	case AlertWarning:
		emoji = "⚠️"
	case AlertCritical:
		emoji = "🚨"
	}

	return fmt.Sprintf("%s <b>[%s]</b> %s | %s | <code>%s</code>", emoji, level, t.exchange, t.symbol, t.botID)
}

// ========================================
// Alert Functions - Cases to Notify
// ========================================

// NotifyFill sends fill notification
// Case: Order was filled (partial or full)
func (t *TelegramNotifier) NotifyFill(side string, price, qty, notional float64, isFull bool, orderID string) {
	if !t.shouldSend("fill") {
		return
	}

	fillType := "Partial Fill"
	emoji := "📊"
	if isFull {
		fillType = "Fill"
		emoji = "✅"
	}

	sideEmoji := "🟢" // BUY
	if side == "SELL" {
		sideEmoji = "🔴"
	}

	msg := fmt.Sprintf(`%s
%s <b>%s</b> %s

%s %s @ %.8f
Qty: %.6f
Notional: $%.2f
Order: <code>%s</code>
Time: %s`,
		t.formatHeader(AlertInfo),
		emoji, fillType, sideEmoji,
		sideEmoji, side, price,
		qty, notional,
		orderID,
		time.Now().Format("15:04:05"))

	if err := t.send(msg); err != nil {
		log.Printf("[Telegram] Failed to send fill notification: %v", err)
	}
}

// NotifyDrawdownWarning sends drawdown warning
// Case: Drawdown exceeds warning threshold (e.g., 3%)
func (t *TelegramNotifier) NotifyDrawdownWarning(drawdownPct, nav, peakNAV float64) {
	if !t.shouldSend("drawdown_warning") {
		return
	}

	msg := fmt.Sprintf(`%s

📉 <b>Drawdown Warning</b>

Drawdown: %.2f%%
Current NAV: $%.2f
Peak NAV: $%.2f
Loss: $%.2f`,
		t.formatHeader(AlertWarning),
		drawdownPct*100, nav, peakNAV, peakNAV-nav)

	if err := t.send(msg); err != nil {
		log.Printf("[Telegram] Failed to send drawdown warning: %v", err)
	}
}

// NotifyDrawdownCritical sends critical drawdown alert
// Case: Drawdown exceeds critical threshold (e.g., 5%) - bot paused
func (t *TelegramNotifier) NotifyDrawdownCritical(drawdownPct, nav, peakNAV float64) {
	if !t.shouldSend("drawdown_critical") {
		return
	}

	msg := fmt.Sprintf(`%s

🛑 <b>CRITICAL: Bot Paused</b>

Drawdown: %.2f%%
Current NAV: $%.2f
Peak NAV: $%.2f
Loss: $%.2f

All orders cancelled. Manual intervention required.`,
		t.formatHeader(AlertCritical),
		drawdownPct*100, nav, peakNAV, peakNAV-nav)

	if err := t.send(msg); err != nil {
		log.Printf("[Telegram] Failed to send critical drawdown alert: %v", err)
	}
}

// NotifyModeChange sends mode change notification
// Case: Bot changes operating mode (Normal → Warning → Recovery → Paused)
func (t *TelegramNotifier) NotifyModeChange(oldMode, newMode string, reason string) {
	if !t.shouldSend("mode_change") {
		return
	}

	level := AlertInfo
	emoji := "🔄"
	if newMode == "PAUSED" {
		level = AlertCritical
		emoji = "🛑"
	} else if newMode == "RECOVERY" {
		level = AlertWarning
		emoji = "⚠️"
	}

	msg := fmt.Sprintf(`%s
%s <b>Mode Changed</b>

From: %s
To: <b>%s</b>
Reason: %s`,
		t.formatHeader(level),
		emoji, oldMode, newMode, reason)

	if err := t.send(msg); err != nil {
		log.Printf("[Telegram] Failed to send mode change notification: %v", err)
	}
}

// NotifyBalanceLow sends low balance warning
// Case: Available balance drops below minimum trading threshold
func (t *TelegramNotifier) NotifyBalanceLow(asset string, available, required float64) {
	if !t.shouldSend("balance_low") {
		return
	}

	msg := fmt.Sprintf(`%s

💰 <b>Low Balance Warning</b>

Asset: %s
Available: %.6f
Required: %.6f
Deficit: %.6f`,
		t.formatHeader(AlertWarning),
		asset, available, required, required-available)

	if err := t.send(msg); err != nil {
		log.Printf("[Telegram] Failed to send balance warning: %v", err)
	}
}

// NotifyConnectionLost sends connection lost alert
// Case: WebSocket connection lost
func (t *TelegramNotifier) NotifyConnectionLost(connectionType string, err error) {
	if !t.shouldSend("connection_lost") {
		return
	}

	errMsg := "Unknown"
	if err != nil {
		errMsg = err.Error()
	}

	msg := fmt.Sprintf(`%s

🔌 <b>Connection Lost</b>

Type: %s
Error: %s
Time: %s

Attempting to reconnect...`,
		t.formatHeader(AlertWarning),
		connectionType, errMsg, time.Now().Format("15:04:05"))

	if err := t.send(msg); err != nil {
		log.Printf("[Telegram] Failed to send connection lost alert: %v", err)
	}
}

// NotifyConnectionRestored sends connection restored notification
// Case: WebSocket connection restored after disconnect
func (t *TelegramNotifier) NotifyConnectionRestored(connectionType string, downtime time.Duration) {
	if !t.shouldSend("connection_restored") {
		return
	}

	msg := fmt.Sprintf(`%s

✅ <b>Connection Restored</b>

Type: %s
Downtime: %s
Time: %s`,
		t.formatHeader(AlertInfo),
		connectionType, downtime.Round(time.Second), time.Now().Format("15:04:05"))

	if err := t.send(msg); err != nil {
		log.Printf("[Telegram] Failed to send connection restored notification: %v", err)
	}
}

// NotifyBotStarted sends bot started notification
// Case: Bot successfully started
func (t *TelegramNotifier) NotifyBotStarted(botType string, config map[string]interface{}) {
	msg := fmt.Sprintf(`%s

🚀 <b>Bot Started</b>

Type: %s
Exchange: %s
Symbol: %s
Bot ID: %s
Time: %s`,
		t.formatHeader(AlertInfo),
		botType, t.exchange, t.symbol, t.botID,
		time.Now().Format("2006-01-02 15:04:05"))

	if err := t.send(msg); err != nil {
		log.Printf("[Telegram] Failed to send bot started notification: %v", err)
	}
}

// NotifyBotStopped sends bot stopped notification
// Case: Bot stopped (graceful shutdown or error)
func (t *TelegramNotifier) NotifyBotStopped(reason string, runtime time.Duration) {
	msg := fmt.Sprintf(`%s

🛑 <b>Bot Stopped</b>

Reason: %s
Runtime: %s
Time: %s`,
		t.formatHeader(AlertInfo),
		reason, runtime.Round(time.Second),
		time.Now().Format("2006-01-02 15:04:05"))

	if err := t.send(msg); err != nil {
		log.Printf("[Telegram] Failed to send bot stopped notification: %v", err)
	}
}

// NotifyDailySummary sends daily performance summary
// Case: Daily scheduled summary (e.g., at midnight)
func (t *TelegramNotifier) NotifyDailySummary(stats DailySummaryStats) {
	if !t.shouldSend("daily_summary") {
		return
	}

	pnlEmoji := "📈"
	if stats.PnL < 0 {
		pnlEmoji = "📉"
	}

	msg := fmt.Sprintf(`%s

📊 <b>Daily Summary</b>

%s PnL: $%.2f (%.2f%%)
Fills: %d (Buy: %d, Sell: %d)
Volume: $%.2f
Fees: $%.2f

NAV: $%.2f
Peak: $%.2f
Max Drawdown: %.2f%%

Date: %s`,
		t.formatHeader(AlertInfo),
		pnlEmoji, stats.PnL, stats.PnLPct*100,
		stats.TotalFills, stats.BuyFills, stats.SellFills,
		stats.TotalVolume, stats.TotalFees,
		stats.CurrentNAV, stats.PeakNAV, stats.MaxDrawdownPct*100,
		time.Now().Format("2006-01-02"))

	if err := t.send(msg); err != nil {
		log.Printf("[Telegram] Failed to send daily summary: %v", err)
	}
}

// NotifyError sends error notification
// Case: Critical error occurred
func (t *TelegramNotifier) NotifyError(errType string, err error, context string) {
	if !t.shouldSend("error_" + errType) {
		return
	}

	msg := fmt.Sprintf(`%s

❌ <b>Error: %s</b>

%v

Context: %s
Time: %s`,
		t.formatHeader(AlertCritical),
		errType, err, context,
		time.Now().Format("15:04:05"))

	if err := t.send(msg); err != nil {
		log.Printf("[Telegram] Failed to send error notification: %v", err)
	}
}

// DailySummaryStats holds daily performance statistics
type DailySummaryStats struct {
	PnL            float64
	PnLPct         float64
	TotalFills     int
	BuyFills       int
	SellFills      int
	TotalVolume    float64
	TotalFees      float64
	CurrentNAV     float64
	PeakNAV        float64
	MaxDrawdownPct float64
}
