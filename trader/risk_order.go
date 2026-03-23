package trader

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"strings"
	"time"
)

const (
	riskRequestMaxSkew       = 30 * time.Second
	riskOrderTimeframe       = "15m"
	riskOrderLookbackCandles = 2
	riskMaxLossRatio         = 0.05
	riskMinStopLossRatio     = 0.005
	riskEntryOffsetRatio     = 0.0005
	riskTakeProfitRatio      = 0.04
	riskFixedLeverage        = 100
)

type RiskOrderRequest struct {
	Symbol        string `json:"symbol"`
	Side          string `json:"side"`
	ClientOrderID string `json:"client_order_id"`
}

type RiskOrderResponse struct {
	Accepted                bool    `json:"accepted"`
	RejectReason            string  `json:"reject_reason,omitempty"`
	TraderID                string  `json:"trader_id"`
	Symbol                  string  `json:"symbol"`
	Side                    string  `json:"side"`
	Equity                  float64 `json:"equity"`
	MaxLossAmount           float64 `json:"max_loss_amount"`
	ReferencePrice          float64 `json:"reference_price"`
	EntryPrice              float64 `json:"entry_price"`
	EntryOrderPrice         float64 `json:"entry_order_price"`
	StopLossPrice           float64 `json:"stop_loss_price"`
	TakeProfitPrice         float64 `json:"take_profit_price"`
	StructuralStopLossRatio float64 `json:"structural_stop_loss_ratio"`
	ActualStopLossRatio     float64 `json:"actual_stop_loss_ratio"`
	Margin                  float64 `json:"margin"`
	Notional                float64 `json:"notional"`
	PositionMultiple        float64 `json:"position_multiple"`
	Quantity                float64 `json:"quantity"`
	FilledQuantity          float64 `json:"filled_quantity"`
	Leverage                int     `json:"leverage"`
	OrderID                 int64   `json:"order_id,omitempty"`
	StopOrderPlaced         bool    `json:"stop_order_placed"`
	TakeProfitOrderPlaced   bool    `json:"take_profit_order_placed"`
	ProtectionMode          string  `json:"protection_mode,omitempty"`
	ProtectionTaskID        string  `json:"protection_task_id,omitempty"`
}

func NormalizeRiskSide(side string) (string, error) {
	switch strings.ToUpper(strings.TrimSpace(side)) {
	case "LONG", "BUY":
		return "LONG", nil
	case "SHORT", "SELL":
		return "SHORT", nil
	default:
		return "", fmt.Errorf("side必须是LONG或SHORT")
	}
}

func ComputeRiskRequestSignature(secret, method, path, timestamp, nonce string, body []byte) string {
	bodyHash := sha256.Sum256(body)
	payload := strings.Join([]string{
		strings.ToUpper(method),
		path,
		timestamp,
		nonce,
		hex.EncodeToString(bodyHash[:]),
	}, "\n")

	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

func ValidateRiskTimestamp(timestamp string, now time.Time) error {
	reqTime, err := parseRiskTimestamp(timestamp)
	if err != nil {
		return err
	}

	diff := now.Sub(reqTime)
	if diff > riskRequestMaxSkew || diff < -riskRequestMaxSkew {
		return fmt.Errorf("timestamp超出允许时间窗口")
	}
	return nil
}

func parseRiskTimestamp(timestamp string) (time.Time, error) {
	ts, err := time.Parse(time.RFC3339Nano, timestamp)
	if err == nil {
		return ts, nil
	}

	ms, err := time.ParseDuration(timestamp + "ms")
	if err != nil {
		return time.Time{}, fmt.Errorf("timestamp格式无效")
	}
	return time.Unix(0, ms.Nanoseconds()), nil
}

type RiskOrderCalcInput struct {
	Equity              float64
	ReferencePrice      float64
	StructuralStopRatio float64
}

type RiskOrderCalcResult struct {
	MaxLossAmount           float64
	ActualStopLossRatio     float64
	Margin                  float64
	Notional                float64
	PositionMultiple        float64
	Quantity                float64
	StructuralStopLossRatio float64
}

func CalculateRiskOrder(input RiskOrderCalcInput) (*RiskOrderCalcResult, error) {
	if input.Equity <= 0 {
		return nil, fmt.Errorf("equity必须大于0")
	}
	if input.ReferencePrice <= 0 {
		return nil, fmt.Errorf("reference price必须大于0")
	}
	if input.StructuralStopRatio < 0 {
		return nil, fmt.Errorf("structural stop ratio不能小于0")
	}

	actualStopRatio := math.Max(input.StructuralStopRatio, riskMinStopLossRatio)
	maxLossAmount := input.Equity * riskMaxLossRatio
	margin := maxLossAmount / (actualStopRatio * riskFixedLeverage)
	notional := margin * riskFixedLeverage
	quantity := notional / input.ReferencePrice

	return &RiskOrderCalcResult{
		MaxLossAmount:           maxLossAmount,
		ActualStopLossRatio:     actualStopRatio,
		Margin:                  margin,
		Notional:                notional,
		PositionMultiple:        notional / input.Equity,
		Quantity:                quantity,
		StructuralStopLossRatio: input.StructuralStopRatio,
	}, nil
}
