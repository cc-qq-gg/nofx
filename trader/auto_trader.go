package trader

import (
	"encoding/json"
	"fmt"
	"log"
	"nofx/decision"
	"nofx/logger"
	"nofx/market"
	"nofx/mcp"
	"nofx/pool"
	"sort"
	"strings"
	"sync"
	"time"
)

// AutoTraderConfig 自动交易配置（简化版 - AI全权决策）
type AutoTraderConfig struct {
	// Trader标识
	ID      string // Trader唯一标识（用于日志目录等）
	Name    string // Trader显示名称
	AIModel string // AI模型: "qwen" 或 "deepseek"

	// 交易平台选择
	Exchange string // "binance", "hyperliquid" 或 "aster"

	// 币安API配置
	BinanceAPIKey    string
	BinanceSecretKey string

	// Hyperliquid配置
	HyperliquidPrivateKey string
	HyperliquidWalletAddr string
	HyperliquidTestnet    bool

	// Aster配置
	AsterUser       string // Aster主钱包地址
	AsterSigner     string // Aster API钱包地址
	AsterPrivateKey string // Aster API钱包私钥

	CoinPoolAPIURL string

	// AI配置
	UseQwen     bool
	DeepSeekKey string
	QwenKey     string

	// 自定义AI API配置
	CustomAPIURL    string
	CustomAPIKey    string
	CustomModelName string

	// 手动风控下单接口认证
	RiskAPIKey    string
	RiskAPISecret string

	// 扫描配置
	ScanInterval time.Duration // 扫描间隔（建议3分钟）

	// 账户配置
	InitialBalance float64 // 初始金额（用于计算盈亏，需手动设置）

	// 杠杆配置
	BTCETHLeverage  int // BTC和ETH的杠杆倍数
	AltcoinLeverage int // 山寨币的杠杆倍数

	// 风险控制（仅作为提示，AI可自主决定）
	MaxDailyLoss    float64       // 最大日亏损百分比（提示）
	MaxDrawdown     float64       // 最大回撤百分比（提示）
	StopTradingTime time.Duration // 触发风控后暂停时长
}

// AutoTrader 自动交易器
type AutoTrader struct {
	id                    string // Trader唯一标识
	name                  string // Trader显示名称
	aiModel               string // AI模型名称
	exchange              string // 交易平台名称
	config                AutoTraderConfig
	trader                Trader // 使用Trader接口（支持多平台）
	mcpClient             *mcp.Client
	decisionLogger        *logger.DecisionLogger // 决策日志记录器
	initialBalance        float64
	dailyPnL              float64
	lastResetTime         time.Time
	stopUntil             time.Time
	isRunning             bool
	startTime             time.Time        // 系统启动时间
	callCount             int              // AI调用次数
	positionFirstSeenTime map[string]int64 // 持仓首次出现时间 (symbol_side -> timestamp毫秒)
	protectionMu          sync.Mutex
	protectionTasks       map[string]contextProtectionTask
	triggerMu             sync.Mutex
	triggerTasks          map[string]riskTriggerTask
}

type contextProtectionTask struct {
	ID                    string
	Symbol                string
	Side                  string
	Quantity              float64
	EntryPrice            float64
	BaseStopLossPrice     float64
	BaseTakeProfitPrice   float64
	StopLossPrice         float64
	TakeProfitPrice       float64
	AppliedProtectionStep int
	CreatedAt             time.Time
}

type riskTriggerTask struct {
	ID               string
	Symbol           string
	Side             string
	TriggerPrice     float64
	TriggerDirection string
	ClientOrderID    string
	Status           string
	ExpireAt         time.Time
	CreatedAt        time.Time
}

const protectionQuantityEpsilon = 1e-9
const protectionMaxStep = 20

// NewAutoTrader 创建自动交易器
func NewAutoTrader(config AutoTraderConfig) (*AutoTrader, error) {
	// 设置默认值
	if config.ID == "" {
		config.ID = "default_trader"
	}
	if config.Name == "" {
		config.Name = "Default Trader"
	}
	if config.AIModel == "" {
		if config.UseQwen {
			config.AIModel = "qwen"
		} else {
			config.AIModel = "deepseek"
		}
	}

	mcpClient := mcp.New()

	// 初始化AI
	if config.AIModel == "custom" {
		// 使用自定义API
		mcpClient.SetCustomAPI(config.CustomAPIURL, config.CustomAPIKey, config.CustomModelName)
		log.Printf("🤖 [%s] 使用自定义AI API: %s (模型: %s)", config.Name, config.CustomAPIURL, config.CustomModelName)
	} else if config.UseQwen || config.AIModel == "qwen" {
		// 使用Qwen
		mcpClient.SetQwenAPIKey(config.QwenKey, "")
		log.Printf("🤖 [%s] 使用阿里云Qwen AI", config.Name)
	} else {
		// 默认使用DeepSeek
		mcpClient.SetDeepSeekAPIKey(config.DeepSeekKey)
		log.Printf("🤖 [%s] 使用DeepSeek AI", config.Name)
	}

	// 初始化币种池API
	if config.CoinPoolAPIURL != "" {
		pool.SetCoinPoolAPI(config.CoinPoolAPIURL)
	}

	// 设置默认交易平台
	if config.Exchange == "" {
		config.Exchange = "binance"
	}

	// 根据配置创建对应的交易器
	var trader Trader
	var err error

	switch config.Exchange {
	case "binance":
		log.Printf("🏦 [%s] 使用币安合约交易", config.Name)
		trader = NewFuturesTrader(config.BinanceAPIKey, config.BinanceSecretKey)
	case "hyperliquid":
		log.Printf("🏦 [%s] 使用Hyperliquid交易", config.Name)
		trader, err = NewHyperliquidTrader(config.HyperliquidPrivateKey, config.HyperliquidWalletAddr, config.HyperliquidTestnet)
		if err != nil {
			return nil, fmt.Errorf("初始化Hyperliquid交易器失败: %w", err)
		}
	case "aster":
		log.Printf("🏦 [%s] 使用Aster交易", config.Name)
		trader, err = NewAsterTrader(config.AsterUser, config.AsterSigner, config.AsterPrivateKey)
		if err != nil {
			return nil, fmt.Errorf("初始化Aster交易器失败: %w", err)
		}
	default:
		return nil, fmt.Errorf("不支持的交易平台: %s", config.Exchange)
	}

	// 验证初始金额配置
	if config.InitialBalance <= 0 {
		return nil, fmt.Errorf("初始金额必须大于0，请在配置中设置InitialBalance")
	}

	// 初始化决策日志记录器（使用trader ID创建独立目录）
	logDir := fmt.Sprintf("decision_logs/%s", config.ID)
	decisionLogger := logger.NewDecisionLogger(logDir)

	return &AutoTrader{
		id:                    config.ID,
		name:                  config.Name,
		aiModel:               config.AIModel,
		exchange:              config.Exchange,
		config:                config,
		trader:                trader,
		mcpClient:             mcpClient,
		decisionLogger:        decisionLogger,
		initialBalance:        config.InitialBalance,
		lastResetTime:         time.Now(),
		startTime:             time.Now(),
		callCount:             0,
		isRunning:             false,
		positionFirstSeenTime: make(map[string]int64),
		protectionTasks:       make(map[string]contextProtectionTask),
		triggerTasks:          make(map[string]riskTriggerTask),
	}, nil
}

// Run 运行自动交易主循环
func (at *AutoTrader) Run() error {
	at.isRunning = true
	log.Println("🚀 AI驱动自动交易系统启动")
	log.Printf("💰 初始余额: %.2f USDT", at.initialBalance)
	log.Printf("⚙️  扫描间隔: %v", at.config.ScanInterval)
	log.Println("🤖 AI将全权决定杠杆、仓位大小、止损止盈等参数")

	ticker := time.NewTicker(at.config.ScanInterval)
	defer ticker.Stop()

	// 首次立即执行
	if err := at.runCycle(); err != nil {
		log.Printf("❌ 执行失败: %v", err)
	}

	for at.isRunning {
		select {
		case <-ticker.C:
			if err := at.runCycle(); err != nil {
				log.Printf("❌ 执行失败: %v", err)
			}
		}
	}

	return nil
}

// Stop 停止自动交易
func (at *AutoTrader) Stop() {
	at.isRunning = false
	at.clearProtectionTasks()
	at.clearTriggerTasks()
	log.Println("⏹ 自动交易系统停止")
}

// runCycle 运行一个交易周期（使用AI全权决策）
func (at *AutoTrader) runCycle() error {
	at.callCount++

	log.Print("\n" + strings.Repeat("=", 70))
	log.Printf("⏰ %s - AI决策周期 #%d", time.Now().Format("2006-01-02 15:04:05"), at.callCount)
	log.Print(strings.Repeat("=", 70))

	// 检查15分钟K线是否走完
	if !market.CheckKlineCompleteness() {
		log.Printf("⏸ K线完整性检查：15分钟K线未走完，跳过本次决策")
		return nil
	}

	// 创建决策记录
	record := &logger.DecisionRecord{
		ExecutionLog: []string{},
		Success:      true,
	}

	// 1. 检查是否需要停止交易
	if time.Now().Before(at.stopUntil) {
		remaining := at.stopUntil.Sub(time.Now())
		log.Printf("⏸ 风险控制：暂停交易中，剩余 %.0f 分钟", remaining.Minutes())
		record.Success = false
		record.ErrorMessage = fmt.Sprintf("风险控制暂停中，剩余 %.0f 分钟", remaining.Minutes())
		at.decisionLogger.LogDecision(record)
		return nil
	}

	// 2. 重置日盈亏（每天重置）
	if time.Since(at.lastResetTime) > 24*time.Hour {
		at.dailyPnL = 0
		at.lastResetTime = time.Now()
		log.Println("📅 日盈亏已重置")
	}

	// 3. 收集交易上下文
	ctx, err := at.buildTradingContext()
	if err != nil {
		record.Success = false
		record.ErrorMessage = fmt.Sprintf("构建交易上下文失败: %v", err)
		at.decisionLogger.LogDecision(record)
		return fmt.Errorf("构建交易上下文失败: %w", err)
	}

	// 保存账户状态快照
	record.AccountState = logger.AccountSnapshot{
		TotalBalance:          ctx.Account.TotalEquity,
		AvailableBalance:      ctx.Account.AvailableBalance,
		TotalUnrealizedProfit: ctx.Account.TotalPnL,
		PositionCount:         ctx.Account.PositionCount,
		MarginUsedPct:         ctx.Account.MarginUsedPct,
	}

	// 保存持仓快照
	for _, pos := range ctx.Positions {
		record.Positions = append(record.Positions, logger.PositionSnapshot{
			Symbol:           pos.Symbol,
			Side:             pos.Side,
			PositionAmt:      pos.Quantity,
			EntryPrice:       pos.EntryPrice,
			MarkPrice:        pos.MarkPrice,
			UnrealizedProfit: pos.UnrealizedPnL,
			Leverage:         float64(pos.Leverage),
			LiquidationPrice: pos.LiquidationPrice,
		})
	}

	// 保存候选币种列表
	for _, coin := range ctx.CandidateCoins {
		record.CandidateCoins = append(record.CandidateCoins, coin.Symbol)
	}

	log.Printf("📊 账户净值: %.2f USDT | 可用: %.2f USDT | 持仓: %d",
		ctx.Account.TotalEquity, ctx.Account.AvailableBalance, ctx.Account.PositionCount)

	// 4. 调用AI获取完整决策
	log.Println("🤖 正在请求AI分析并决策...")
	decision, err := decision.GetFullDecision(ctx, at.mcpClient)

	// 即使有错误，也保存思维链、决策和输入prompt（用于debug）
	if decision != nil {
		record.InputPrompt = decision.UserPrompt
		record.CoTTrace = decision.CoTTrace
		if len(decision.Decisions) > 0 {
			decisionJSON, _ := json.MarshalIndent(decision.Decisions, "", "  ")
			record.DecisionJSON = string(decisionJSON)
		}
	}

	if err != nil {
		record.Success = false
		record.ErrorMessage = fmt.Sprintf("获取AI决策失败: %v", err)

		// 打印AI思维链（即使有错误）
		if decision != nil && decision.CoTTrace != "" {
			log.Print("\n" + strings.Repeat("-", 70))
			log.Println("💭 AI思维链分析（错误情况）:")
			log.Println(strings.Repeat("-", 70))
			log.Println(decision.CoTTrace)
			log.Print(strings.Repeat("-", 70) + "\n")
		}

		at.decisionLogger.LogDecision(record)
		return fmt.Errorf("获取AI决策失败: %w", err)
	}

	// 5. 打印AI思维链
	log.Print("\n" + strings.Repeat("-", 70))
	log.Println("💭 AI思维链分析:")
	log.Println(strings.Repeat("-", 70))
	log.Println(decision.CoTTrace)
	log.Print(strings.Repeat("-", 70) + "\n")

	// 6. 打印AI决策
	log.Printf("📋 AI决策列表 (%d 个):\n", len(decision.Decisions))
	for i, d := range decision.Decisions {
		log.Printf("  [%d] %s: %s - %s", i+1, d.Symbol, d.Action, d.Reasoning)
		if d.Action == "open_long" || d.Action == "open_short" {
			log.Printf("      杠杆: %dx | 仓位: %.2f USDT | 止损: %.4f | 止盈: %.4f",
				d.Leverage, d.PositionSizeUSD, d.StopLoss, d.TakeProfit)
		}
	}
	log.Println()

	// 7. 对决策排序：确保先平仓后开仓（防止仓位叠加超限）
	sortedDecisions := sortDecisionsByPriority(decision.Decisions)

	log.Println("🔄 执行顺序（已优化）: 先平仓→后开仓")
	for i, d := range sortedDecisions {
		log.Printf("  [%d] %s %s", i+1, d.Symbol, d.Action)
	}
	log.Println()

	// 执行决策并记录结果
	for _, d := range sortedDecisions {
		actionRecord := logger.DecisionAction{
			Action:    d.Action,
			Symbol:    d.Symbol,
			Quantity:  0,
			Leverage:  d.Leverage,
			Price:     0,
			Timestamp: time.Now(),
			Success:   false,
		}

		if err := at.executeDecisionWithRecord(&d, &actionRecord); err != nil {
			log.Printf("❌ 执行决策失败 (%s %s): %v", d.Symbol, d.Action, err)
			actionRecord.Error = err.Error()
			record.ExecutionLog = append(record.ExecutionLog, fmt.Sprintf("❌ %s %s 失败: %v", d.Symbol, d.Action, err))
		} else {
			actionRecord.Success = true
			record.ExecutionLog = append(record.ExecutionLog, fmt.Sprintf("✓ %s %s 成功", d.Symbol, d.Action))
			// 成功执行后短暂延迟
			time.Sleep(1 * time.Second)
		}

		record.Decisions = append(record.Decisions, actionRecord)
	}

	// 8. 保存决策记录
	if err := at.decisionLogger.LogDecision(record); err != nil {
		log.Printf("⚠ 保存决策记录失败: %v", err)
	}

	return nil
}

// buildTradingContext 构建交易上下文
func (at *AutoTrader) buildTradingContext() (*decision.Context, error) {
	// 1. 获取账户信息
	balance, err := at.trader.GetBalance()
	if err != nil {
		return nil, fmt.Errorf("获取账户余额失败: %w", err)
	}

	// 获取账户字段
	totalWalletBalance := 0.0
	totalUnrealizedProfit := 0.0
	availableBalance := 0.0

	if wallet, ok := balance["totalWalletBalance"].(float64); ok {
		totalWalletBalance = wallet
	}
	if unrealized, ok := balance["totalUnrealizedProfit"].(float64); ok {
		totalUnrealizedProfit = unrealized
	}
	if avail, ok := balance["availableBalance"].(float64); ok {
		availableBalance = avail
	}

	// Total Equity = 钱包余额 + 未实现盈亏
	totalEquity := totalWalletBalance + totalUnrealizedProfit

	// 2. 获取持仓信息
	positions, err := at.trader.GetPositions()
	if err != nil {
		return nil, fmt.Errorf("获取持仓失败: %w", err)
	}

	var positionInfos []decision.PositionInfo
	totalMarginUsed := 0.0

	// 当前持仓的key集合（用于清理已平仓的记录）
	currentPositionKeys := make(map[string]bool)

	for _, pos := range positions {
		symbol := pos["symbol"].(string)
		side := pos["side"].(string)
		entryPrice := pos["entryPrice"].(float64)
		markPrice := pos["markPrice"].(float64)
		quantity := pos["positionAmt"].(float64)
		if quantity < 0 {
			quantity = -quantity // 空仓数量为负，转为正数
		}
		unrealizedPnl := pos["unRealizedProfit"].(float64)
		liquidationPrice := pos["liquidationPrice"].(float64)

		// 计算占用保证金（估算）
		leverage := 10 // 默认值，实际应该从持仓信息获取
		if lev, ok := pos["leverage"].(float64); ok {
			leverage = int(lev)
		}
		marginUsed := (quantity * markPrice) / float64(leverage)
		totalMarginUsed += marginUsed

		// 计算盈亏百分比
		pnlPct := 0.0
		if side == "long" {
			pnlPct = ((markPrice - entryPrice) / entryPrice) * float64(leverage) * 100
		} else {
			pnlPct = ((entryPrice - markPrice) / entryPrice) * float64(leverage) * 100
		}

		// 跟踪持仓首次出现时间
		posKey := symbol + "_" + side
		currentPositionKeys[posKey] = true
		if _, exists := at.positionFirstSeenTime[posKey]; !exists {
			// 新持仓，记录当前时间
			at.positionFirstSeenTime[posKey] = time.Now().UnixMilli()
		}
		updateTime := at.positionFirstSeenTime[posKey]

		positionInfos = append(positionInfos, decision.PositionInfo{
			Symbol:           symbol,
			Side:             side,
			EntryPrice:       entryPrice,
			MarkPrice:        markPrice,
			Quantity:         quantity,
			Leverage:         leverage,
			UnrealizedPnL:    unrealizedPnl,
			UnrealizedPnLPct: pnlPct,
			LiquidationPrice: liquidationPrice,
			MarginUsed:       marginUsed,
			UpdateTime:       updateTime,
		})
	}

	// 清理已平仓的持仓记录
	for key := range at.positionFirstSeenTime {
		if !currentPositionKeys[key] {
			delete(at.positionFirstSeenTime, key)
		}
	}

	// 3. 获取合并的候选币种池（AI500 + OI Top，去重）
	// 无论有没有持仓，都分析相同数量的币种（让AI看到所有好机会）
	// AI会根据保证金使用率和现有持仓情况，自己决定是否要换仓
	const ai500Limit = 20 // AI500取前20个评分最高的币种

	// 获取合并后的币种池（AI500 + OI Top）
	mergedPool, err := pool.GetMergedCoinPool(ai500Limit)
	if err != nil {
		return nil, fmt.Errorf("获取合并币种池失败: %w", err)
	}

	// 构建候选币种列表（包含来源信息）
	var candidateCoins []decision.CandidateCoin
	for _, symbol := range mergedPool.AllSymbols {
		sources := mergedPool.SymbolSources[symbol]
		candidateCoins = append(candidateCoins, decision.CandidateCoin{
			Symbol:  symbol,
			Sources: sources, // "ai500" 和/或 "oi_top"
		})
	}

	log.Printf("📋 合并币种池: AI500前%d + OI_Top20 = 总计%d个候选币种",
		ai500Limit, len(candidateCoins))

	// 4. 计算总盈亏
	totalPnL := totalEquity - at.initialBalance
	totalPnLPct := 0.0
	if at.initialBalance > 0 {
		totalPnLPct = (totalPnL / at.initialBalance) * 100
	}

	marginUsedPct := 0.0
	if totalEquity > 0 {
		marginUsedPct = (totalMarginUsed / totalEquity) * 100
	}

	// 5. 分析历史表现（最近100个周期，避免长期持仓的交易记录丢失）
	// 假设每3分钟一个周期，100个周期 = 5小时，足够覆盖大部分交易
	performance, err := at.decisionLogger.AnalyzePerformance(100)
	if err != nil {
		log.Printf("⚠️  分析历史表现失败: %v", err)
		// 不影响主流程，继续执行（但设置performance为nil以避免传递错误数据）
		performance = nil
	}

	// 6. 构建上下文
	ctx := &decision.Context{
		CurrentTime:     time.Now().Format("2006-01-02 15:04:05"),
		RuntimeMinutes:  int(time.Since(at.startTime).Minutes()),
		CallCount:       at.callCount,
		BTCETHLeverage:  at.config.BTCETHLeverage,  // 使用配置的杠杆倍数
		AltcoinLeverage: at.config.AltcoinLeverage, // 使用配置的杠杆倍数
		Account: decision.AccountInfo{
			TotalEquity:      totalEquity,
			AvailableBalance: availableBalance,
			TotalPnL:         totalPnL,
			TotalPnLPct:      totalPnLPct,
			MarginUsed:       totalMarginUsed,
			MarginUsedPct:    marginUsedPct,
			PositionCount:    len(positionInfos),
		},
		Positions:      positionInfos,
		CandidateCoins: candidateCoins,
		Performance:    performance, // 添加历史表现分析
	}

	return ctx, nil
}

// executeDecisionWithRecord 执行AI决策并记录详细信息
func (at *AutoTrader) executeDecisionWithRecord(decision *decision.Decision, actionRecord *logger.DecisionAction) error {
	switch decision.Action {
	case "open_long":
		return at.executeOpenLongWithRecord(decision, actionRecord)
	case "open_short":
		return at.executeOpenShortWithRecord(decision, actionRecord)
	case "close_long":
		return at.executeCloseLongWithRecord(decision, actionRecord)
	case "close_short":
		return at.executeCloseShortWithRecord(decision, actionRecord)
	case "hold", "wait":
		// 无需执行，仅记录
		return nil
	default:
		return fmt.Errorf("未知的action: %s", decision.Action)
	}
}

// executeOpenLongWithRecord 执行开多仓并记录详细信息
func (at *AutoTrader) executeOpenLongWithRecord(decision *decision.Decision, actionRecord *logger.DecisionAction) error {
	log.Printf("  📈 开多仓: %s", decision.Symbol)

	// ⚠️ 关键：检查是否已有同币种同方向持仓，如果有则拒绝开仓（防止仓位叠加超限）
	positions, err := at.trader.GetPositions()
	if err == nil {
		for _, pos := range positions {
			if pos["symbol"] == decision.Symbol && pos["side"] == "long" {
				return fmt.Errorf("❌ %s 已有多仓，拒绝开仓以防止仓位叠加超限。如需换仓，请先给出 close_long 决策", decision.Symbol)
			}
		}
	}

	// 获取当前价格
	marketData, err := market.Get(decision.Symbol)
	if err != nil {
		return err
	}

	// 计算数量
	quantity := decision.PositionSizeUSD / marketData.CurrentPrice
	actionRecord.Quantity = quantity
	actionRecord.Price = marketData.CurrentPrice

	// 开仓
	order, err := at.trader.OpenLong(decision.Symbol, quantity, decision.Leverage)
	if err != nil {
		return err
	}

	// 记录订单ID
	if orderID, ok := order["orderId"].(int64); ok {
		actionRecord.OrderID = orderID
	}

	log.Printf("  ✓ 开仓成功，订单ID: %v, 数量: %.4f", order["orderId"], quantity)

	// 记录开仓时间
	posKey := decision.Symbol + "_long"
	at.positionFirstSeenTime[posKey] = time.Now().UnixMilli()

	// 设置止损止盈
	if err := at.trader.SetStopLoss(decision.Symbol, "LONG", quantity, decision.StopLoss); err != nil {
		log.Printf("  ⚠ 设置止损失败: %v", err)
	}
	if err := at.trader.SetTakeProfit(decision.Symbol, "LONG", quantity, decision.TakeProfit); err != nil {
		log.Printf("  ⚠ 设置止盈失败: %v", err)
	}

	return nil
}

// executeOpenShortWithRecord 执行开空仓并记录详细信息
func (at *AutoTrader) executeOpenShortWithRecord(decision *decision.Decision, actionRecord *logger.DecisionAction) error {
	log.Printf("  📉 开空仓: %s", decision.Symbol)

	// ⚠️ 关键：检查是否已有同币种同方向持仓，如果有则拒绝开仓（防止仓位叠加超限）
	positions, err := at.trader.GetPositions()
	if err == nil {
		for _, pos := range positions {
			if pos["symbol"] == decision.Symbol && pos["side"] == "short" {
				return fmt.Errorf("❌ %s 已有空仓，拒绝开仓以防止仓位叠加超限。如需换仓，请先给出 close_short 决策", decision.Symbol)
			}
		}
	}

	// 获取当前价格
	marketData, err := market.Get(decision.Symbol)
	if err != nil {
		return err
	}

	// 计算数量
	quantity := decision.PositionSizeUSD / marketData.CurrentPrice
	actionRecord.Quantity = quantity
	actionRecord.Price = marketData.CurrentPrice

	// 开仓
	order, err := at.trader.OpenShort(decision.Symbol, quantity, decision.Leverage)
	if err != nil {
		return err
	}

	// 记录订单ID
	if orderID, ok := order["orderId"].(int64); ok {
		actionRecord.OrderID = orderID
	}

	log.Printf("  ✓ 开仓成功，订单ID: %v, 数量: %.4f", order["orderId"], quantity)

	// 记录开仓时间
	posKey := decision.Symbol + "_short"
	at.positionFirstSeenTime[posKey] = time.Now().UnixMilli()

	// 设置止损止盈
	if err := at.trader.SetStopLoss(decision.Symbol, "SHORT", quantity, decision.StopLoss); err != nil {
		log.Printf("  ⚠ 设置止损失败: %v", err)
	}
	if err := at.trader.SetTakeProfit(decision.Symbol, "SHORT", quantity, decision.TakeProfit); err != nil {
		log.Printf("  ⚠ 设置止盈失败: %v", err)
	}

	return nil
}

// executeCloseLongWithRecord 执行平多仓并记录详细信息
func (at *AutoTrader) executeCloseLongWithRecord(decision *decision.Decision, actionRecord *logger.DecisionAction) error {
	log.Printf("  🔄 平多仓: %s", decision.Symbol)

	// 获取当前价格
	marketData, err := market.Get(decision.Symbol)
	if err != nil {
		return err
	}
	actionRecord.Price = marketData.CurrentPrice

	// 平仓
	order, err := at.trader.CloseLong(decision.Symbol, 0) // 0 = 全部平仓
	if err != nil {
		return err
	}

	// 记录订单ID
	if orderID, ok := order["orderId"].(int64); ok {
		actionRecord.OrderID = orderID
	}

	log.Printf("  ✓ 平仓成功")
	return nil
}

// executeCloseShortWithRecord 执行平空仓并记录详细信息
func (at *AutoTrader) executeCloseShortWithRecord(decision *decision.Decision, actionRecord *logger.DecisionAction) error {
	log.Printf("  🔄 平空仓: %s", decision.Symbol)

	// 获取当前价格
	marketData, err := market.Get(decision.Symbol)
	if err != nil {
		return err
	}
	actionRecord.Price = marketData.CurrentPrice

	// 平仓
	order, err := at.trader.CloseShort(decision.Symbol, 0) // 0 = 全部平仓
	if err != nil {
		return err
	}

	// 记录订单ID
	if orderID, ok := order["orderId"].(int64); ok {
		actionRecord.OrderID = orderID
	}

	log.Printf("  ✓ 平仓成功")
	return nil
}

// GetID 获取trader ID
func (at *AutoTrader) GetID() string {
	return at.id
}

// GetName 获取trader名称
func (at *AutoTrader) GetName() string {
	return at.name
}

// GetAIModel 获取AI模型
func (at *AutoTrader) GetAIModel() string {
	return at.aiModel
}

// GetDecisionLogger 获取决策日志记录器
func (at *AutoTrader) GetDecisionLogger() *logger.DecisionLogger {
	return at.decisionLogger
}

// GetStatus 获取系统状态（用于API）
func (at *AutoTrader) GetStatus() map[string]interface{} {
	aiProvider := "DeepSeek"
	if at.config.UseQwen {
		aiProvider = "Qwen"
	}

	return map[string]interface{}{
		"trader_id":       at.id,
		"trader_name":     at.name,
		"ai_model":        at.aiModel,
		"exchange":        at.exchange,
		"is_running":      at.isRunning,
		"start_time":      at.startTime.Format(time.RFC3339),
		"runtime_minutes": int(time.Since(at.startTime).Minutes()),
		"call_count":      at.callCount,
		"initial_balance": at.initialBalance,
		"scan_interval":   at.config.ScanInterval.String(),
		"stop_until":      at.stopUntil.Format(time.RFC3339),
		"last_reset_time": at.lastResetTime.Format(time.RFC3339),
		"ai_provider":     aiProvider,
	}
}

// ValidateRiskRequest 验证风控下单请求签名
func (at *AutoTrader) ValidateRiskRequest(method, path, apiKey, timestamp, nonce, signature string, body []byte) error {
	if at.config.RiskAPIKey == "" || at.config.RiskAPISecret == "" {
		return fmt.Errorf("该trader未配置risk_api_key/risk_api_secret")
	}
	if apiKey != at.config.RiskAPIKey {
		return fmt.Errorf("API Key无效")
	}
	if nonce == "" {
		return fmt.Errorf("nonce不能为空")
	}
	if err := ValidateRiskTimestamp(timestamp, time.Now()); err != nil {
		return err
	}

	expected := ComputeRiskRequestSignature(at.config.RiskAPISecret, method, path, timestamp, nonce, body)
	if !strings.EqualFold(signature, expected) {
		return fmt.Errorf("signature无效")
	}
	return nil
}

// ExecuteRiskOrder 执行手动风控下单
func (at *AutoTrader) ExecuteRiskOrder(req *RiskOrderRequest) (*RiskOrderResponse, error) {
	side, err := NormalizeRiskSide(req.Side)
	if err != nil {
		return nil, err
	}
	log.Printf("🚀 收到手动风控下单请求: trader=%s symbol=%s side=%s clientOrderID=%s",
		at.id, req.Symbol, side, req.ClientOrderID)

	if req.Price > 0 {
		return at.registerTriggerRiskOrder(req, side)
	}

	return at.executeImmediateRiskOrder(req, side)
}

func (at *AutoTrader) executeImmediateRiskOrder(req *RiskOrderRequest, side string) (*RiskOrderResponse, error) {
	log.Printf("⚡ 执行即时风控下单: trader=%s symbol=%s side=%s clientOrderID=%s",
		at.id, req.Symbol, side, req.ClientOrderID)

	binanceTrader, ok := at.trader.(*FuturesTrader)
	if !ok {
		return nil, fmt.Errorf("当前trader仅支持币安风控下单接口")
	}

	account, err := at.GetAccountInfo()
	if err != nil {
		return nil, fmt.Errorf("获取账户信息失败: %w", err)
	}

	equity, _ := account["total_equity"].(float64)
	availableBalance, _ := account["available_balance"].(float64)
	if equity <= 0 {
		return nil, fmt.Errorf("账户净值无效")
	}

	if err := at.ensureNoSymbolPosition(req.Symbol); err != nil {
		return nil, err
	}

	bestBid, bestAsk, err := binanceTrader.GetBestBidAsk(req.Symbol)
	if err != nil {
		return nil, fmt.Errorf("获取盘口失败: %w", err)
	}

	referencePrice := bestAsk
	if side == "SHORT" {
		referencePrice = bestBid
	}
	if referencePrice <= 0 {
		return nil, fmt.Errorf("参考价格无效")
	}
	log.Printf("📚 下单参考盘口: symbol=%s side=%s bid=%.8f ask=%.8f reference=%.8f",
		req.Symbol, side, bestBid, bestAsk, referencePrice)

	structuralStopRatio, err := calculateStructuralStopRatio(req.Symbol, side, referencePrice)
	if err != nil {
		return nil, err
	}

	calc, err := CalculateRiskOrder(RiskOrderCalcInput{
		Equity:              equity,
		ReferencePrice:      referencePrice,
		StructuralStopRatio: structuralStopRatio,
	})
	if err != nil {
		return nil, err
	}
	if calc.Margin > availableBalance {
		return nil, fmt.Errorf("所需保证金%.4f超过可用余额%.4f", calc.Margin, availableBalance)
	}
	log.Printf("🧮 风控计算结果: symbol=%s side=%s equity=%.8f available=%.8f structuralStop=%.6f actualStop=%.6f margin=%.8f notional=%.8f quantity=%.8f",
		req.Symbol, side, equity, availableBalance, structuralStopRatio, calc.ActualStopLossRatio, calc.Margin, calc.Notional, calc.Quantity)

	entryOrderPrice := referencePrice * (1 + riskEntryOffsetRatio)
	if side == "SHORT" {
		entryOrderPrice = referencePrice * (1 - riskEntryOffsetRatio)
	}
	log.Printf("📝 IOC激进限价: symbol=%s side=%s entryOrderPrice=%.8f offset=%.4f%%",
		req.Symbol, side, entryOrderPrice, riskEntryOffsetRatio*100)

	orderResult, err := binanceTrader.PlaceAggressiveRiskEntry(req.Symbol, side, calc.Quantity, riskFixedLeverage, entryOrderPrice)
	if err != nil {
		return nil, err
	}

	filledQuantity, _ := orderResult["executedQty"].(float64)
	avgFillPrice, _ := orderResult["avgPrice"].(float64)
	orderID, _ := orderResult["orderId"].(int64)
	log.Printf("📦 风控下单结果: symbol=%s side=%s orderId=%d status=%v filledQty=%.8f avgPrice=%.8f",
		req.Symbol, side, orderID, orderResult["status"], filledQuantity, avgFillPrice)

	if filledQuantity <= 0 || avgFillPrice <= 0 {
		log.Printf("⚠ IOC最终未成交: symbol=%s side=%s orderId=%d", req.Symbol, side, orderID)
		return &RiskOrderResponse{
			Accepted:                false,
			RejectReason:            "IOC订单未成交",
			TraderID:                at.id,
			Symbol:                  req.Symbol,
			Side:                    side,
			Equity:                  equity,
			MaxLossAmount:           calc.MaxLossAmount,
			ReferencePrice:          referencePrice,
			EntryOrderPrice:         entryOrderPrice,
			StructuralStopLossRatio: calc.StructuralStopLossRatio,
			ActualStopLossRatio:     calc.ActualStopLossRatio,
			Margin:                  calc.Margin,
			Notional:                calc.Notional,
			PositionMultiple:        calc.PositionMultiple,
			Quantity:                calc.Quantity,
			Leverage:                riskFixedLeverage,
			OrderID:                 orderID,
		}, nil
	}

	stopLossPrice := avgFillPrice * (1 - calc.ActualStopLossRatio)
	takeProfitPrice := avgFillPrice * (1 + riskTakeProfitRatio)
	if side == "SHORT" {
		stopLossPrice = avgFillPrice * (1 + calc.ActualStopLossRatio)
		takeProfitPrice = avgFillPrice * (1 - riskTakeProfitRatio)
	}
	log.Printf("🛡 保护单参数: symbol=%s side=%s filledQty=%.8f avgPrice=%.8f stopLoss=%.8f takeProfit=%.8f",
		req.Symbol, side, filledQuantity, avgFillPrice, stopLossPrice, takeProfitPrice)

	taskID, err := at.registerLocalProtection(req.Symbol, side, filledQuantity, avgFillPrice, stopLossPrice, takeProfitPrice)
	if err != nil {
		log.Printf("⚠ 风控下单后注册本地保护任务失败: %v", err)
		return &RiskOrderResponse{
			Accepted:                true,
			TraderID:                at.id,
			Symbol:                  req.Symbol,
			Side:                    side,
			Equity:                  equity,
			MaxLossAmount:           calc.MaxLossAmount,
			ReferencePrice:          referencePrice,
			EntryPrice:              avgFillPrice,
			EntryOrderPrice:         entryOrderPrice,
			StopLossPrice:           stopLossPrice,
			TakeProfitPrice:         takeProfitPrice,
			StructuralStopLossRatio: calc.StructuralStopLossRatio,
			ActualStopLossRatio:     calc.ActualStopLossRatio,
			Margin:                  calc.Margin,
			Notional:                calc.Notional,
			PositionMultiple:        calc.PositionMultiple,
			Quantity:                calc.Quantity,
			FilledQuantity:          filledQuantity,
			Leverage:                riskFixedLeverage,
			OrderID:                 orderID,
			StopOrderPlaced:         false,
			TakeProfitOrderPlaced:   false,
			ProtectionMode:          "local_monitor",
		}, nil
	}
	log.Printf("✅ 本地保护任务已注册: taskId=%s symbol=%s side=%s quantity=%.8f stopLoss=%.8f takeProfit=%.8f",
		taskID, req.Symbol, side, filledQuantity, stopLossPrice, takeProfitPrice)

	return &RiskOrderResponse{
		Accepted:                true,
		TraderID:                at.id,
		Symbol:                  req.Symbol,
		Side:                    side,
		Equity:                  equity,
		MaxLossAmount:           calc.MaxLossAmount,
		ReferencePrice:          referencePrice,
		EntryPrice:              avgFillPrice,
		EntryOrderPrice:         entryOrderPrice,
		StopLossPrice:           stopLossPrice,
		TakeProfitPrice:         takeProfitPrice,
		StructuralStopLossRatio: calc.StructuralStopLossRatio,
		ActualStopLossRatio:     calc.ActualStopLossRatio,
		Margin:                  calc.Margin,
		Notional:                calc.Notional,
		PositionMultiple:        calc.PositionMultiple,
		Quantity:                calc.Quantity,
		FilledQuantity:          filledQuantity,
		Leverage:                riskFixedLeverage,
		OrderID:                 orderID,
		StopOrderPlaced:         true,
		TakeProfitOrderPlaced:   true,
		ProtectionMode:          "local_monitor",
		ProtectionTaskID:        taskID,
	}, nil
}

func (at *AutoTrader) registerLocalProtection(symbol, side string, quantity, entryPrice, stopLossPrice, takeProfitPrice float64) (string, error) {
	if quantity <= 0 {
		return "", fmt.Errorf("保护数量必须大于0")
	}

	taskID := fmt.Sprintf("%s-%s-%d", at.id, strings.ToLower(symbol), time.Now().UnixNano())
	task := contextProtectionTask{
		ID:                    taskID,
		Symbol:                symbol,
		Side:                  side,
		Quantity:              quantity,
		EntryPrice:            entryPrice,
		BaseStopLossPrice:     stopLossPrice,
		BaseTakeProfitPrice:   takeProfitPrice,
		StopLossPrice:         stopLossPrice,
		TakeProfitPrice:       takeProfitPrice,
		AppliedProtectionStep: 0,
		CreatedAt:             time.Now(),
	}

	at.protectionMu.Lock()
	at.protectionTasks[taskID] = task
	at.protectionMu.Unlock()

	go at.runLocalProtection(task)
	return taskID, nil
}

func (at *AutoTrader) registerTriggerRiskOrder(req *RiskOrderRequest, side string) (*RiskOrderResponse, error) {
	binanceTrader, ok := at.trader.(*FuturesTrader)
	if !ok {
		return nil, fmt.Errorf("当前trader仅支持币安风控下单接口")
	}

	if err := at.ensureNoSymbolPosition(req.Symbol); err != nil {
		return nil, err
	}

	bestBid, bestAsk, err := binanceTrader.GetBestBidAsk(req.Symbol)
	if err != nil {
		return nil, fmt.Errorf("获取盘口失败: %w", err)
	}

	currentPrice := bestAsk
	if side == "SHORT" {
		currentPrice = bestBid
	}
	if currentPrice <= 0 {
		return nil, fmt.Errorf("参考价格无效")
	}

	triggerDirection := determineTriggerDirection(req.Price, currentPrice)
	if triggerDirection == "" {
		log.Printf("ℹ 触发价等于当前价，直接降级为即时下单: symbol=%s side=%s price=%.8f", req.Symbol, side, req.Price)
		return at.executeImmediateRiskOrder(req, side)
	}

	triggerTask, replacedTaskID := at.upsertTriggerTask(req, side, triggerDirection)
	expireAt := triggerTask.ExpireAt.Format(time.RFC3339)
	if replacedTaskID != "" {
		log.Printf("♻ 监听任务已替换: oldTaskId=%s newTaskId=%s symbol=%s side=%s triggerPrice=%.8f direction=%s",
			replacedTaskID, triggerTask.ID, triggerTask.Symbol, triggerTask.Side, triggerTask.TriggerPrice, triggerTask.TriggerDirection)
	} else {
		log.Printf("📝 监听任务已创建: taskId=%s symbol=%s side=%s triggerPrice=%.8f direction=%s expireAt=%s",
			triggerTask.ID, triggerTask.Symbol, triggerTask.Side, triggerTask.TriggerPrice, triggerTask.TriggerDirection, expireAt)
	}

	go at.runTriggerTask(triggerTask)

	return &RiskOrderResponse{
		Accepted:         true,
		TraderID:         at.id,
		Symbol:           req.Symbol,
		Side:             side,
		TriggerPrice:     req.Price,
		TriggerDirection: triggerDirection,
		TriggerStatus:    triggerTask.Status,
		TriggerExpireAt:  expireAt,
	}, nil
}

func determineTriggerDirection(targetPrice, currentPrice float64) string {
	switch {
	case targetPrice > currentPrice:
		return "up"
	case targetPrice < currentPrice:
		return "down"
	default:
		return ""
	}
}

func (at *AutoTrader) upsertTriggerTask(req *RiskOrderRequest, side, triggerDirection string) (riskTriggerTask, string) {
	at.triggerMu.Lock()
	defer at.triggerMu.Unlock()

	replacedTaskID := ""
	for taskID, task := range at.triggerTasks {
		if task.Symbol == req.Symbol && task.Side == side && task.Status == "pending" {
			replacedTaskID = taskID
			task.Status = "replaced"
			at.triggerTasks[taskID] = task
			delete(at.triggerTasks, taskID)
			break
		}
	}

	taskID := fmt.Sprintf("%s-%s-%s-%d", at.id, strings.ToLower(req.Symbol), strings.ToLower(side), time.Now().UnixNano())
	triggerTask := riskTriggerTask{
		ID:               taskID,
		Symbol:           req.Symbol,
		Side:             side,
		TriggerPrice:     req.Price,
		TriggerDirection: triggerDirection,
		ClientOrderID:    req.ClientOrderID,
		Status:           "pending",
		ExpireAt:         time.Now().Add(24 * time.Hour),
		CreatedAt:        time.Now(),
	}
	at.triggerTasks[taskID] = triggerTask
	return triggerTask, replacedTaskID
}

func (at *AutoTrader) runTriggerTask(task riskTriggerTask) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		currentTask, ok := at.getTriggerTask(task.ID)
		if !ok || currentTask.Status != "pending" {
			return
		}

		if time.Now().After(currentTask.ExpireAt) {
			at.updateTriggerTaskStatus(currentTask.ID, "expired")
			log.Printf("⏰ 监听任务已过期: taskId=%s symbol=%s side=%s triggerPrice=%.8f",
				currentTask.ID, currentTask.Symbol, currentTask.Side, currentTask.TriggerPrice)
			at.removeTriggerTask(currentTask.ID)
			return
		}

		price, err := at.trader.GetMarketPrice(currentTask.Symbol)
		if err != nil {
			log.Printf("⚠ 监听任务取价失败: taskId=%s symbol=%s err=%v", currentTask.ID, currentTask.Symbol, err)
			continue
		}

		triggered := false
		if currentTask.TriggerDirection == "up" && price >= currentTask.TriggerPrice {
			triggered = true
		}
		if currentTask.TriggerDirection == "down" && price <= currentTask.TriggerPrice {
			triggered = true
		}
		if !triggered {
			continue
		}

		at.updateTriggerTaskStatus(currentTask.ID, "triggered")
		log.Printf("🎯 监听任务触发: taskId=%s symbol=%s side=%s triggerPrice=%.8f marketPrice=%.8f direction=%s",
			currentTask.ID, currentTask.Symbol, currentTask.Side, currentTask.TriggerPrice, price, currentTask.TriggerDirection)

		req := &RiskOrderRequest{
			Symbol:        currentTask.Symbol,
			Side:          currentTask.Side,
			ClientOrderID: currentTask.ClientOrderID,
		}
		resp, err := at.executeImmediateRiskOrder(req, currentTask.Side)
		if err != nil {
			log.Printf("⚠ 监听任务触发后执行失败: taskId=%s symbol=%s side=%s err=%v",
				currentTask.ID, currentTask.Symbol, currentTask.Side, err)
			at.updateTriggerTaskStatus(currentTask.ID, "triggered_unfilled")
			at.removeTriggerTask(currentTask.ID)
			return
		}
		if !resp.Accepted {
			log.Printf("⚠ 监听任务触发后 IOC 未成交: taskId=%s symbol=%s side=%s rejectReason=%s",
				currentTask.ID, currentTask.Symbol, currentTask.Side, resp.RejectReason)
			at.updateTriggerTaskStatus(currentTask.ID, "triggered_unfilled")
			at.removeTriggerTask(currentTask.ID)
			return
		}

		at.updateTriggerTaskStatus(currentTask.ID, "filled")
		log.Printf("✅ 监听任务触发后下单成功: taskId=%s symbol=%s side=%s protectionTaskId=%s",
			currentTask.ID, currentTask.Symbol, currentTask.Side, resp.ProtectionTaskID)
		at.removeTriggerTask(currentTask.ID)
		return
	}
}

func (at *AutoTrader) runLocalProtection(task contextProtectionTask) {
	log.Printf("🛰 本地保护任务启动: taskId=%s symbol=%s side=%s quantity=%.8f entry=%.8f stopLoss=%.8f takeProfit=%.8f",
		task.ID, task.Symbol, task.Side, task.Quantity, task.EntryPrice, task.StopLossPrice, task.TakeProfitPrice)

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	pollCount := 0
	for range ticker.C {
		if err := at.syncProtectionTasksForPosition(task.Symbol, task.Side); err != nil {
			log.Printf("⚠ 本地保护持仓同步失败: taskId=%s symbol=%s side=%s err=%v", task.ID, task.Symbol, task.Side, err)
			continue
		}

		currentTask, ok := at.getProtectionTask(task.ID)
		if !ok {
			return
		}

		price, err := at.trader.GetMarketPrice(currentTask.Symbol)
		if err != nil {
			log.Printf("⚠ 本地保护任务取价失败: taskId=%s symbol=%s err=%v", currentTask.ID, currentTask.Symbol, err)
			continue
		}

		currentTask = at.advanceProtectionTask(currentTask, price)

		pollCount++
		if pollCount == 1 || pollCount%15 == 0 {
			log.Printf("📡 本地保护巡检: taskId=%s symbol=%s side=%s price=%.8f stopLoss=%.8f takeProfit=%.8f",
				currentTask.ID, currentTask.Symbol, currentTask.Side, price, currentTask.StopLossPrice, currentTask.TakeProfitPrice)
		}

		triggerReason := ""
		switch currentTask.Side {
		case "LONG":
			if price <= currentTask.StopLossPrice {
				triggerReason = "stop_loss"
			} else if price >= currentTask.TakeProfitPrice {
				triggerReason = "take_profit"
			}
		case "SHORT":
			if price >= currentTask.StopLossPrice {
				triggerReason = "stop_loss"
			} else if price <= currentTask.TakeProfitPrice {
				triggerReason = "take_profit"
			}
		default:
			log.Printf("⚠ 本地保护任务方向无效: taskId=%s side=%s", currentTask.ID, currentTask.Side)
			at.removeProtectionTask(currentTask.ID)
			return
		}

		if triggerReason == "" {
			continue
		}

		log.Printf("🚨 本地保护触发: taskId=%s symbol=%s side=%s reason=%s triggerPrice=%.8f marketPrice=%.8f quantity=%.8f",
			currentTask.ID, currentTask.Symbol, currentTask.Side, triggerReason, at.protectionTriggerPrice(currentTask, triggerReason), price, currentTask.Quantity)

		var closeErr error
		if currentTask.Side == "LONG" {
			_, closeErr = at.trader.CloseLong(currentTask.Symbol, currentTask.Quantity)
		} else {
			_, closeErr = at.trader.CloseShort(currentTask.Symbol, currentTask.Quantity)
		}
		if closeErr != nil {
			if at.isNoPositionError(closeErr) {
				log.Printf("🧹 本地保护触发前发现仓位已无效，任务取消: taskId=%s symbol=%s side=%s err=%v",
					currentTask.ID, currentTask.Symbol, currentTask.Side, closeErr)
				at.removeProtectionTask(currentTask.ID)
				return
			}
			log.Printf("⚠ 本地保护执行平仓失败: taskId=%s symbol=%s side=%s reason=%s err=%v",
				currentTask.ID, currentTask.Symbol, currentTask.Side, triggerReason, closeErr)
			continue
		}

		log.Printf("✅ 本地保护执行成功: taskId=%s symbol=%s side=%s reason=%s quantity=%.8f",
			currentTask.ID, currentTask.Symbol, currentTask.Side, triggerReason, currentTask.Quantity)
		at.removeProtectionTask(currentTask.ID)
		return
	}
}

func (at *AutoTrader) protectionTriggerPrice(task contextProtectionTask, reason string) float64 {
	if reason == "stop_loss" {
		return task.StopLossPrice
	}
	return task.TakeProfitPrice
}

func (at *AutoTrader) advanceProtectionTask(task contextProtectionTask, price float64) contextProtectionTask {
	step, stopLossPrice, takeProfitPrice := computeDynamicProtection(task, price)
	if step <= task.AppliedProtectionStep {
		return task
	}

	updatedTask := task
	updatedTask.AppliedProtectionStep = step
	updatedTask.StopLossPrice = stopLossPrice
	updatedTask.TakeProfitPrice = takeProfitPrice

	at.protectionMu.Lock()
	if _, ok := at.protectionTasks[task.ID]; ok {
		at.protectionTasks[task.ID] = updatedTask
	}
	at.protectionMu.Unlock()

	log.Printf("📈 本地保护阶梯上调: taskId=%s symbol=%s side=%s step=%d price=%.8f stopLoss=%.8f takeProfit=%.8f",
		updatedTask.ID, updatedTask.Symbol, updatedTask.Side, updatedTask.AppliedProtectionStep, price, updatedTask.StopLossPrice, updatedTask.TakeProfitPrice)
	return updatedTask
}

func computeDynamicProtection(task contextProtectionTask, price float64) (int, float64, float64) {
	if task.EntryPrice <= 0 {
		return task.AppliedProtectionStep, task.StopLossPrice, task.TakeProfitPrice
	}

	favorableMoveRatio := 0.0
	switch task.Side {
	case "LONG":
		favorableMoveRatio = (price - task.EntryPrice) / task.EntryPrice
	case "SHORT":
		favorableMoveRatio = (task.EntryPrice - price) / task.EntryPrice
	default:
		return task.AppliedProtectionStep, task.StopLossPrice, task.TakeProfitPrice
	}

	if favorableMoveRatio < 0.01 {
		return task.AppliedProtectionStep, task.StopLossPrice, task.TakeProfitPrice
	}

	step := int(favorableMoveRatio * 100)
	if step > protectionMaxStep {
		step = protectionMaxStep
	}
	if step < 1 {
		return task.AppliedProtectionStep, task.StopLossPrice, task.TakeProfitPrice
	}

	stopMoveRatio := 0.004 + 0.009*float64(step-1)
	takeProfitMoveRatio := 0.025 + 0.01*float64(step-1)

	stopLossPrice := task.BaseStopLossPrice
	takeProfitPrice := task.BaseTakeProfitPrice

	if task.Side == "LONG" {
		stopLossPrice = task.EntryPrice * (1 + stopMoveRatio)
		takeProfitPrice = task.EntryPrice * (1 + takeProfitMoveRatio)
	}
	if task.Side == "SHORT" {
		stopLossPrice = task.EntryPrice * (1 - stopMoveRatio)
		takeProfitPrice = task.EntryPrice * (1 - takeProfitMoveRatio)
	}

	return step, stopLossPrice, takeProfitPrice
}

func (at *AutoTrader) getProtectionTask(taskID string) (contextProtectionTask, bool) {
	at.protectionMu.Lock()
	defer at.protectionMu.Unlock()
	task, ok := at.protectionTasks[taskID]
	return task, ok
}

func (at *AutoTrader) removeProtectionTask(taskID string) {
	at.protectionMu.Lock()
	defer at.protectionMu.Unlock()
	delete(at.protectionTasks, taskID)
}

func (at *AutoTrader) clearProtectionTasks() {
	at.protectionMu.Lock()
	defer at.protectionMu.Unlock()
	for taskID := range at.protectionTasks {
		delete(at.protectionTasks, taskID)
	}
}

func (at *AutoTrader) getTriggerTask(taskID string) (riskTriggerTask, bool) {
	at.triggerMu.Lock()
	defer at.triggerMu.Unlock()
	task, ok := at.triggerTasks[taskID]
	return task, ok
}

func (at *AutoTrader) updateTriggerTaskStatus(taskID, status string) {
	at.triggerMu.Lock()
	defer at.triggerMu.Unlock()
	task, ok := at.triggerTasks[taskID]
	if !ok {
		return
	}
	task.Status = status
	at.triggerTasks[taskID] = task
}

func (at *AutoTrader) removeTriggerTask(taskID string) {
	at.triggerMu.Lock()
	defer at.triggerMu.Unlock()
	delete(at.triggerTasks, taskID)
}

func (at *AutoTrader) clearTriggerTasks() {
	at.triggerMu.Lock()
	defer at.triggerMu.Unlock()
	for taskID := range at.triggerTasks {
		delete(at.triggerTasks, taskID)
	}
}

func (at *AutoTrader) syncProtectionTasksForPosition(symbol, side string) error {
	currentPositionQty, err := at.getCurrentPositionQuantity(symbol, side)
	if err != nil {
		return err
	}

	at.protectionMu.Lock()
	defer at.protectionMu.Unlock()

	var tasks []contextProtectionTask
	totalProtectedQty := 0.0
	for _, task := range at.protectionTasks {
		if task.Symbol == symbol && task.Side == side {
			tasks = append(tasks, task)
			totalProtectedQty += task.Quantity
		}
	}
	if len(tasks) == 0 {
		return nil
	}

	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].CreatedAt.Equal(tasks[j].CreatedAt) {
			return tasks[i].ID < tasks[j].ID
		}
		return tasks[i].CreatedAt.Before(tasks[j].CreatedAt)
	})

	log.Printf("🧭 本地保护持仓同步: symbol=%s side=%s currentPosition=%.8f protectedTotal=%.8f taskCount=%d",
		symbol, side, currentPositionQty, totalProtectedQty, len(tasks))

	if currentPositionQty <= protectionQuantityEpsilon {
		for _, task := range tasks {
			delete(at.protectionTasks, task.ID)
			log.Printf("🗑 本地保护任务取消: taskId=%s symbol=%s side=%s reason=manual_closed",
				task.ID, task.Symbol, task.Side)
		}
		return nil
	}

	if currentPositionQty+protectionQuantityEpsilon >= totalProtectedQty {
		return nil
	}

	excessQty := totalProtectedQty - currentPositionQty
	for _, task := range tasks {
		if excessQty <= protectionQuantityEpsilon {
			break
		}

		if task.Quantity <= excessQty+protectionQuantityEpsilon {
			delete(at.protectionTasks, task.ID)
			excessQty -= task.Quantity
			log.Printf("✂ 本地保护任务缩减: taskId=%s symbol=%s side=%s oldQty=%.8f newQty=0.00000000 reason=manual_reduce_fifo",
				task.ID, task.Symbol, task.Side, task.Quantity)
			continue
		}

		updatedTask := task
		updatedTask.Quantity = task.Quantity - excessQty
		at.protectionTasks[task.ID] = updatedTask
		log.Printf("✂ 本地保护任务缩减: taskId=%s symbol=%s side=%s oldQty=%.8f newQty=%.8f reason=manual_reduce_fifo",
			task.ID, task.Symbol, task.Side, task.Quantity, updatedTask.Quantity)
		excessQty = 0
	}

	return nil
}

func (at *AutoTrader) getCurrentPositionQuantity(symbol, side string) (float64, error) {
	positions, err := at.trader.GetPositions()
	if err != nil {
		return 0, fmt.Errorf("获取持仓失败: %w", err)
	}

	for _, pos := range positions {
		if pos["symbol"] != symbol {
			continue
		}

		positionSide, _ := pos["side"].(string)
		positionAmt, ok := pos["positionAmt"].(float64)
		if !ok {
			continue
		}

		if side == "LONG" && positionSide == "long" {
			if positionAmt < 0 {
				return -positionAmt, nil
			}
			return positionAmt, nil
		}
		if side == "SHORT" && positionSide == "short" {
			if positionAmt < 0 {
				return -positionAmt, nil
			}
			return positionAmt, nil
		}
	}

	return 0, nil
}

func (at *AutoTrader) isNoPositionError(err error) bool {
	if err == nil {
		return false
	}
	errText := err.Error()
	return strings.Contains(errText, "没有找到") || strings.Contains(errText, "position is zero")
}

func (at *AutoTrader) ensureNoSymbolPosition(symbol string) error {
	positions, err := at.trader.GetPositions()
	if err != nil {
		return fmt.Errorf("获取持仓失败: %w", err)
	}
	for _, pos := range positions {
		if pos["symbol"] == symbol {
			return fmt.Errorf("%s 当前已有持仓，拒绝重复开仓", symbol)
		}
	}
	return nil
}

func calculateStructuralStopRatio(symbol, side string, referencePrice float64) (float64, error) {
	currentKline, err := market.GetCurrentKline(symbol, riskOrderTimeframe)
	if err != nil {
		return 0, fmt.Errorf("获取当前15m K线失败: %w", err)
	}

	if side == "LONG" {
		lowestLow := currentKline.Low
		if lowestLow <= 0 || lowestLow >= referencePrice {
			return 0, nil
		}
		return (referencePrice - lowestLow) / referencePrice, nil
	}

	highestHigh := currentKline.High
	if highestHigh <= referencePrice {
		return 0, nil
	}
	return (highestHigh - referencePrice) / referencePrice, nil
}

// GetAccountInfo 获取账户信息（用于API）
func (at *AutoTrader) GetAccountInfo() (map[string]interface{}, error) {
	balance, err := at.trader.GetBalance()
	if err != nil {
		return nil, fmt.Errorf("获取余额失败: %w", err)
	}

	// 获取账户字段
	totalWalletBalance := 0.0
	totalUnrealizedProfit := 0.0
	availableBalance := 0.0

	if wallet, ok := balance["totalWalletBalance"].(float64); ok {
		totalWalletBalance = wallet
	}
	if unrealized, ok := balance["totalUnrealizedProfit"].(float64); ok {
		totalUnrealizedProfit = unrealized
	}
	if avail, ok := balance["availableBalance"].(float64); ok {
		availableBalance = avail
	}

	// Total Equity = 钱包余额 + 未实现盈亏
	totalEquity := totalWalletBalance + totalUnrealizedProfit

	// 获取持仓计算总保证金
	positions, err := at.trader.GetPositions()
	if err != nil {
		return nil, fmt.Errorf("获取持仓失败: %w", err)
	}

	totalMarginUsed := 0.0
	totalUnrealizedPnL := 0.0
	for _, pos := range positions {
		markPrice := pos["markPrice"].(float64)
		quantity := pos["positionAmt"].(float64)
		if quantity < 0 {
			quantity = -quantity
		}
		unrealizedPnl := pos["unRealizedProfit"].(float64)
		totalUnrealizedPnL += unrealizedPnl

		leverage := 10
		if lev, ok := pos["leverage"].(float64); ok {
			leverage = int(lev)
		}
		marginUsed := (quantity * markPrice) / float64(leverage)
		totalMarginUsed += marginUsed
	}

	totalPnL := totalEquity - at.initialBalance
	totalPnLPct := 0.0
	if at.initialBalance > 0 {
		totalPnLPct = (totalPnL / at.initialBalance) * 100
	}

	marginUsedPct := 0.0
	if totalEquity > 0 {
		marginUsedPct = (totalMarginUsed / totalEquity) * 100
	}

	return map[string]interface{}{
		// 核心字段
		"total_equity":      totalEquity,           // 账户净值 = wallet + unrealized
		"wallet_balance":    totalWalletBalance,    // 钱包余额（不含未实现盈亏）
		"unrealized_profit": totalUnrealizedProfit, // 未实现盈亏（从API）
		"available_balance": availableBalance,      // 可用余额

		// 盈亏统计
		"total_pnl":            totalPnL,           // 总盈亏 = equity - initial
		"total_pnl_pct":        totalPnLPct,        // 总盈亏百分比
		"total_unrealized_pnl": totalUnrealizedPnL, // 未实现盈亏（从持仓计算）
		"initial_balance":      at.initialBalance,  // 初始余额
		"daily_pnl":            at.dailyPnL,        // 日盈亏

		// 持仓信息
		"position_count":  len(positions),  // 持仓数量
		"margin_used":     totalMarginUsed, // 保证金占用
		"margin_used_pct": marginUsedPct,   // 保证金使用率
	}, nil
}

// GetPositions 获取持仓列表（用于API）
func (at *AutoTrader) GetPositions() ([]map[string]interface{}, error) {
	positions, err := at.trader.GetPositions()
	if err != nil {
		return nil, fmt.Errorf("获取持仓失败: %w", err)
	}

	var result []map[string]interface{}
	for _, pos := range positions {
		symbol := pos["symbol"].(string)
		side := pos["side"].(string)
		entryPrice := pos["entryPrice"].(float64)
		markPrice := pos["markPrice"].(float64)
		quantity := pos["positionAmt"].(float64)
		if quantity < 0 {
			quantity = -quantity
		}
		unrealizedPnl := pos["unRealizedProfit"].(float64)
		liquidationPrice := pos["liquidationPrice"].(float64)

		leverage := 10
		if lev, ok := pos["leverage"].(float64); ok {
			leverage = int(lev)
		}

		pnlPct := 0.0
		if side == "long" {
			pnlPct = ((markPrice - entryPrice) / entryPrice) * float64(leverage) * 100
		} else {
			pnlPct = ((entryPrice - markPrice) / entryPrice) * float64(leverage) * 100
		}

		marginUsed := (quantity * markPrice) / float64(leverage)

		result = append(result, map[string]interface{}{
			"symbol":             symbol,
			"side":               side,
			"entry_price":        entryPrice,
			"mark_price":         markPrice,
			"quantity":           quantity,
			"leverage":           leverage,
			"unrealized_pnl":     unrealizedPnl,
			"unrealized_pnl_pct": pnlPct,
			"liquidation_price":  liquidationPrice,
			"margin_used":        marginUsed,
		})
	}

	return result, nil
}

// sortDecisionsByPriority 对决策排序：先平仓，再开仓，最后hold/wait
// 这样可以避免换仓时仓位叠加超限
func sortDecisionsByPriority(decisions []decision.Decision) []decision.Decision {
	if len(decisions) <= 1 {
		return decisions
	}

	// 定义优先级
	getActionPriority := func(action string) int {
		switch action {
		case "close_long", "close_short":
			return 1 // 最高优先级：先平仓
		case "open_long", "open_short":
			return 2 // 次优先级：后开仓
		case "hold", "wait":
			return 3 // 最低优先级：观望
		default:
			return 999 // 未知动作放最后
		}
	}

	// 复制决策列表
	sorted := make([]decision.Decision, len(decisions))
	copy(sorted, decisions)

	// 按优先级排序
	for i := 0; i < len(sorted)-1; i++ {
		for j := i + 1; j < len(sorted); j++ {
			if getActionPriority(sorted[i].Action) > getActionPriority(sorted[j].Action) {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}

	return sorted
}
