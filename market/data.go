package market

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Data 市场数据结构
type Data struct {
	Symbol            string
	CurrentPrice      float64
	PriceChange1h     float64 // 1小时价格变化百分比
	PriceChange4h     float64 // 4小时价格变化百分比
	OpenInterest      *OIData
	FundingRate       float64
	LongerTermContext *LongerTermData
	MA21_4h           float64   // 4小时MA21
	MA21_4hSeries     []float64 // 4小时MA21序列（最近3个，用于趋势判断）
	MA15_15m          float64   // 15分钟MA15
}

// OIData Open Interest数据
type OIData struct {
	Latest  float64
	Average float64
}

// LongerTermData 长期数据(4小时时间框架)
type LongerTermData struct {
	EMA20         float64
	EMA50         float64
	ATR3          float64
	ATR14         float64
	CurrentVolume float64
	AverageVolume float64
	MACDValues    []float64
	RSI14Values   []float64
}

// Kline K线数据
type Kline struct {
	OpenTime  int64
	Open      float64
	High      float64
	Low       float64
	Close     float64
	Volume    float64
	CloseTime int64
}

// BinanceError Binance API错误响应结构
type BinanceError struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
}

// Get 获取指定代币的市场数据
func Get(symbol string) (*Data, error) {
	// 标准化symbol
	symbol = Normalize(symbol)

	// 获取4小时K线数据 (最近10个)
	klines4h, err := getKlines(symbol, "4h", 60) // 多获取用于计算指标
	if err != nil {
		return nil, fmt.Errorf("获取4小时K线失败: %v", err)
	}
	// 过滤掉未走完的4小时K线
	klines4h = filterCompletedKlines(klines4h)

	// 获取15分钟K线数据 (用于计算MA15和当前价格)
	klines15m, err := getKlines(symbol, "15m", 40)
	if err != nil {
		return nil, fmt.Errorf("获取15分钟K线失败: %v", err)
	}
	// 过滤掉未走完的15分钟K线
	klines15m = filterCompletedKlines(klines15m)

	// 计算当前指标 (基于15分钟最新数据)
	currentPrice := klines15m[len(klines15m)-1].Close

	// 计算价格变化百分比
	// 1小时价格变化 = 4个15分钟K线前的价格
	priceChange1h := 0.0
	if len(klines15m) >= 5 { // 至少需要5根K线 (当前 + 4根前)
		price1hAgo := klines15m[len(klines15m)-5].Close
		if price1hAgo > 0 {
			priceChange1h = ((currentPrice - price1hAgo) / price1hAgo) * 100
		}
	}

	// 4小时价格变化 = 1个4小时K线前的价格
	priceChange4h := 0.0
	if len(klines4h) >= 2 {
		price4hAgo := klines4h[len(klines4h)-2].Close
		if price4hAgo > 0 {
			priceChange4h = ((currentPrice - price4hAgo) / price4hAgo) * 100
		}
	}

	// 获取OI数据
	oiData, err := getOpenInterestData(symbol)
	if err != nil {
		// OI失败不影响整体,使用默认值
		oiData = &OIData{Latest: 0, Average: 0}
	}

	// 获取Funding Rate
	fundingRate, _ := getFundingRate(symbol)

	// 计算长期数据
	longerTermData := calculateLongerTermData(klines4h)

	// 计算MA21_4h (4小时21期简单移动平均线)
	ma21_4h := calculateSMA(klines4h, 21)

	// 计算MA21_4h序列（最近3个值，用于趋势判断）
	ma21_4hSeries := make([]float64, 0, 3)
	if len(klines4h) >= 23 { // 至少需要23根K线来计算3个MA21值
		for i := len(klines4h) - 3; i < len(klines4h); i++ {
			ma21_4hSeries = append(ma21_4hSeries, calculateSMA(klines4h[:i+1], 21))
		}
	}

	// 计算MA15_15m (15分钟15期简单移动平均线)
	ma15_15m := calculateSMA(klines15m, 15)

	return &Data{
		Symbol:            symbol,
		CurrentPrice:      currentPrice,
		PriceChange1h:     priceChange1h,
		PriceChange4h:     priceChange4h,
		OpenInterest:      oiData,
		FundingRate:       fundingRate,
		LongerTermContext: longerTermData,
		MA21_4h:           ma21_4h,
		MA21_4hSeries:     ma21_4hSeries,
		MA15_15m:          ma15_15m,
	}, nil
}

// getKlines 从Binance获取K线数据
func getKlines(symbol, interval string, limit int) ([]Kline, error) {
	url := fmt.Sprintf("https://fapi.binance.com/fapi/v1/klines?symbol=%s&interval=%s&limit=%d",
		symbol, interval, limit)

	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// Check if response is an error object first
	var binanceErr BinanceError
	if err := json.Unmarshal(body, &binanceErr); err == nil && binanceErr.Code != 0 {
		return nil, fmt.Errorf("Binance API Error %d: %s", binanceErr.Code, binanceErr.Msg)
	}

	// Parse klines data if not an error
	var rawData [][]interface{}
	if err := json.Unmarshal(body, &rawData); err != nil {
		return nil, fmt.Errorf("Failed to parse klines data: %v", err)
	}

	klines := make([]Kline, len(rawData))
	for i, item := range rawData {
		openTime := int64(item[0].(float64))
		open, _ := parseFloat(item[1])
		high, _ := parseFloat(item[2])
		low, _ := parseFloat(item[3])
		close, _ := parseFloat(item[4])
		volume, _ := parseFloat(item[5])
		closeTime := int64(item[6].(float64))

		klines[i] = Kline{
			OpenTime:  openTime,
			Open:      open,
			High:      high,
			Low:       low,
			Close:     close,
			Volume:    volume,
			CloseTime: closeTime,
		}
	}

	return klines, nil
}

// calculateEMA 计算EMA
func calculateEMA(klines []Kline, period int) float64 {
	if len(klines) < period {
		return 0
	}

	// 计算SMA作为初始EMA
	sum := 0.0
	for i := 0; i < period; i++ {
		sum += klines[i].Close
	}
	ema := sum / float64(period)

	// 计算EMA
	multiplier := 2.0 / float64(period+1)
	for i := period; i < len(klines); i++ {
		ema = (klines[i].Close-ema)*multiplier + ema
	}

	return ema
}

// calculateSMA 计算简单移动平均线(Simple Moving Average)
func calculateSMA(klines []Kline, period int) float64 {
	if len(klines) < period {
		return 0
	}

	sum := 0.0
	for i := len(klines) - period; i < len(klines); i++ {
		sum += klines[i].Close
	}
	return sum / float64(period)
}

// calculateMACD 计算MACD
func calculateMACD(klines []Kline) float64 {
	if len(klines) < 26 {
		return 0
	}

	// 计算12期和26期EMA
	ema12 := calculateEMA(klines, 12)
	ema26 := calculateEMA(klines, 26)

	// MACD = EMA12 - EMA26
	return ema12 - ema26
}

// calculateRSI 计算RSI
func calculateRSI(klines []Kline, period int) float64 {
	if len(klines) <= period {
		return 0
	}

	gains := 0.0
	losses := 0.0

	// 计算初始平均涨跌幅
	for i := 1; i <= period; i++ {
		change := klines[i].Close - klines[i-1].Close
		if change > 0 {
			gains += change
		} else {
			losses += -change
		}
	}

	avgGain := gains / float64(period)
	avgLoss := losses / float64(period)

	// 使用Wilder平滑方法计算后续RSI
	for i := period + 1; i < len(klines); i++ {
		change := klines[i].Close - klines[i-1].Close
		if change > 0 {
			avgGain = (avgGain*float64(period-1) + change) / float64(period)
			avgLoss = (avgLoss * float64(period-1)) / float64(period)
		} else {
			avgGain = (avgGain * float64(period-1)) / float64(period)
			avgLoss = (avgLoss*float64(period-1) + (-change)) / float64(period)
		}
	}

	if avgLoss == 0 {
		return 100
	}

	rs := avgGain / avgLoss
	rsi := 100 - (100 / (1 + rs))

	return rsi
}

// calculateATR 计算ATR
func calculateATR(klines []Kline, period int) float64 {
	if len(klines) <= period {
		return 0
	}

	trs := make([]float64, len(klines))
	for i := 1; i < len(klines); i++ {
		high := klines[i].High
		low := klines[i].Low
		prevClose := klines[i-1].Close

		tr1 := high - low
		tr2 := math.Abs(high - prevClose)
		tr3 := math.Abs(low - prevClose)

		trs[i] = math.Max(tr1, math.Max(tr2, tr3))
	}

	// 计算初始ATR
	sum := 0.0
	for i := 1; i <= period; i++ {
		sum += trs[i]
	}
	atr := sum / float64(period)

	// Wilder平滑
	for i := period + 1; i < len(klines); i++ {
		atr = (atr*float64(period-1) + trs[i]) / float64(period)
	}

	return atr
}

// calculateLongerTermData 计算长期数据
func calculateLongerTermData(klines []Kline) *LongerTermData {
	data := &LongerTermData{
		MACDValues:  make([]float64, 0, 10),
		RSI14Values: make([]float64, 0, 10),
	}

	// 计算EMA
	data.EMA20 = calculateEMA(klines, 20)
	data.EMA50 = calculateEMA(klines, 50)

	// 计算ATR
	data.ATR3 = calculateATR(klines, 3)
	data.ATR14 = calculateATR(klines, 14)

	// 计算成交量
	if len(klines) > 0 {
		data.CurrentVolume = klines[len(klines)-1].Volume
		// 计算平均成交量
		sum := 0.0
		for _, k := range klines {
			sum += k.Volume
		}
		data.AverageVolume = sum / float64(len(klines))
	}

	// 计算MACD和RSI序列
	start := len(klines) - 10
	if start < 0 {
		start = 0
	}

	for i := start; i < len(klines); i++ {
		if i >= 25 {
			macd := calculateMACD(klines[:i+1])
			data.MACDValues = append(data.MACDValues, macd)
		}
		if i >= 14 {
			rsi14 := calculateRSI(klines[:i+1], 14)
			data.RSI14Values = append(data.RSI14Values, rsi14)
		}
	}

	return data
}

// getOpenInterestData 获取OI数据
func getOpenInterestData(symbol string) (*OIData, error) {
	url := fmt.Sprintf("https://fapi.binance.com/fapi/v1/openInterest?symbol=%s", symbol)

	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var result struct {
		OpenInterest string `json:"openInterest"`
		Symbol       string `json:"symbol"`
		Time         int64  `json:"time"`
	}

	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}

	oi, _ := strconv.ParseFloat(result.OpenInterest, 64)

	return &OIData{
		Latest:  oi,
		Average: oi * 0.999, // 近似平均值
	}, nil
}

// getFundingRate 获取资金费率
func getFundingRate(symbol string) (float64, error) {
	url := fmt.Sprintf("https://fapi.binance.com/fapi/v1/premiumIndex?symbol=%s", symbol)

	resp, err := http.Get(url)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}

	var result struct {
		Symbol          string `json:"symbol"`
		MarkPrice       string `json:"markPrice"`
		IndexPrice      string `json:"indexPrice"`
		LastFundingRate string `json:"lastFundingRate"`
		NextFundingTime int64  `json:"nextFundingTime"`
		InterestRate    string `json:"interestRate"`
		Time            int64  `json:"time"`
	}

	if err := json.Unmarshal(body, &result); err != nil {
		return 0, err
	}

	rate, _ := strconv.ParseFloat(result.LastFundingRate, 64)
	return rate, nil
}

// Format 格式化输出市场数据
func Format(data *Data) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("current_price = %.2f\n\n", data.CurrentPrice))

	// 添加MA21_4h和趋势信息
	sb.WriteString(fmt.Sprintf("MA21_4h: %.2f\n", data.MA21_4h))
	if len(data.MA21_4hSeries) >= 3 {
		trend := "横盘"
		if isRising(data.MA21_4hSeries) {
			trend = "上涨"
		} else if isFalling(data.MA21_4hSeries) {
			trend = "下跌"
		}
		sb.WriteString(fmt.Sprintf("4小时趋势(MA21连续3): %s (序列: %s)\n", trend, formatFloatSlice(data.MA21_4hSeries)))
	}

	// 添加MA15_15m和价格距离
	sb.WriteString(fmt.Sprintf("MA15_15m: %.2f\n", data.MA15_15m))
	priceToMA15Dist := ((data.CurrentPrice - data.MA15_15m) / data.MA15_15m) * 100
	sb.WriteString(fmt.Sprintf("价格与MA15_15m距离: %.2f%%\n\n", priceToMA15Dist))

	sb.WriteString(fmt.Sprintf("In addition, here is the latest %s open interest and funding rate for perps:\n\n",
		data.Symbol))

	if data.OpenInterest != nil {
		sb.WriteString(fmt.Sprintf("Open Interest: Latest: %.2f Average: %.2f\n\n",
			data.OpenInterest.Latest, data.OpenInterest.Average))
	}

	sb.WriteString(fmt.Sprintf("Funding Rate: %.2e\n\n", data.FundingRate))

	if data.LongerTermContext != nil {
		sb.WriteString("Longer‑term context (4‑hour timeframe):\n\n")

		sb.WriteString(fmt.Sprintf("20‑Period EMA: %.3f vs. 50‑Period EMA: %.3f\n\n",
			data.LongerTermContext.EMA20, data.LongerTermContext.EMA50))

		sb.WriteString(fmt.Sprintf("3‑Period ATR: %.3f vs. 14‑Period ATR: %.3f\n\n",
			data.LongerTermContext.ATR3, data.LongerTermContext.ATR14))

		sb.WriteString(fmt.Sprintf("Current Volume: %.3f vs. Average Volume: %.3f\n\n",
			data.LongerTermContext.CurrentVolume, data.LongerTermContext.AverageVolume))

		if len(data.LongerTermContext.MACDValues) > 0 {
			sb.WriteString(fmt.Sprintf("MACD indicators: %s\n\n", formatFloatSlice(data.LongerTermContext.MACDValues)))
		}

		if len(data.LongerTermContext.RSI14Values) > 0 {
			sb.WriteString(fmt.Sprintf("RSI indicators (14‑Period): %s\n\n", formatFloatSlice(data.LongerTermContext.RSI14Values)))
		}
	}

	return sb.String()
}

// formatFloatSlice 格式化float64切片为字符串
func formatFloatSlice(values []float64) string {
	strValues := make([]string, len(values))
	for i, v := range values {
		strValues[i] = fmt.Sprintf("%.3f", v)
	}
	return "[" + strings.Join(strValues, ", ") + "]"
}

// Normalize 标准化symbol,确保是USDT交易对
func Normalize(symbol string) string {
	symbol = strings.ToUpper(symbol)
	if strings.HasSuffix(symbol, "USDT") {
		return symbol
	}
	return symbol + "USDT"
}

// parseFloat 解析float值
func parseFloat(v interface{}) (float64, error) {
	switch val := v.(type) {
	case string:
		return strconv.ParseFloat(val, 64)
	case float64:
		return val, nil
	case int:
		return float64(val), nil
	case int64:
		return float64(val), nil
	default:
		return 0, fmt.Errorf("unsupported type: %T", v)
	}
}

// isRising 判断序列是否连续上升
func isRising(series []float64) bool {
	if len(series) < 2 {
		return false
	}
	for i := 1; i < len(series); i++ {
		if series[i] <= series[i-1] {
			return false
		}
	}
	return true
}

// isFalling 判断序列是否连续下降
func isFalling(series []float64) bool {
	if len(series) < 2 {
		return false
	}
	for i := 1; i < len(series); i++ {
		if series[i] >= series[i-1] {
			return false
		}
	}
	return true
}

// CheckKlineCompleteness 检查15分钟K线是否走完
// 返回true表示K线已完成，可以用于决策
func CheckKlineCompleteness() bool {
	// 获取当前时间
	now := time.Now()

	// 当前分钟数（0-59）
	currentMinute := now.Minute()

	// 计算当前15分钟周期的开始时间
	// 例如：如果现在是10:37，当前周期是10:30-10:45
	klineStartMinute := (currentMinute / 15) * 15
	klineStartTime := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), klineStartMinute, 0, 0, now.Location())

	// 计算K线结束时间
	klineEndTime := klineStartTime.Add(15 * time.Minute)

	// 如果当前时间已经达到或超过K线结束时间，说明K线已完成
	return now.Equal(klineEndTime) || now.After(klineEndTime)
}

// filterCompletedKlines 过滤掉未走完的K线
// 返回只包含已收盘K线的数组
func filterCompletedKlines(klines []Kline) []Kline {
	if len(klines) == 0 {
		return klines
	}

	// 获取当前时间戳（毫秒）
	now := time.Now().UnixMilli()

	// 过滤掉 CloseTime > now 的K线（未走完的K线）
	completed := make([]Kline, 0, len(klines))
	for _, k := range klines {
		// 如果K线的收盘时间 <= 当前时间，说明K线已走完
		if k.CloseTime <= now {
			completed = append(completed, k)
		}
	}

	return completed
}
