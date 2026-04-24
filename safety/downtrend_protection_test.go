package safety

import (
	"fmt"
	"opensqt/config"
	"opensqt/exchange"
	"testing"
	"time"
)

// MockExchange 模拟交易所（用于测试）
type MockExchange struct {
	name    string
	candles []*exchange.Candle
}

func NewMockExchange() *MockExchange {
	return &MockExchange{
		name:    "mock",
		candles: make([]*exchange.Candle, 0),
	}
}

func (m *MockExchange) GetName() string { return m.name }

func (m *MockExchange) GetHistoricalKlines(ctx interface{}, symbol, interval string, limit int) ([]*exchange.Candle, error) {
	// 返回预设的K线数据
	if len(m.candles) > limit {
		return m.candles[len(m.candles)-limit:], nil
	}
	return m.candles, nil
}

func (m *MockExchange) StartKlineStream(ctx interface{}, symbols []string, interval string, callback func(*exchange.Candle)) error {
	// 测试中不启动真实的WebSocket
	return nil
}

func (m *MockExchange) StopKlineStream() {}

// SetCandles 设置模拟K线数据
func (m *MockExchange) SetCandles(candles []*exchange.Candle) {
	m.candles = candles
}

// 创建测试配置
func createTestConfig() *config.Config {
	cfg := &config.Config{}
	cfg.DowntrendProtection.Enabled = true
	cfg.DowntrendProtection.EMAShort = 5
	cfg.DowntrendProtection.EMALong = 10
	cfg.DowntrendProtection.DropThresholdPercent = 3.0
	cfg.DowntrendProtection.StabilizeCandles = 5
	cfg.DowntrendProtection.RangeThresholdPercent = 1.5
	cfg.DowntrendProtection.CandleInterval = "15m"
	cfg.DowntrendProtection.MaxFilledPositions = 0
	return cfg
}

// 创建模拟K线
func createCandle(symbol string, open, high, low, close float64, volume float64, isClosed bool, timestamp int64) *exchange.Candle {
	return &exchange.Candle{
		Symbol:    symbol,
		Open:      open,
		High:      high,
		Low:       low,
		Close:     close,
		Volume:    volume,
		IsClosed:  isClosed,
		Timestamp: timestamp,
	}
}

// TestEMACalculation 测试EMA计算
func TestEMACalculation(t *testing.T) {
	cfg := createTestConfig()
	mockEx := NewMockExchange()

	// 创建稳定上涨的K线数据
	candles := make([]*exchange.Candle, 20)
	basePrice := 100.0
	baseTime := time.Now().Unix() * 1000

	for i := 0; i < 20; i++ {
		price := basePrice + float64(i)*0.5 // 缓慢上涨
		candles[i] = createCandle("SOLUSDT", price, price+0.2, price-0.2, price, 1000, true, baseTime+int64(i*900000))
	}
	mockEx.SetCandles(candles)

	dp := NewDowntrendProtection(cfg, nil, "SOLUSDT")
	dp.candles = candles
	dp.initializeEMA()

	emaShort, emaLong := dp.GetEMAValues()

	// EMA短线应该高于长线（上涨趋势）
	if emaShort <= emaLong {
		t.Errorf("上涨趋势中EMA短线应该高于长线: EMA%d=%.4f, EMA%d=%.4f",
			cfg.DowntrendProtection.EMAShort, emaShort,
			cfg.DowntrendProtection.EMALong, emaLong)
	}

	t.Logf("✅ EMA计算测试通过: EMA%d=%.4f, EMA%d=%.4f",
		cfg.DowntrendProtection.EMAShort, emaShort,
		cfg.DowntrendProtection.EMALong, emaLong)
}

// TestStateTransition_ActiveToPaused 测试从ACTIVE到PAUSED的状态转换
func TestStateTransition_ActiveToPaused(t *testing.T) {
	cfg := createTestConfig()
	dp := NewDowntrendProtection(cfg, nil, "SOLUSDT")

	// 初始化：设置一个高点
	dp.recentHigh = 100.0
	dp.recentHighTime = time.Now()
	dp.emaShort = 98.0
	dp.emaLong = 97.0

	// 验证初始状态
	if dp.GetState() != TrendActive {
		t.Fatalf("初始状态应该是ACTIVE，实际是: %s", dp.GetStateString())
	}

	// 模拟价格下跌超过阈值（3%）
	// 从100跌到96 = 4%跌幅
	candle := createCandle("SOLUSDT", 97, 97.5, 95.5, 96, 1000, true, time.Now().Unix()*1000)

	// 手动调用检查函数
	dp.stateMu.Lock()
	dp.checkForDowntrend(96, 97.5, 95.5)
	dp.stateMu.Unlock()

	// 验证状态转换
	if dp.GetState() != TrendPaused {
		t.Errorf("跌幅超过阈值后应该转为PAUSED，实际是: %s", dp.GetStateString())
	}

	// 验证最低价被记录
	if dp.lowestLow != 95.5 {
		t.Errorf("最低价应该是95.5，实际是: %.4f", dp.lowestLow)
	}

	t.Logf("✅ ACTIVE->PAUSED状态转换测试通过 (价格从100跌到96，跌幅4%%)")
	_ = candle // suppress unused warning
}

// TestStateTransition_PausedToStabilizing 测试从PAUSED到STABILIZING的状态转换
func TestStateTransition_PausedToStabilizing(t *testing.T) {
	cfg := createTestConfig()
	cfg.DowntrendProtection.StabilizeCandles = 3 // 设置较小的值便于测试
	dp := NewDowntrendProtection(cfg, nil, "SOLUSDT")

	// 设置为PAUSED状态
	dp.state = TrendPaused
	dp.lowestLow = 84.0
	dp.lowestLowTime = time.Now()
	dp.candlesSinceNewLow = 0

	// 模拟3根K线没有创新低
	for i := 0; i < 3; i++ {
		dp.stateMu.Lock()
		dp.checkForStabilization(84.5, 84.2, true) // 当前低点84.2 > 最低价84.0，没有创新低
		dp.stateMu.Unlock()
	}

	// 验证状态转换
	if dp.GetState() != TrendStabilizing {
		t.Errorf("连续%d根K线未创新低后应该转为STABILIZING，实际是: %s",
			cfg.DowntrendProtection.StabilizeCandles, dp.GetStateString())
	}

	t.Logf("✅ PAUSED->STABILIZING状态转换测试通过 (连续%d根K线未创新低)", cfg.DowntrendProtection.StabilizeCandles)
}

// TestStateTransition_StabilizingToActive 测试从STABILIZING到ACTIVE的状态转换
func TestStateTransition_StabilizingToActive(t *testing.T) {
	cfg := createTestConfig()
	cfg.DowntrendProtection.RangeThresholdPercent = 2.0 // 2%阈值
	dp := NewDowntrendProtection(cfg, nil, "SOLUSDT")

	// 设置为STABILIZING状态
	dp.state = TrendStabilizing
	dp.lowestLow = 84.0
	dp.recentHigh = 89.0

	// 模拟稳定的价格窗口（波动小于2%）
	// 价格在84.0到85.0之间 = 1.19%波动
	dp.stabilizeWindow = []float64{84.2, 84.5, 84.3, 84.8, 84.5}

	// 检测是否回到ACTIVE
	dp.stateMu.Lock()
	dp.checkStabilizationConfirmation(84.5, 84.2, true)
	dp.stateMu.Unlock()

	// 验证状态转换
	if dp.GetState() != TrendActive {
		t.Errorf("波动范围小于阈值后应该转为ACTIVE，实际是: %s", dp.GetStateString())
	}

	// 验证近期高点被重置为当前价格
	if dp.recentHigh != 84.5 {
		t.Errorf("恢复后近期高点应该重置为当前价格84.5，实际是: %.4f", dp.recentHigh)
	}

	t.Logf("✅ STABILIZING->ACTIVE状态转换测试通过 (波动范围收窄)")
}

// TestShouldAllowBuying 测试买入控制
func TestShouldAllowBuying(t *testing.T) {
	cfg := createTestConfig()
	dp := NewDowntrendProtection(cfg, nil, "SOLUSDT")

	// ACTIVE状态应该允许买入
	dp.state = TrendActive
	allow, reason := dp.ShouldAllowBuying()
	if !allow {
		t.Errorf("ACTIVE状态应该允许买入，但返回: %s", reason)
	}

	// PAUSED状态应该禁止买入
	dp.state = TrendPaused
	dp.recentHigh = 89.0
	dp.lowestLow = 84.0
	allow, reason = dp.ShouldAllowBuying()
	if allow {
		t.Errorf("PAUSED状态应该禁止买入")
	}
	t.Logf("PAUSED禁止原因: %s", reason)

	// STABILIZING状态应该禁止买入
	dp.state = TrendStabilizing
	dp.candlesSinceNewLow = 5
	allow, reason = dp.ShouldAllowBuying()
	if allow {
		t.Errorf("STABILIZING状态应该禁止买入")
	}
	t.Logf("STABILIZING禁止原因: %s", reason)

	t.Log("✅ ShouldAllowBuying测试通过")
}

// TestMaxFilledPositions 测试最大持仓限制
func TestMaxFilledPositions(t *testing.T) {
	cfg := createTestConfig()
	cfg.DowntrendProtection.MaxFilledPositions = 10
	dp := NewDowntrendProtection(cfg, nil, "SOLUSDT")
	dp.state = TrendActive

	// 持仓未达上限
	dp.SetFilledPositionCount(5)
	allow, _ := dp.ShouldAllowBuying()
	if !allow {
		t.Error("持仓未达上限应该允许买入")
	}

	// 持仓达到上限
	dp.SetFilledPositionCount(10)
	allow, reason := dp.ShouldAllowBuying()
	if allow {
		t.Error("持仓达到上限应该禁止买入")
	}
	t.Logf("持仓上限禁止原因: %s", reason)

	// 持仓超过上限
	dp.SetFilledPositionCount(15)
	allow, _ = dp.ShouldAllowBuying()
	if allow {
		t.Error("持仓超过上限应该禁止买入")
	}

	t.Log("✅ MaxFilledPositions测试通过")
}

// TestRangePercentCalculation 测试波动范围计算
func TestRangePercentCalculation(t *testing.T) {
	cfg := createTestConfig()
	dp := NewDowntrendProtection(cfg, nil, "SOLUSDT")

	// 测试正常范围
	dp.stabilizeWindow = []float64{84.0, 84.5, 85.0, 84.2, 84.8}
	rangePercent := dp.calculateRangePercent()
	// (85.0 - 84.0) / 84.0 * 100 = 1.19%
	expectedRange := (85.0 - 84.0) / 84.0 * 100

	if rangePercent < expectedRange-0.01 || rangePercent > expectedRange+0.01 {
		t.Errorf("波动范围计算错误: 期望%.2f%%, 实际%.2f%%", expectedRange, rangePercent)
	}

	t.Logf("✅ 波动范围计算测试通过: %.2f%%", rangePercent)
}

// TestScenario_SOL_89_to_84 模拟SOL从89跌到84的完整场景
func TestScenario_SOL_89_to_84(t *testing.T) {
	cfg := createTestConfig()
	cfg.DowntrendProtection.DropThresholdPercent = 3.0
	cfg.DowntrendProtection.StabilizeCandles = 5
	cfg.DowntrendProtection.RangeThresholdPercent = 1.5

	dp := NewDowntrendProtection(cfg, nil, "SOLUSDT")

	// 初始化：价格在89附近
	dp.recentHigh = 89.0
	dp.recentHighTime = time.Now()
	dp.emaShort = 88.5
	dp.emaLong = 88.0

	fmt.Println("\n========== SOL 89→84 场景测试 ==========")

	// 阶段1: 价格在89附近，应该正常交易
	fmt.Println("\n--- 阶段1: 价格在89附近 ---")
	allow, _ := dp.ShouldAllowBuying()
	fmt.Printf("价格: 89.0, 状态: %s, 允许买入: %v\n", dp.GetStateString(), allow)
	if !allow {
		t.Error("阶段1: 价格稳定时应该允许买入")
	}

	// 阶段2: 价格开始下跌，但未超过阈值
	fmt.Println("\n--- 阶段2: 价格下跌到87（跌幅2.2%，未触发）---")
	dp.stateMu.Lock()
	dp.checkForDowntrend(87.0, 87.5, 86.8)
	dp.stateMu.Unlock()
	allow, _ = dp.ShouldAllowBuying()
	fmt.Printf("价格: 87.0, 跌幅: 2.2%%, 状态: %s, 允许买入: %v\n", dp.GetStateString(), allow)
	if !allow {
		t.Error("阶段2: 跌幅未超阈值时应该允许买入")
	}

	// 阶段3: 价格跌破阈值，触发PAUSED
	fmt.Println("\n--- 阶段3: 价格跌到86（跌幅3.4%，触发暂停）---")
	dp.emaShort = 86.5 // 模拟EMA跟随下跌
	dp.stateMu.Lock()
	dp.checkForDowntrend(86.0, 86.2, 85.8)
	dp.stateMu.Unlock()
	allow, reason := dp.ShouldAllowBuying()
	fmt.Printf("价格: 86.0, 跌幅: 3.4%%, 状态: %s, 允许买入: %v, 原因: %s\n",
		dp.GetStateString(), allow, reason)
	if allow {
		t.Error("阶段3: 跌幅超过阈值应该禁止买入")
	}
	if dp.GetState() != TrendPaused {
		t.Error("阶段3: 应该进入PAUSED状态")
	}

	// 阶段4: 价格继续下跌，创新低
	fmt.Println("\n--- 阶段4: 价格继续跌到84（创新低）---")
	dp.stateMu.Lock()
	dp.checkForStabilization(84.0, 83.8, true)
	dp.stateMu.Unlock()
	fmt.Printf("价格: 84.0, 最低价: %.2f, 状态: %s, 无新低计数: %d\n",
		dp.lowestLow, dp.GetStateString(), dp.candlesSinceNewLow)
	if dp.lowestLow != 83.8 {
		t.Errorf("阶段4: 最低价应该更新为83.8，实际: %.2f", dp.lowestLow)
	}

	// 阶段5: 价格开始稳定，不再创新低
	fmt.Println("\n--- 阶段5: 价格稳定在84附近（连续5根K线未创新低）---")
	for i := 0; i < 5; i++ {
		dp.stateMu.Lock()
		dp.checkForStabilization(84.0+float64(i)*0.1, 84.0, true) // 低点都在84以上
		dp.stateMu.Unlock()
		fmt.Printf("  K线%d: 价格=%.2f, 无新低计数: %d, 状态: %s\n",
			i+1, 84.0+float64(i)*0.1, dp.candlesSinceNewLow, dp.GetStateString())
	}
	if dp.GetState() != TrendStabilizing {
		t.Errorf("阶段5: 连续5根K线未创新低后应该进入STABILIZING状态，实际: %s", dp.GetStateString())
	}

	// 阶段6: 波动范围收窄，恢复交易
	fmt.Println("\n--- 阶段6: 波动范围收窄，恢复交易 ---")
	dp.stabilizeWindow = []float64{84.0, 84.2, 84.1, 84.3, 84.2} // 波动约0.36%
	dp.stateMu.Lock()
	dp.checkStabilizationConfirmation(84.2, 84.0, true)
	dp.stateMu.Unlock()
	allow, _ = dp.ShouldAllowBuying()
	fmt.Printf("价格: 84.2, 波动范围: %.2f%%, 状态: %s, 允许买入: %v\n",
		dp.calculateRangePercent(), dp.GetStateString(), allow)
	if !allow {
		t.Error("阶段6: 波动收窄后应该恢复买入")
	}
	if dp.GetState() != TrendActive {
		t.Error("阶段6: 应该恢复到ACTIVE状态")
	}

	// 验证近期高点被重置
	fmt.Printf("\n新锚点: %.2f (原高点: 89.0)\n", dp.recentHigh)
	if dp.recentHigh > 85.0 {
		t.Errorf("恢复后近期高点应该重置为当前价格附近，实际: %.2f", dp.recentHigh)
	}

	fmt.Println("\n========== 场景测试完成 ==========")
	fmt.Println("✅ SOL 89→84 场景测试通过！")
	fmt.Println("   - 在89→86过程中检测到下跌趋势，暂停买入")
	fmt.Println("   - 避免了在88、87、86、85、84买入")
	fmt.Println("   - 在84稳定后恢复交易，以84为新锚点")
}

// TestScenario_FalseBreakout 测试假突破场景（价格小幅下跌后快速恢复）
func TestScenario_FalseBreakout(t *testing.T) {
	cfg := createTestConfig()
	cfg.DowntrendProtection.DropThresholdPercent = 3.0

	dp := NewDowntrendProtection(cfg, nil, "SOLUSDT")
	dp.recentHigh = 100.0
	dp.emaShort = 99.0
	dp.emaLong = 98.0

	fmt.Println("\n========== 假突破场景测试 ==========")

	// 价格下跌2%，未触发
	dp.stateMu.Lock()
	dp.checkForDowntrend(98.0, 98.5, 97.5)
	dp.stateMu.Unlock()

	allow, _ := dp.ShouldAllowBuying()
	fmt.Printf("价格下跌2%%到98，状态: %s, 允许买入: %v\n", dp.GetStateString(), allow)

	if dp.GetState() != TrendActive {
		t.Error("2%跌幅不应该触发暂停")
	}

	// 价格快速恢复到新高
	dp.stateMu.Lock()
	dp.checkForDowntrend(101.0, 101.5, 100.5)
	dp.stateMu.Unlock()

	fmt.Printf("价格恢复到101，近期高点更新为: %.2f\n", dp.recentHigh)

	if dp.recentHigh != 101.5 {
		t.Errorf("近期高点应该更新为101.5，实际: %.2f", dp.recentHigh)
	}

	fmt.Println("✅ 假突破场景测试通过！小幅下跌不触发暂停")
}

// BenchmarkShouldAllowBuying 性能测试
func BenchmarkShouldAllowBuying(b *testing.B) {
	cfg := createTestConfig()
	dp := NewDowntrendProtection(cfg, nil, "SOLUSDT")
	dp.state = TrendActive

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dp.ShouldAllowBuying()
	}
}

// BenchmarkCheckForDowntrend 性能测试
func BenchmarkCheckForDowntrend(b *testing.B) {
	cfg := createTestConfig()
	dp := NewDowntrendProtection(cfg, nil, "SOLUSDT")
	dp.recentHigh = 100.0
	dp.emaShort = 99.0
	dp.emaLong = 98.0

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dp.stateMu.Lock()
		dp.checkForDowntrend(98.0, 98.5, 97.5)
		dp.state = TrendActive // 重置状态
		dp.stateMu.Unlock()
	}
}
