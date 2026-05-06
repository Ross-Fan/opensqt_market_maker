package safety

import (
	"context"
	"fmt"
	"math"
	"opensqt/config"
	"opensqt/exchange"
	"opensqt/logger"
	"sync"
	"time"
)

// TrendState 趋势状态
type TrendState int

const (
	TrendActive      TrendState = iota // 正常交易
	TrendPaused                        // 暂停买入（下跌中）
	TrendStabilizing                   // 观察稳定中
)

func (s TrendState) String() string {
	switch s {
	case TrendActive:
		return "ACTIVE"
	case TrendPaused:
		return "PAUSED"
	case TrendStabilizing:
		return "STABILIZING"
	default:
		return "UNKNOWN"
	}
}

// DowntrendProtection 下跌趋势保护器
// 功能：检测单币种的下跌趋势，在下跌时暂停买入，在稳定后恢复交易并重新锚定价格
type DowntrendProtection struct {
	cfg      *config.Config
	exchange exchange.IExchange
	symbol   string

	// 状态
	state    TrendState
	stateMu  sync.RWMutex
	lastMsg  string
	stateLog string // 状态变化日志

	// K线数据缓存
	candles   []*exchange.Candle
	candlesMu sync.RWMutex

	// EMA值
	emaShort float64 // 短期EMA
	emaLong  float64 // 长期EMA

	// 趋势追踪
	recentHigh         float64   // 近期最高价
	recentHighTime     time.Time // 近期最高价时间
	lowestLow          float64   // 暂停后的最低价
	lowestLowTime      time.Time // 最低价时间
	candlesSinceNewLow int       // 无新低的K线计数

	// 稳定检测窗口
	stabilizeWindow []float64 // 稳定期价格窗口

	// 锚点重置回调
	onAnchorReset func(newPrice float64)

	// 持仓计数器（用于限制最大持仓）
	filledPositionCount int
	positionCountMu     sync.RWMutex
}

// NewDowntrendProtection 创建下跌趋势保护器
func NewDowntrendProtection(cfg *config.Config, ex exchange.IExchange, symbol string) *DowntrendProtection {
	return &DowntrendProtection{
		cfg:             cfg,
		exchange:        ex,
		symbol:          symbol,
		state:           TrendActive,
		candles:         make([]*exchange.Candle, 0),
		stabilizeWindow: make([]float64, 0),
	}
}

// SetAnchorResetCallback 设置锚点重置回调
func (d *DowntrendProtection) SetAnchorResetCallback(callback func(newPrice float64)) {
	d.onAnchorReset = callback
}

// SetFilledPositionCount 设置当前持仓数量（由外部调用更新）
func (d *DowntrendProtection) SetFilledPositionCount(count int) {
	d.positionCountMu.Lock()
	d.filledPositionCount = count
	d.positionCountMu.Unlock()
}

// Start 启动下跌趋势保护
// 注意：即使 disabled，也会启动K线流和EMA计算（用于趋势自适应网格）
func (d *DowntrendProtection) Start(ctx context.Context) {
	protoEnabled := d.cfg.DowntrendProtection.Enabled

	if protoEnabled {
		logger.Info("📉 启动下跌趋势保护 (周期: %s, EMA: %d/%d, 跌幅阈值: %.1f%%, 稳定K线: %d)",
			d.cfg.DowntrendProtection.CandleInterval,
			d.cfg.DowntrendProtection.EMAShort,
			d.cfg.DowntrendProtection.EMALong,
			d.cfg.DowntrendProtection.DropThresholdPercent,
			d.cfg.DowntrendProtection.StabilizeCandles)

		if d.cfg.DowntrendProtection.MaxFilledPositions > 0 {
			logger.Info("📉 最大持仓限制: %d 个槽位", d.cfg.DowntrendProtection.MaxFilledPositions)
		}
	} else {
		logger.Info("ℹ️ 下跌趋势保护未启用（但EMA计算仍在运行，用于趋势自适应网格）")
	}

	// 预加载历史K线数据（即使disabled也需要，用于EMA初始化）
	logger.Info("📊 正在加载 %s 历史K线数据...", d.symbol)
	requiredCandles := d.cfg.DowntrendProtection.EMALong + d.cfg.DowntrendProtection.StabilizeCandles + 10
	candles, err := d.exchange.GetHistoricalKlines(ctx, d.symbol, d.cfg.DowntrendProtection.CandleInterval, requiredCandles)
	if err != nil {
		logger.Warn("⚠️ 加载 %s 历史K线失败: %v", d.symbol, err)
	} else if len(candles) > 0 {
		d.candlesMu.Lock()
		d.candles = candles
		d.candlesMu.Unlock()
		logger.Info("✅ %s: 已加载 %d 根历史K线", d.symbol, len(candles))

		// 初始化EMA
		d.initializeEMA()

		// 初始化近期最高价
		d.initializeRecentHigh()
	}

	// 启动K线流（即使disabled也需要，用于EMA实时更新）
	symbols := []string{d.symbol}
	if err := d.exchange.StartKlineStream(ctx, symbols, d.cfg.DowntrendProtection.CandleInterval, d.onCandleUpdate); err != nil {
		logger.Error("❌ 启动下跌趋势保护K线流失败: %v", err)
		return
	}

	// 只在启用时启动定期报告和状态机
	if protoEnabled {
		go d.reportLoop(ctx)
		logger.Info("✅ 下跌趋势保护已启动")
	} else {
		logger.Info("✅ EMA趋势计算已启动（下跌趋势保护未启用）")
	}
}

// initializeEMA 初始化EMA值
func (d *DowntrendProtection) initializeEMA() {
	d.candlesMu.RLock()
	defer d.candlesMu.RUnlock()

	if len(d.candles) < d.cfg.DowntrendProtection.EMALong {
		return
	}

	// 计算初始SMA作为EMA起点
	shortPeriod := d.cfg.DowntrendProtection.EMAShort
	longPeriod := d.cfg.DowntrendProtection.EMALong

	// 计算短期EMA
	var sumShort float64
	for i := 0; i < shortPeriod && i < len(d.candles); i++ {
		sumShort += d.candles[i].Close
	}
	d.emaShort = sumShort / float64(shortPeriod)

	// 计算长期EMA
	var sumLong float64
	for i := 0; i < longPeriod && i < len(d.candles); i++ {
		sumLong += d.candles[i].Close
	}
	d.emaLong = sumLong / float64(longPeriod)

	// 使用剩余K线更新EMA
	shortMultiplier := 2.0 / float64(shortPeriod+1)
	longMultiplier := 2.0 / float64(longPeriod+1)

	for i := longPeriod; i < len(d.candles); i++ {
		price := d.candles[i].Close
		d.emaShort = (price-d.emaShort)*shortMultiplier + d.emaShort
		d.emaLong = (price-d.emaLong)*longMultiplier + d.emaLong
	}

	logger.Info("📊 EMA初始化完成: EMA%d=%.4f, EMA%d=%.4f",
		shortPeriod, d.emaShort, longPeriod, d.emaLong)
}

// initializeRecentHigh 初始化近期最高价
func (d *DowntrendProtection) initializeRecentHigh() {
	d.candlesMu.RLock()
	defer d.candlesMu.RUnlock()

	if len(d.candles) == 0 {
		return
	}

	// 找最近20根K线的最高价
	lookback := 20
	if lookback > len(d.candles) {
		lookback = len(d.candles)
	}

	d.recentHigh = 0
	for i := len(d.candles) - lookback; i < len(d.candles); i++ {
		if d.candles[i].High > d.recentHigh {
			d.recentHigh = d.candles[i].High
			// 自动判断时间戳单位
			if d.candles[i].Timestamp > 10000000000 {
				d.recentHighTime = time.Unix(d.candles[i].Timestamp/1000, 0)
			} else {
				d.recentHighTime = time.Unix(d.candles[i].Timestamp, 0)
			}
		}
	}

	logger.Info("📊 近期最高价初始化: %.4f (时间: %s)", d.recentHigh, d.recentHighTime.Format("2006-01-02 15:04"))
}

// onCandleUpdate K线更新回调
func (d *DowntrendProtection) onCandleUpdate(candle *exchange.Candle) {
	if candle == nil || candle.Symbol != d.symbol {
		return
	}

	// 更新K线缓存
	d.updateCandles(candle)

	// 更新EMA（始终需要，用于趋势自适应网格）
	if candle.IsClosed {
		d.updateEMA(candle.Close)
	}

	// 检查趋势状态（仅在下跌保护启用时执行状态机逻辑）
	if d.cfg.DowntrendProtection.Enabled {
		d.checkTrend(candle)
	}
}

// updateCandles 更新K线缓存
func (d *DowntrendProtection) updateCandles(candle *exchange.Candle) {
	d.candlesMu.Lock()
	defer d.candlesMu.Unlock()

	if candle.IsClosed {
		d.candles = append(d.candles, candle)
		// 保留足够的K线
		maxCandles := d.cfg.DowntrendProtection.EMALong + d.cfg.DowntrendProtection.StabilizeCandles + 20
		if len(d.candles) > maxCandles {
			d.candles = d.candles[len(d.candles)-maxCandles:]
		}
	} else {
		// 更新最后一根未完结K线
		if len(d.candles) > 0 && !d.candles[len(d.candles)-1].IsClosed {
			d.candles[len(d.candles)-1] = candle
		} else {
			d.candles = append(d.candles, candle)
		}
	}
}

// updateEMA 更新EMA值
func (d *DowntrendProtection) updateEMA(price float64) {
	shortPeriod := d.cfg.DowntrendProtection.EMAShort
	longPeriod := d.cfg.DowntrendProtection.EMALong

	shortMultiplier := 2.0 / float64(shortPeriod+1)
	longMultiplier := 2.0 / float64(longPeriod+1)

	if d.emaShort == 0 {
		d.emaShort = price
	} else {
		d.emaShort = (price-d.emaShort)*shortMultiplier + d.emaShort
	}

	if d.emaLong == 0 {
		d.emaLong = price
	} else {
		d.emaLong = (price-d.emaLong)*longMultiplier + d.emaLong
	}
}

// checkTrend 检查趋势状态
func (d *DowntrendProtection) checkTrend(candle *exchange.Candle) {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()

	currentPrice := candle.Close
	currentHigh := candle.High
	currentLow := candle.Low

	switch d.state {
	case TrendActive:
		d.checkForDowntrend(currentPrice, currentHigh, currentLow)
	case TrendPaused:
		d.checkForStabilization(currentPrice, currentLow, candle.IsClosed)
	case TrendStabilizing:
		d.checkStabilizationConfirmation(currentPrice, currentLow, candle.IsClosed)
	}
}

// checkForDowntrend 检查是否进入下跌趋势
func (d *DowntrendProtection) checkForDowntrend(currentPrice, currentHigh, currentLow float64) {
	// 更新近期最高价
	if currentHigh > d.recentHigh {
		d.recentHigh = currentHigh
		d.recentHighTime = time.Now()
	}

	// 条件1: 价格跌幅超过阈值
	dropPercent := (d.recentHigh - currentPrice) / d.recentHigh * 100
	dropThreshold := d.cfg.DowntrendProtection.DropThresholdPercent

	// 条件2: 价格低于短期EMA
	belowEMAShort := currentPrice < d.emaShort

	// 条件3: 短期EMA低于长期EMA（下跌趋势确认）
	emaDowntrend := d.emaShort < d.emaLong

	// 触发条件: 跌幅超标 且 (价格低于短EMA 或 EMA死叉)
	if dropPercent >= dropThreshold && (belowEMAShort || emaDowntrend) {
		d.state = TrendPaused
		d.lowestLow = currentLow
		d.lowestLowTime = time.Now()
		d.candlesSinceNewLow = 0
		d.stabilizeWindow = make([]float64, 0)

		d.stateLog = fmt.Sprintf("从高点%.4f下跌%.2f%%至%.4f，EMA%d=%.4f, EMA%d=%.4f",
			d.recentHigh, dropPercent, currentPrice,
			d.cfg.DowntrendProtection.EMAShort, d.emaShort,
			d.cfg.DowntrendProtection.EMALong, d.emaLong)

		logger.Warn("📉📉📉 [下跌趋势保护] 触发暂停买入! %s", d.stateLog)
		logger.Warn("📉 当前状态: %s -> PAUSED", TrendActive.String())
	}
}

// checkForStabilization 检查是否开始稳定
func (d *DowntrendProtection) checkForStabilization(currentPrice, currentLow float64, isClosed bool) {
	// 检查是否创新低
	if currentLow < d.lowestLow {
		d.lowestLow = currentLow
		d.lowestLowTime = time.Now()
		d.candlesSinceNewLow = 0
		d.stabilizeWindow = make([]float64, 0)
		logger.Debug("📉 [下跌趋势保护] 创新低: %.4f", currentLow)
		return
	}

	// 只在K线完结时计数
	if isClosed {
		d.candlesSinceNewLow++
		d.stabilizeWindow = append(d.stabilizeWindow, currentPrice)

		// 保持窗口大小
		windowSize := d.cfg.DowntrendProtection.StabilizeCandles
		if len(d.stabilizeWindow) > windowSize {
			d.stabilizeWindow = d.stabilizeWindow[len(d.stabilizeWindow)-windowSize:]
		}

		logger.Debug("📊 [下跌趋势保护] 无新低计数: %d/%d, 最低价: %.4f",
			d.candlesSinceNewLow, d.cfg.DowntrendProtection.StabilizeCandles, d.lowestLow)

		// 达到稳定K线数要求，进入确认阶段
		if d.candlesSinceNewLow >= d.cfg.DowntrendProtection.StabilizeCandles {
			d.state = TrendStabilizing
			logger.Info("📊 [下跌趋势保护] 进入稳定确认阶段 (无新低%d根K线)", d.candlesSinceNewLow)
		}
	}
}

// checkStabilizationConfirmation 确认稳定并恢复交易
func (d *DowntrendProtection) checkStabilizationConfirmation(currentPrice, currentLow float64, isClosed bool) {
	// 如果又创新低，回到PAUSED状态
	if currentLow < d.lowestLow {
		d.lowestLow = currentLow
		d.lowestLowTime = time.Now()
		d.candlesSinceNewLow = 0
		d.stabilizeWindow = make([]float64, 0)
		d.state = TrendPaused
		logger.Warn("📉 [下跌趋势保护] 稳定确认失败，再次创新低: %.4f", currentLow)
		return
	}

	if !isClosed {
		return
	}

	// 计算价格波动范围
	rangePercent := d.calculateRangePercent()

	// 稳定条件: 波动范围小于阈值
	rangeThreshold := d.cfg.DowntrendProtection.RangeThresholdPercent
	if rangePercent <= rangeThreshold {
		// 稳定确认，恢复交易
		newAnchorPrice := currentPrice

		d.stateLog = fmt.Sprintf("价格稳定在%.4f附近(波动%.2f%% < %.2f%%阈值)，重新锚定",
			newAnchorPrice, rangePercent, rangeThreshold)

		logger.Info("✅✅✅ [下跌趋势保护] 价格稳定确认! %s", d.stateLog)
		logger.Info("✅ 当前状态: STABILIZING -> ACTIVE")
		logger.Info("✅ 新锚点价格: %.4f (原高点: %.4f, 最低点: %.4f)",
			newAnchorPrice, d.recentHigh, d.lowestLow)

		// 重置状态
		d.state = TrendActive
		d.recentHigh = currentPrice // 重置近期最高价为当前价
		d.recentHighTime = time.Now()
		d.candlesSinceNewLow = 0
		d.stabilizeWindow = make([]float64, 0)

		// 触发锚点重置回调
		if d.onAnchorReset != nil {
			d.onAnchorReset(newAnchorPrice)
		}
	} else {
		logger.Debug("📊 [下跌趋势保护] 波动范围 %.2f%% > %.2f%% 阈值，继续观察",
			rangePercent, rangeThreshold)
	}
}

// calculateRangePercent 计算稳定窗口内的价格波动百分比
func (d *DowntrendProtection) calculateRangePercent() float64 {
	if len(d.stabilizeWindow) < 2 {
		return 100.0 // 数据不足，返回大值
	}

	minPrice := d.stabilizeWindow[0]
	maxPrice := d.stabilizeWindow[0]

	for _, p := range d.stabilizeWindow {
		if p < minPrice {
			minPrice = p
		}
		if p > maxPrice {
			maxPrice = p
		}
	}

	if minPrice == 0 {
		return 100.0
	}

	return (maxPrice - minPrice) / minPrice * 100
}

// ShouldAllowBuying 是否允许买入
// 返回: (允许买入, 原因)
func (d *DowntrendProtection) ShouldAllowBuying() (bool, string) {
	if !d.cfg.DowntrendProtection.Enabled {
		return true, ""
	}

	// 检查持仓数量限制
	if d.cfg.DowntrendProtection.MaxFilledPositions > 0 {
		d.positionCountMu.RLock()
		count := d.filledPositionCount
		d.positionCountMu.RUnlock()

		if count >= d.cfg.DowntrendProtection.MaxFilledPositions {
			return false, fmt.Sprintf("持仓数量已达上限(%d/%d)",
				count, d.cfg.DowntrendProtection.MaxFilledPositions)
		}
	}

	d.stateMu.RLock()
	defer d.stateMu.RUnlock()

	switch d.state {
	case TrendActive:
		return true, ""
	case TrendPaused:
		return false, fmt.Sprintf("下跌趋势中(从%.4f跌至%.4f)", d.recentHigh, d.lowestLow)
	case TrendStabilizing:
		return false, fmt.Sprintf("等待稳定确认(无新低%d根K线)", d.candlesSinceNewLow)
	default:
		return true, ""
	}
}

// IsTriggered 是否触发了下跌保护（兼容RiskMonitor接口）
func (d *DowntrendProtection) IsTriggered() bool {
	if !d.cfg.DowntrendProtection.Enabled {
		return false
	}

	d.stateMu.RLock()
	defer d.stateMu.RUnlock()

	return d.state != TrendActive
}

// GetState 获取当前状态
func (d *DowntrendProtection) GetState() TrendState {
	d.stateMu.RLock()
	defer d.stateMu.RUnlock()
	return d.state
}

// GetStateString 获取状态字符串
func (d *DowntrendProtection) GetStateString() string {
	d.stateMu.RLock()
	defer d.stateMu.RUnlock()
	return d.state.String()
}

// reportLoop 定期报告状态
func (d *DowntrendProtection) reportLoop(ctx context.Context) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.reportStatus()
		}
	}
}

// reportStatus 报告状态
func (d *DowntrendProtection) reportStatus() {
	d.stateMu.RLock()
	state := d.state
	recentHigh := d.recentHigh
	lowestLow := d.lowestLow
	candlesSinceNewLow := d.candlesSinceNewLow
	d.stateMu.RUnlock()

	d.candlesMu.RLock()
	var currentPrice float64
	if len(d.candles) > 0 {
		currentPrice = d.candles[len(d.candles)-1].Close
	}
	d.candlesMu.RUnlock()

	// 计算当前跌幅
	dropPercent := 0.0
	if recentHigh > 0 {
		dropPercent = (recentHigh - currentPrice) / recentHigh * 100
	}

	switch state {
	case TrendActive:
		logger.Info("📈 [下跌趋势保护] 状态: %s, 当前价: %.4f, 近期高点: %.4f, 跌幅: %.2f%%, EMA%d=%.4f, EMA%d=%.4f",
			state.String(), currentPrice, recentHigh, dropPercent,
			d.cfg.DowntrendProtection.EMAShort, d.emaShort,
			d.cfg.DowntrendProtection.EMALong, d.emaLong)
	case TrendPaused:
		logger.Warn("📉 [下跌趋势保护] 状态: %s, 当前价: %.4f, 高点: %.4f, 最低: %.4f, 无新低: %d根",
			state.String(), currentPrice, recentHigh, lowestLow, candlesSinceNewLow)
	case TrendStabilizing:
		rangePercent := d.calculateRangePercent()
		logger.Info("📊 [下跌趋势保护] 状态: %s, 当前价: %.4f, 波动范围: %.2f%%, 阈值: %.2f%%",
			state.String(), currentPrice, rangePercent, d.cfg.DowntrendProtection.RangeThresholdPercent)
	}

	// 显示持仓限制状态
	if d.cfg.DowntrendProtection.MaxFilledPositions > 0 {
		d.positionCountMu.RLock()
		count := d.filledPositionCount
		d.positionCountMu.RUnlock()
		logger.Info("📊 [下跌趋势保护] 持仓数量: %d/%d",
			count, d.cfg.DowntrendProtection.MaxFilledPositions)
	}
}

// Stop 停止下跌趋势保护
func (d *DowntrendProtection) Stop() {
	logger.Info("⏹️ 停止下跌趋势保护")
}

// ForceResetToActive 强制重置为活跃状态（用于手动干预）
func (d *DowntrendProtection) ForceResetToActive(newPrice float64) {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()

	oldState := d.state
	d.state = TrendActive
	d.recentHigh = newPrice
	d.recentHighTime = time.Now()
	d.candlesSinceNewLow = 0
	d.stabilizeWindow = make([]float64, 0)

	logger.Info("🔧 [下跌趋势保护] 手动重置: %s -> ACTIVE, 新锚点: %.4f", oldState.String(), newPrice)
}

// GetEMAValues 获取EMA值（用于调试）
func (d *DowntrendProtection) GetEMAValues() (emaShort, emaLong float64) {
	return d.emaShort, d.emaLong
}

// GetTrendStrength 返回当前趋势强度 [-1, 1]
//   - 正值 = 上涨趋势（短EMA > 长EMA）
//   - 负值 = 下跌趋势（短EMA < 长EMA）
//   - 0   = 无明显趋势（短EMA ≈ 长EMA）
// 该值用于趋势自适应网格间距，动态调整网格密度和交易策略
func (d *DowntrendProtection) GetTrendStrength() float64 {
	d.stateMu.RLock()
	defer d.stateMu.RUnlock()

	if d.emaLong == 0 {
		return 0
	}

	// EMA差值百分比
	diff := (d.emaShort - d.emaLong) / d.emaLong
	// 归一化到 [-1, 1]，±2% 为全量
	// 即: 短EMA比长EMA高2%以上 -> +1 (强上涨)
	//     短EMA比长EMA低2%以上 -> -1 (强下跌)
	strength := diff / 0.02
	if strength > 1 {
		return 1
	}
	if strength < -1 {
		return -1
	}
	return strength
}

// GetDropPercent 获取当前跌幅百分比
func (d *DowntrendProtection) GetDropPercent() float64 {
	d.stateMu.RLock()
	recentHigh := d.recentHigh
	d.stateMu.RUnlock()

	d.candlesMu.RLock()
	var currentPrice float64
	if len(d.candles) > 0 {
		currentPrice = d.candles[len(d.candles)-1].Close
	}
	d.candlesMu.RUnlock()

	if recentHigh == 0 {
		return 0
	}

	return math.Max(0, (recentHigh-currentPrice)/recentHigh*100)
}
