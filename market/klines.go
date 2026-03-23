package market

import "fmt"

// GetRecentCompletedKlines 获取最近N根已完成K线
func GetRecentCompletedKlines(symbol, interval string, limit int) ([]Kline, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("limit必须大于0")
	}

	klines, err := getKlines(symbol, interval, limit+3)
	if err != nil {
		return nil, err
	}

	completed := filterCompletedKlines(klines)
	if len(completed) < limit {
		return nil, fmt.Errorf("已完成K线不足: 需要%d根，实际%d根", limit, len(completed))
	}

	return completed[len(completed)-limit:], nil
}

// GetCurrentKline 获取当前所在周期的最新K线（可能未完成）
func GetCurrentKline(symbol, interval string) (*Kline, error) {
	klines, err := getKlines(symbol, interval, 1)
	if err != nil {
		return nil, err
	}
	if len(klines) == 0 {
		return nil, fmt.Errorf("未获取到K线")
	}
	return &klines[len(klines)-1], nil
}
