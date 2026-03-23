package trader

import (
	"testing"
	"time"
)

func TestCalculateRiskOrderUsesMinStopLoss(t *testing.T) {
	result, err := CalculateRiskOrder(RiskOrderCalcInput{
		Equity:              1000,
		ReferencePrice:      100,
		StructuralStopRatio: 0.002,
	})
	if err != nil {
		t.Fatalf("CalculateRiskOrder error = %v", err)
	}

	if result.ActualStopLossRatio != 0.005 {
		t.Fatalf("actual stop ratio = %v, want 0.005", result.ActualStopLossRatio)
	}
	if result.Margin != 100 {
		t.Fatalf("margin = %v, want 100", result.Margin)
	}
	if result.Notional != 10000 {
		t.Fatalf("notional = %v, want 10000", result.Notional)
	}
	if result.PositionMultiple != 10 {
		t.Fatalf("position multiple = %v, want 10", result.PositionMultiple)
	}
}

func TestComputeRiskRequestSignatureDeterministic(t *testing.T) {
	body := []byte(`{"symbol":"BTCUSDT","side":"SHORT"}`)
	sig1 := ComputeRiskRequestSignature("secret", "POST", "/api/risk/order", "1740000000000", "nonce-1", body)
	sig2 := ComputeRiskRequestSignature("secret", "POST", "/api/risk/order", "1740000000000", "nonce-1", body)

	if sig1 == "" {
		t.Fatal("signature is empty")
	}
	if sig1 != sig2 {
		t.Fatalf("signature mismatch: %s != %s", sig1, sig2)
	}
}

func TestValidateRiskTimestamp(t *testing.T) {
	now := time.Unix(1740000000, 0)
	if err := ValidateRiskTimestamp("1740000000000", now); err != nil {
		t.Fatalf("unexpected timestamp validation error: %v", err)
	}
	if err := ValidateRiskTimestamp("1739999900000", now); err == nil {
		t.Fatal("expected stale timestamp to fail")
	}
}
