// internal/indicators/indicators.go
//
// WHAT: Pure math functions that turn a slice of daily price/volume data into
//       the technical indicator values the strategy engine uses.
//
// WHY:  All technical analysis must be deterministic and code-owned.
//       No black-box library — each function is explicit and auditable.
//
// HOW:  Functions operate on []float64 slices (oldest first, newest last).
//       They return the last computed value for the strategy engine to compare
//       against thresholds. All require at least (period) input bars.
//
// WHAT BREAKS: If the caller passes fewer bars than the period, the function
//              returns 0 and ok=false. The strategy engine treats ok=false as
//              insufficient data and skips the symbol.
//
// VERIFY: go test ./internal/indicators/ — unit tests in indicators_test.go

package indicators

import "math"

// Bar is a single OHLCV day bar. All fields are float64 for math convenience.
type Bar struct {
	Open   float64
	High   float64
	Low    float64
	Close  float64
	Volume float64
}

// ──────────────────────────────────────────────────────────────────────────────
// EMA — Exponential Moving Average
// ──────────────────────────────────────────────────────────────────────────────

// EMASlice computes EMA over the full slice and returns the full result slice.
// The first (period-1) values are seeded from SMA and then smoothed forward.
// Oldest bar = index 0, newest = index len-1.
func EMASlice(values []float64, period int) []float64 {
	if len(values) < period {
		return nil
	}
	result := make([]float64, len(values))
	k := 2.0 / float64(period+1)

	// Seed with simple average of first `period` values
	var sum float64
	for i := 0; i < period; i++ {
		sum += values[i]
	}
	result[period-1] = sum / float64(period)

	// Smooth forward
	for i := period; i < len(values); i++ {
		result[i] = values[i]*k + result[i-1]*(1-k)
	}
	return result
}

// EMALast returns only the most recent EMA value.
func EMALast(values []float64, period int) (float64, bool) {
	s := EMASlice(values, period)
	if s == nil {
		return 0, false
	}
	return s[len(s)-1], true
}

// ──────────────────────────────────────────────────────────────────────────────
// MACD — Moving Average Convergence Divergence
// ──────────────────────────────────────────────────────────────────────────────

// MACDResult holds the three MACD components.
type MACDResult struct {
	MACD      float64 // fast EMA - slow EMA
	Signal    float64 // 9-day EMA of MACD line
	Histogram float64 // MACD - Signal (positive = bullish momentum)
}

// MACDLast computes MACD(12,26,9) and returns the latest values.
// Needs at least 35 bars (26 for slow EMA + 9 for signal).
func MACDLast(closes []float64) (MACDResult, bool) {
	if len(closes) < 35 {
		return MACDResult{}, false
	}
	fast := EMASlice(closes, 12)
	slow := EMASlice(closes, 26)
	if fast == nil || slow == nil {
		return MACDResult{}, false
	}

	// MACD line = fast EMA - slow EMA (valid from index 25 onward)
	macdLine := make([]float64, len(closes))
	for i := 25; i < len(closes); i++ {
		macdLine[i] = fast[i] - slow[i]
	}

	// Signal = 9-EMA of MACD line (only the valid portion)
	valid := macdLine[25:]
	if len(valid) < 9 {
		return MACDResult{}, false
	}
	signalSlice := EMASlice(valid, 9)
	if signalSlice == nil {
		return MACDResult{}, false
	}

	macdVal := macdLine[len(macdLine)-1]
	signalVal := signalSlice[len(signalSlice)-1]
	return MACDResult{
		MACD:      macdVal,
		Signal:    signalVal,
		Histogram: macdVal - signalVal,
	}, true
}

// ──────────────────────────────────────────────────────────────────────────────
// RSI — Relative Strength Index
// ──────────────────────────────────────────────────────────────────────────────

// RSILast computes RSI(14) and returns the latest value.
// Needs at least 15 bars.
func RSILast(closes []float64, period int) (float64, bool) {
	if period <= 0 {
		period = 14
	}
	if len(closes) < period+1 {
		return 0, false
	}

	var gainSum, lossSum float64
	for i := 1; i <= period; i++ {
		diff := closes[i] - closes[i-1]
		if diff > 0 {
			gainSum += diff
		} else {
			lossSum += -diff
		}
	}
	avgGain := gainSum / float64(period)
	avgLoss := lossSum / float64(period)

	for i := period + 1; i < len(closes); i++ {
		diff := closes[i] - closes[i-1]
		if diff > 0 {
			avgGain = (avgGain*float64(period-1) + diff) / float64(period)
			avgLoss = (avgLoss * float64(period-1)) / float64(period)
		} else {
			avgGain = (avgGain * float64(period-1)) / float64(period)
			avgLoss = (avgLoss*float64(period-1) + (-diff)) / float64(period)
		}
	}

	if avgLoss == 0 {
		return 100, true
	}
	rs := avgGain / avgLoss
	return 100 - (100 / (1 + rs)), true
}

// ──────────────────────────────────────────────────────────────────────────────
// Volume Ratio
// ──────────────────────────────────────────────────────────────────────────────

// VolumeRatioLast returns (last bar volume) / (avg of prior period bars).
// period=20 is the standard lookback for "20-day average volume".
func VolumeRatioLast(volumes []float64, period int) (float64, bool) {
	if len(volumes) < period+1 {
		return 0, false
	}
	n := len(volumes)
	var avg float64
	for i := n - 1 - period; i < n-1; i++ {
		avg += volumes[i]
	}
	avg /= float64(period)
	if avg == 0 {
		return 0, false
	}
	return volumes[n-1] / avg, true
}

// ──────────────────────────────────────────────────────────────────────────────
// Rate of Change (ROC)
// ──────────────────────────────────────────────────────────────────────────────

// ROCLast computes the N-day rate of change: (close[-1] - close[-N-1]) / close[-N-1] * 100
func ROCLast(closes []float64, period int) (float64, bool) {
	n := len(closes)
	if n < period+1 {
		return 0, false
	}
	base := closes[n-1-period]
	if base == 0 {
		return 0, false
	}
	return (closes[n-1] - base) / base * 100, true
}

// ──────────────────────────────────────────────────────────────────────────────
// ATR — Average True Range
// ──────────────────────────────────────────────────────────────────────────────

// ATRLast returns the most recent ATR(period) value.
func ATRLast(bars []Bar, period int) (float64, bool) {
	if len(bars) < period+1 {
		return 0, false
	}
	trValues := make([]float64, len(bars)-1)
	for i := 1; i < len(bars); i++ {
		hl := bars[i].High - bars[i].Low
		hc := math.Abs(bars[i].High - bars[i-1].Close)
		lc := math.Abs(bars[i].Low - bars[i-1].Close)
		trValues[i-1] = math.Max(hl, math.Max(hc, lc))
	}
	// Simple average for first ATR, then Wilder smoothing
	if len(trValues) < period {
		return 0, false
	}
	var atr float64
	for i := 0; i < period; i++ {
		atr += trValues[i]
	}
	atr /= float64(period)
	for i := period; i < len(trValues); i++ {
		atr = (atr*float64(period-1) + trValues[i]) / float64(period)
	}
	return atr, true
}

// ──────────────────────────────────────────────────────────────────────────────
// Pattern Detection Helpers
// ──────────────────────────────────────────────────────────────────────────────

// IsBullFlag detects a bull flag: sharp advance followed by shallow consolidation.
// Returns (detected, confidence 0-1).
func IsBullFlag(bars []Bar) (bool, float64) {
	n := len(bars)
	if n < 15 {
		return false, 0
	}

	// Look for a "pole": 10%+ advance in last 3-5 bars ending ~8 bars ago
	poleEnd := n - 8
	poleStart := poleEnd - 5
	if poleStart < 0 {
		return false, 0
	}

	poleGain := (bars[poleEnd].Close - bars[poleStart].Close) / bars[poleStart].Close
	if poleGain < 0.08 { // at least 8% pole
		return false, 0
	}

	// "Flag": last 7 bars should be in a tight range declining on volume
	flagHigh := bars[poleEnd].High
	flagLow := bars[poleEnd].Low
	var flagVolSum float64
	for i := poleEnd + 1; i < n; i++ {
		if bars[i].High > flagHigh {
			flagHigh = bars[i].High
		}
		if bars[i].Low < flagLow {
			flagLow = bars[i].Low
		}
		flagVolSum += bars[i].Volume
	}
	flagRange := (flagHigh - flagLow) / bars[poleEnd].Close
	// Flag should not retrace more than 50% of pole, range should be tight
	pullback := (bars[poleEnd].Close - bars[n-1].Close) / bars[poleEnd].Close
	if pullback > poleGain*0.5 {
		return false, 0
	}

	// Average pole volume
	var poleVolSum float64
	for i := poleStart; i <= poleEnd; i++ {
		poleVolSum += bars[i].Volume
	}
	avgPoleVol := poleVolSum / float64(poleEnd-poleStart+1)
	avgFlagVol := flagVolSum / float64(n-poleEnd-1)

	if flagRange > 0.12 {
		return false, 0
	}

	confidence := 0.5
	if avgFlagVol < avgPoleVol*0.7 {
		confidence += 0.2 // volume contracting in flag — bullish
	}
	if pullback < poleGain*0.3 {
		confidence += 0.15 // shallow pullback
	}
	if poleGain > 0.15 {
		confidence += 0.1 // strong pole
	}
	return true, math.Min(confidence, 0.95)
}

// IsTightBase detects price consolidation: range < 10% over last 10-15 bars.
func IsTightBase(bars []Bar, lookback int) (bool, float64) {
	if lookback <= 0 {
		lookback = 12
	}
	n := len(bars)
	if n < lookback {
		return false, 0
	}
	slice := bars[n-lookback:]
	hi := slice[0].High
	lo := slice[0].Low
	for _, b := range slice {
		if b.High > hi {
			hi = b.High
		}
		if b.Low < lo {
			lo = b.Low
		}
	}
	mid := (hi + lo) / 2
	if mid == 0 {
		return false, 0
	}
	rangeRatio := (hi - lo) / mid
	if rangeRatio > 0.10 {
		return false, 0
	}

	confidence := 0.5 + (0.10-rangeRatio)/0.10*0.35
	return true, math.Min(confidence, 0.90)
}

// IsVCB detects Volume Contraction Breakout: ATR shrinking + price near resistance.
func IsVCB(bars []Bar) (bool, float64) {
	n := len(bars)
	if n < 25 {
		return false, 0
	}

	// Price near 20-bar high
	hi20 := bars[n-20].High
	for i := n - 20; i < n; i++ {
		if bars[i].High > hi20 {
			hi20 = bars[i].High
		}
	}
	near := bars[n-1].Close >= hi20*0.98 // within 2% of high

	// Volume contracting in last 5 vs prior 15
	var vol5, vol15 float64
	for i := n - 5; i < n; i++ {
		vol5 += bars[i].Volume
	}
	for i := n - 20; i < n-5; i++ {
		vol15 += bars[i].Volume
	}
	vol5Avg := vol5 / 5
	vol15Avg := vol15 / 15
	contracted := vol5Avg < vol15Avg*0.80

	if !near || !contracted {
		return false, 0
	}
	return true, 0.72
}

// RelativeStrength returns (stock 20d return - spy 20d return).
// Positive means the stock is outperforming SPY.
func RelativeStrength(stockCloses, spyCloses []float64) (float64, bool) {
	stockROC, ok1 := ROCLast(stockCloses, 20)
	spyROC, ok2 := ROCLast(spyCloses, 20)
	if !ok1 || !ok2 {
		return 0, false
	}
	return stockROC - spyROC, true
}

// ──────────────────────────────────────────────────────────────────────────────
// Anti-Pattern Detection
// ──────────────────────────────────────────────────────────────────────────────

// IsLateStageExtension detects a blow-off: price >15% above EMA20 with volume spike.
func IsLateStageExtension(bars []Bar) bool {
	n := len(bars)
	if n < 25 {
		return false
	}
	closes := make([]float64, n)
	for i, b := range bars {
		closes[i] = b.Close
	}
	ema20, ok := EMALast(closes, 20)
	if !ok || ema20 == 0 {
		return false
	}
	extension := (bars[n-1].Close - ema20) / ema20
	if extension < 0.15 {
		return false
	}
	// Check for volume spike in last 3 bars
	var vol3, vol20 float64
	for i := n - 3; i < n; i++ {
		vol3 += bars[i].Volume
	}
	for i := n - 20; i < n; i++ {
		vol20 += bars[i].Volume
	}
	avgVol3 := vol3 / 3
	avgVol20 := vol20 / 20
	return avgVol3 > avgVol20*2.5
}

// HasDistributionDays returns true if >= 3 of last 10 bars show high-volume selling.
func HasDistributionDays(bars []Bar) bool {
	n := len(bars)
	if n < 15 {
		return false
	}
	lookback := 10
	slice := bars[n-lookback:]

	// Compute 20-day average volume for context
	var avgVol float64
	start := n - 20
	if start < 0 {
		start = 0
	}
	for i := start; i < n; i++ {
		avgVol += bars[i].Volume
	}
	avgVol /= float64(n - start)

	distCount := 0
	for _, b := range slice {
		// Distribution day: high volume + close < open + close in lower half of range
		isHighVol := b.Volume > avgVol*1.3
		isDown := b.Close < b.Open
		rangeMid := (b.High + b.Low) / 2
		nearLow := b.Close < rangeMid
		if isHighVol && isDown && nearLow {
			distCount++
		}
	}
	return distCount >= 3
}

// ──────────────────────────────────────────────────────────────────────────────
// Closing Helpers
// ──────────────────────────────────────────────────────────────────────────────

// Closes extracts Close prices from a slice of bars.
func Closes(bars []Bar) []float64 {
	out := make([]float64, len(bars))
	for i, b := range bars {
		out[i] = b.Close
	}
	return out
}

// Volumes extracts Volume from a slice of bars.
func Volumes(bars []Bar) []float64 {
	out := make([]float64, len(bars))
	for i, b := range bars {
		out[i] = b.Volume
	}
	return out
}

// MaxClose returns the maximum close price in the last N bars.
func MaxClose(closes []float64, n int) float64 {
	if len(closes) < n {
		n = len(closes)
	}
	max := closes[len(closes)-n]
	for _, v := range closes[len(closes)-n:] {
		if v > max {
			max = v
		}
	}
	return max
}

// MinClose returns the minimum close price in the last N bars.
func MinClose(closes []float64, n int) float64 {
	if len(closes) < n {
		n = len(closes)
	}
	min := closes[len(closes)-n]
	for _, v := range closes[len(closes)-n:] {
		if v < min {
			min = v
		}
	}
	return min
}

// ──────────────────────────────────────────────────────────────────────────────
// EMA Slope — rising / falling detection
// ──────────────────────────────────────────────────────────────────────────────

// EMASlope returns the percentage change of EMA(period) over the last `lookback`
// bars. Positive means the EMA is rising; negative means falling.
// Returns (0, false) when there is insufficient data.
func EMASlope(closes []float64, period, lookback int) (float64, bool) {
	if len(closes) < period+lookback {
		return 0, false
	}
	full := EMASlice(closes, period)
	if full == nil {
		return 0, false
	}
	n := len(full)
	past := full[n-1-lookback]
	if past == 0 {
		return 0, false
	}
	return (full[n-1] - past) / past * 100, true
}

// ──────────────────────────────────────────────────────────────────────────────
// Swing High / Low detection — structural support and resistance
// ──────────────────────────────────────────────────────────────────────────────

// SwingHighs returns the High values of bars that are local maxima within the
// last `lookback` bars. A local maximum: bar[i].High > bar[i-1].High AND
// bar[i].High > bar[i+1].High.
func SwingHighs(bars []Bar, lookback int) []float64 {
	n := len(bars)
	if n < 3 {
		return nil
	}
	start := n - lookback
	if start < 1 {
		start = 1
	}
	var highs []float64
	for i := start; i < n-1; i++ {
		if bars[i].High > bars[i-1].High && bars[i].High > bars[i+1].High {
			highs = append(highs, bars[i].High)
		}
	}
	return highs
}

// SwingLows returns the Low values of bars that are local minima within the
// last `lookback` bars.
func SwingLows(bars []Bar, lookback int) []float64 {
	n := len(bars)
	if n < 3 {
		return nil
	}
	start := n - lookback
	if start < 1 {
		start = 1
	}
	var lows []float64
	for i := start; i < n-1; i++ {
		if bars[i].Low < bars[i-1].Low && bars[i].Low < bars[i+1].Low {
			lows = append(lows, bars[i].Low)
		}
	}
	return lows
}

// NearestResistance returns the nearest swing high above `currentPrice` within
// the last `lookback` bars. Returns 0 if none is found.
func NearestResistance(bars []Bar, currentPrice float64, lookback int) float64 {
	nearest := 0.0
	for _, h := range SwingHighs(bars, lookback) {
		if h > currentPrice {
			if nearest == 0 || h < nearest {
				nearest = h
			}
		}
	}
	return nearest
}

// NearestSupport returns the nearest swing low below `currentPrice` within the
// last `lookback` bars. Returns 0 if none is found.
func NearestSupport(bars []Bar, currentPrice float64, lookback int) float64 {
	nearest := 0.0
	for _, l := range SwingLows(bars, lookback) {
		if l < currentPrice {
			if nearest == 0 || l > nearest {
				nearest = l
			}
		}
	}
	return nearest
}

// ──────────────────────────────────────────────────────────────────────────────
// ATR-based structure targets
// ──────────────────────────────────────────────────────────────────────────────

// ATRTargetRange computes structure-based base and stretch price targets from
// an ATR multiple. This replaces arbitrary percent targets.
//
//	Bullish: base = entry + 3.0×ATR,  stretch = entry + 4.5×ATR
//	Bearish: base = entry − 3.0×ATR,  stretch = entry − 4.5×ATR
//
// Multipliers raised from 2.0/3.5 to 3.0/4.5 — backtest showed avg wins of
// only +3.7% underlying because 2x ATR targets were too close, producing a
// 0.52:1 R/R that barely cleared break-even at 67% win rate.
// At 3x ATR base, R/R improves to ~2:1 vs the 1.5x ATR stop.
//
// Returns (0, 0, false) when ATR cannot be computed.
func ATRTargetRange(bars []Bar, period int, entry float64, direction string) (base, stretch float64, ok bool) {
	atr, atrOK := ATRLast(bars, period)
	if !atrOK || atr == 0 {
		return 0, 0, false
	}
	if direction == "bullish" || direction == "call" {
		return entry + 3.0*atr, entry + 4.5*atr, true
	}
	// bearish / put
	return entry - 3.0*atr, entry - 4.5*atr, true
}

// ──────────────────────────────────────────────────────────────────────────────
// Bear flag detection (counterpart to IsBullFlag)
// ──────────────────────────────────────────────────────────────────────────────

// IsBearFlag detects a bear flag: sharp decline followed by a shallow,
// low-volume consolidation that does not recover more than 50% of the pole.
func IsBearFlag(bars []Bar) bool {
	n := len(bars)
	if n < 15 {
		return false
	}
	poleEnd := n - 8
	poleStart := poleEnd - 5
	if poleStart < 0 {
		return false
	}
	// Pole: at least 8% decline
	poleDecline := (bars[poleStart].Close - bars[poleEnd].Close) / bars[poleStart].Close
	if poleDecline < 0.08 {
		return false
	}
	// Flag: last bars have not recovered more than 50% of the decline
	recovery := (bars[n-1].Close - bars[poleEnd].Close) / bars[poleEnd].Close
	return recovery < poleDecline*0.5
}

// ──────────────────────────────────────────────────────────────────────────────
// Higher-low / lower-high structure helpers
// ──────────────────────────────────────────────────────────────────────────────

// IsHigherLowContinuation returns true when the last three bar-level lows
// within the recent `lookback` bars are sequentially higher (bullish structure).
func IsHigherLowContinuation(bars []Bar, lookback int) bool {
	lows := SwingLows(bars, lookback)
	if len(lows) < 3 {
		return false
	}
	last := lows[len(lows)-3:]
	return last[1] > last[0] && last[2] > last[1]
}

// IsLowerHighBreakdown returns true when the last three bar-level highs within
// the recent `lookback` bars are sequentially lower (bearish structure).
func IsLowerHighBreakdown(bars []Bar, lookback int) bool {
	highs := SwingHighs(bars, lookback)
	if len(highs) < 3 {
		return false
	}
	last := highs[len(highs)-3:]
	return last[1] < last[0] && last[2] < last[1]
}

// ──────────────────────────────────────────────────────────────────────────────
// Realized Volatility — annualized standard deviation of log daily returns
// ──────────────────────────────────────────────────────────────────────────────

// RealizedVolatility computes the annualized realized volatility over the last
// `period` bars using log daily returns. Returns (0, false) when data is short.
//
//	realVol = stddev(logReturns) × sqrt(252)
//
// Used to normalize momentum across tickers (vol-scaled momentum).
func RealizedVolatility(closes []float64, period int) (float64, bool) {
	n := len(closes)
	if n < period+1 {
		return 0, false
	}
	// compute log returns for the last `period` days
	logReturns := make([]float64, period)
	for i := 0; i < period; i++ {
		idx := n - period + i
		if closes[idx-1] <= 0 {
			return 0, false
		}
		logReturns[i] = math.Log(closes[idx] / closes[idx-1])
	}
	// mean
	var sum float64
	for _, r := range logReturns {
		sum += r
	}
	mean := sum / float64(period)
	// variance
	var variance float64
	for _, r := range logReturns {
		diff := r - mean
		variance += diff * diff
	}
	variance /= float64(period - 1) // sample variance
	return math.Sqrt(variance) * math.Sqrt(252), true
}

// ──────────────────────────────────────────────────────────────────────────────
// Vol-Scaled Momentum — return normalized by realized volatility
// ──────────────────────────────────────────────────────────────────────────────

// VolScaledMomentum returns the N-day return divided by annualized realized
// volatility over the same window. Higher = stronger risk-adjusted move.
//
//	volScaledMom = ROC(N) / realizedVol(N)
//
// A value > 1.5 indicates a meaningful trend relative to recent noise.
// Returns (0, false) when data is insufficient or vol is zero.
func VolScaledMomentum(closes []float64, period int) (float64, bool) {
	roc, ok1 := ROCLast(closes, period)
	vol, ok2 := RealizedVolatility(closes, period)
	if !ok1 || !ok2 || vol == 0 {
		return 0, false
	}
	return (roc / 100) / vol, true // roc is %, convert to decimal first
}

// ──────────────────────────────────────────────────────────────────────────────
// Shannon Entropy of Daily Returns — measures trend vs noise
// ──────────────────────────────────────────────────────────────────────────────

// ShannonEntropy computes the entropy of the sign distribution of daily returns
// over the last `period` bars. Low entropy = clean directional trend.
// High entropy = choppy / mean-reverting.
//
//	H = -p_up * log2(p_up) - p_down * log2(p_down)
//
// Perfect trend (all up or all down): H = 0.0
// Coin flip (50/50): H = 1.0
// Returns (0, false) when data is insufficient.
func ShannonEntropy(closes []float64, period int) (float64, bool) {
	n := len(closes)
	if n < period+1 {
		return 0, false
	}
	var ups, downs int
	for i := n - period; i < n; i++ {
		if closes[i] > closes[i-1] {
			ups++
		} else if closes[i] < closes[i-1] {
			downs++
		}
		// flat days are ignored (neither up nor down)
	}
	total := ups + downs
	if total == 0 {
		return 1.0, true // all flat → maximum uncertainty
	}
	pUp := float64(ups) / float64(total)
	pDown := float64(downs) / float64(total)

	var entropy float64
	if pUp > 0 {
		entropy -= pUp * math.Log2(pUp)
	}
	if pDown > 0 {
		entropy -= pDown * math.Log2(pDown)
	}
	return entropy, true
}

// ──────────────────────────────────────────────────────────────────────────────
// Bollinger Band Width — envelope expansion / contraction signal
// ──────────────────────────────────────────────────────────────────────────────

// BollingerWidth computes the normalized Bollinger Band width over the last
// `period` bars. Width = (upper - lower) / middle, where middle is SMA(period).
//
// Expanding width → increasing volatility / breakout condition.
// Contracting width → squeeze — often precedes a directional move.
// Returns (0, false) when data is insufficient.
func BollingerWidth(closes []float64, period int, numStdDev float64) (float64, bool) {
	n := len(closes)
	if n < period {
		return 0, false
	}
	slice := closes[n-period:]

	// SMA
	var sum float64
	for _, v := range slice {
		sum += v
	}
	sma := sum / float64(period)
	if sma == 0 {
		return 0, false
	}

	// stddev
	var variance float64
	for _, v := range slice {
		diff := v - sma
		variance += diff * diff
	}
	stddev := math.Sqrt(variance / float64(period))

	upper := sma + numStdDev*stddev
	lower := sma - numStdDev*stddev
	return (upper - lower) / sma, true
}

// ──────────────────────────────────────────────────────────────────────────────
// Squeeze Ratio — Keltner Channel vs Bollinger Band compression
// ──────────────────────────────────────────────────────────────────────────────

// HighestHigh returns the highest bar High value over the last n bars.
// Includes all n bars up to and including the most recent.
func HighestHigh(bars []Bar, n int) float64 {
	if n <= 0 || len(bars) == 0 {
		return 0
	}
	start := len(bars) - n
	if start < 0 {
		start = 0
	}
	hi := bars[start].High
	for i := start + 1; i < len(bars); i++ {
		if bars[i].High > hi {
			hi = bars[i].High
		}
	}
	return hi
}

// LowestLow returns the lowest bar Low value over the last n bars.
// Includes all n bars up to and including the most recent.
func LowestLow(bars []Bar, n int) float64 {
	if n <= 0 || len(bars) == 0 {
		return 0
	}
	start := len(bars) - n
	if start < 0 {
		start = 0
	}
	lo := bars[start].Low
	for i := start + 1; i < len(bars); i++ {
		if bars[i].Low < lo {
			lo = bars[i].Low
		}
	}
	return lo
}

// AvgDailyVolume returns the simple average volume over the last n bars.
func AvgDailyVolume(volumes []float64, n int) float64 {
	if n <= 0 || len(volumes) == 0 {
		return 0
	}
	start := len(volumes) - n
	if start < 0 {
		start = 0
	}
	count := len(volumes) - start
	var sum float64
	for i := start; i < len(volumes); i++ {
		sum += volumes[i]
	}
	if count == 0 {
		return 0
	}
	return sum / float64(count)
}

// BollingerWidthPercentile returns where the current Bollinger Band width sits
// within its historical distribution over the prior `lookback` days.
//
// Returns a value 0.0-1.0: 0.0 means the current width is at or below the
// historical minimum (squeeze active); 1.0 means at or above the maximum
// (maximum expansion). A value < 0.25 indicates a volatility squeeze.
//
// Requires len(closes) >= period + lookback.
func BollingerWidthPercentile(closes []float64, period, lookback int, numStdDev float64) (float64, bool) {
	n := len(closes)
	if n < period+lookback {
		return 0, false
	}

	// Current BB width (most recent period bars).
	currentWidth, ok := BollingerWidth(closes, period, numStdDev)
	if !ok {
		return 0, false
	}

	// Historical widths: one per prior day in the lookback window.
	// Point i uses closes ending at n-lookback+i (excludes today).
	historicalWidths := make([]float64, lookback)
	for i := 0; i < lookback; i++ {
		end := n - lookback + i
		if end < period {
			return 0, false
		}
		slice := closes[end-period : end]
		var sum float64
		for _, v := range slice {
			sum += v
		}
		sma := sum / float64(period)
		if sma == 0 {
			return 0, false
		}
		var variance float64
		for _, v := range slice {
			d := v - sma
			variance += d * d
		}
		stddev := math.Sqrt(variance / float64(period))
		upper := sma + numStdDev*stddev
		lower := sma - numStdDev*stddev
		historicalWidths[i] = (upper - lower) / sma
	}

	// Percentile rank: fraction of historical widths BELOW the current width.
	// Low percentile = current width narrower than most of history = squeeze.
	belowCount := 0
	for _, w := range historicalWidths {
		if w < currentWidth {
			belowCount++
		}
	}
	pct := float64(belowCount) / float64(lookback)
	if pct < 0 {
		return 0, true
	}
	if pct > 1 {
		return 1, true
	}
	return pct, true
}

// ATRCompressionExists returns true when the current ATR is at least 10% lower
// than the ATR computed `lookback` bars ago, indicating a volatility contraction.
func ATRCompressionExists(bars []Bar, period, lookback int) bool {
	n := len(bars)
	if n < period+lookback+1 {
		return false
	}
	currentATR, ok1 := ATRLast(bars, period)
	pastATR, ok2 := ATRLast(bars[:n-lookback], period)
	if !ok1 || !ok2 || pastATR == 0 {
		return false
	}
	return currentATR < pastATR*0.90
}

// SqueezeRatio computes how compressed Bollinger Bands are relative to the
// Keltner Channel. A ratio < 1.0 indicates the bands are inside the channel —
// a classic "squeeze" condition that often precedes large directional moves.
//
//	squeezeRatio = BollingerWidth / KeltnerWidth
//	            = (4×stddev) / (4×ATR)   [both normalized by SMA / EMA20]
//
// Returns (0, false) when ATR cannot be computed or data is short.
func SqueezeRatio(bars []Bar, period int) (float64, bool) {
	if len(bars) < period {
		return 0, false
	}
	closes := Closes(bars)

	bWidth, ok1 := BollingerWidth(closes, period, 2.0)
	atr, ok2 := ATRLast(bars, period)
	if !ok1 || !ok2 || atr == 0 {
		return 0, false
	}

	// Keltner width (normalized): 4×ATR / EMA20
	ema20, ok3 := EMALast(closes, period)
	if !ok3 || ema20 == 0 {
		return 0, false
	}
	keltnerWidth := 4 * atr / ema20

	if keltnerWidth == 0 {
		return 0, false
	}
	return bWidth / keltnerWidth, true
}
