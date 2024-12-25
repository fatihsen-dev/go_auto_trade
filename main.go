package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/joho/godotenv"
)

var (
	apiKey    string
	apiSecret string
	baseURL   = "https://api.binance.com"
	client    = &http.Client{Timeout: 10 * time.Second}

	cache24hVolume     = make(map[string]float64)
	cache24hVolumeTime = make(map[string]int64)
	cacheKlines        = make(map[string][]float64)
	cacheKlinesTime    = make(map[string]int64)
	mu                 sync.Mutex
)

type OrderResp struct {
	Code  int    `json:"code"`
	Msg   string `json:"msg"`
	Fills []struct {
		Price string `json:"price"`
		Qty   string `json:"qty"`
	} `json:"fills"`
}

type BalanceResp struct {
	Balances []struct {
		Asset  string `json:"asset"`
		Free   string `json:"free"`
		Locked string `json:"locked"`
	} `json:"balances"`
}

type PositionData struct {
	InPosition   bool
	LastBuyPrice float64
	Qty          float64
}

type pingError struct {
	statusCode int
	body       string
}

func (e *pingError) Error() string {
	return "ping failed, status code: " + strconv.Itoa(e.statusCode) + ", body: " + e.body
}

func init() {
	err := godotenv.Load()
	if err != nil {
		log.Fatalf("Error loading .env file: %v", err)
	}

	apiKey = os.Getenv("BINANCE_API_KEY")
	if apiKey == "" {
		log.Fatalln("BINANCE_API_KEY is missing or empty in .env file")
	}

	apiSecret = os.Getenv("BINANCE_API_SECRET")
	if apiSecret == "" {
		log.Fatalln("BINANCE_API_SECRET is missing or empty in .env file")
	}
}

func main() {
	err := testBinancePing()
	if err != nil {
		log.Fatalf("Binance ping test failed: %v", err)
	}
	log.Println("Binance ping test successful. Starting the bot...")
	portfolio := map[string]float64{
		"BTCUSDT": 80,
		"ETHUSDT": 40,
	}
	autoTradePortfolio(
		portfolio,
		3600,
		0.05,
		0.1,
		14,
		30,
		70,
		12,
		26,
		9,
		"15m",
		1e7,
		60,
	)
}

func testBinancePing() error {
	u := baseURL + "/api/v3/ping"
	resp, err := client.Get(u)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return &pingError{
			statusCode: resp.StatusCode,
			body:       string(body),
		}
	}
	return nil
}

func autoTradePortfolio(
	portfolio map[string]float64,
	buyInterval int64,
	stopLossPct float64,
	takeProfitPct float64,
	rsiPeriod int,
	buyRSIThreshold float64,
	sellRSIThreshold float64,
	macdShort int,
	macdLong int,
	macdSignal int,
	candleInterval string,
	volumeThreshold float64,
	refreshInterval int64,
) {
	positions := make(map[string]*PositionData)
	lastBuyTime := make(map[string]int64)
	for sym := range portfolio {
		positions[sym] = &PositionData{InPosition: false, LastBuyPrice: 0, Qty: 0}
		lastBuyTime[sym] = 0
	}
	for {
		for sym, allocation := range portfolio {
			posData := positions[sym]
			inPosition := posData.InPosition
			lastBuyPrice := posData.LastBuyPrice
			tNow := time.Now().Unix()
			if !inPosition {
				if tNow-lastBuyTime[sym] >= buyInterval {
					vol24h := get24hVolumeUSDT(sym, refreshInterval)
					if vol24h >= volumeThreshold {
						balUSDT := getBalance("USDT")
						if balUSDT >= allocation {
							klines := getKlines(sym, candleInterval, 50, refreshInterval)
							if len(klines) >= macdLong {
								rsiVal := calcRSI(klines, rsiPeriod)
								_, _, mHist := calcMACD(klines, macdShort, macdLong, macdSignal)
								if rsiVal <= buyRSIThreshold && mHist > 0 {
									currentPrice := getCurrentPrice(sym)
									if currentPrice > 0 {
										qty := math.Floor((allocation/currentPrice)*100000) / 100000
										_, avgFill, filledQty := placeMarketOrder(sym, "BUY", qty)
										if filledQty > 0 {
											positions[sym].InPosition = true
											positions[sym].LastBuyPrice = avgFill
											positions[sym].Qty = filledQty
											lastBuyTime[sym] = tNow
											log.Printf("Bought %s at %.4f, qty=%.5f", sym, avgFill, filledQty)
										}
									}
								}
							}
						}
					}
				}
			} else {
				currentPrice := getCurrentPrice(sym)
				if currentPrice > 0 && lastBuyPrice > 0 {
					if currentPrice <= lastBuyPrice*(1-stopLossPct) {
						coinSym := sym[:len(sym)-4]
						balCoin := getBalance(coinSym)
						if balCoin > 0 {
							_, avgFill, filledQty := placeMarketOrder(sym, "SELL", roundDown(balCoin, 5))
							if filledQty > 0 {
								positions[sym].InPosition = false
								positions[sym].LastBuyPrice = 0
								positions[sym].Qty = 0
								log.Printf("Stop-loss triggered for %s, sold at %.4f", sym, avgFill)
							}
						}
					} else if currentPrice >= lastBuyPrice*(1+takeProfitPct) {
						coinSym := sym[:len(sym)-4]
						balCoin := getBalance(coinSym)
						if balCoin > 0 {
							_, avgFill, filledQty := placeMarketOrder(sym, "SELL", roundDown(balCoin, 5))
							if filledQty > 0 {
								positions[sym].InPosition = false
								positions[sym].LastBuyPrice = 0
								positions[sym].Qty = 0
								log.Printf("Take-profit triggered for %s, sold at %.4f", sym, avgFill)
							}
						}
					} else {
						klines := getKlines(sym, candleInterval, 50, refreshInterval)
						if len(klines) >= macdLong {
							rsiVal := calcRSI(klines, rsiPeriod)
							_, _, mHist := calcMACD(klines, macdShort, macdLong, macdSignal)
							if rsiVal >= sellRSIThreshold && mHist < 0 {
								coinSym := sym[:len(sym)-4]
								balCoin := getBalance(coinSym)
								if balCoin > 0 {
									_, avgFill, filledQty := placeMarketOrder(sym, "SELL", roundDown(balCoin, 5))
									if filledQty > 0 {
										positions[sym].InPosition = false
										positions[sym].LastBuyPrice = 0
										positions[sym].Qty = 0
										log.Printf("RSI+MACD sell signal triggered for %s, sold at %.4f", sym, avgFill)
									}
								}
							}
						}
					}
				}
			}
		}
		time.Sleep(10 * time.Second)
	}
}

func get24hVolumeUSDT(symbol string, refreshInterval int64) float64 {
	mu.Lock()
	defer mu.Unlock()
	now := time.Now().Unix()
	if v, ok := cache24hVolume[symbol]; ok {
		lastT := cache24hVolumeTime[symbol]
		if (now - lastT) < refreshInterval {
			return v
		}
	}
	u := baseURL + "/api/v3/ticker/24hr?symbol=" + symbol
	d := safeGet(u)
	if d == nil {
		cache24hVolume[symbol] = 0
		cache24hVolumeTime[symbol] = now
		return 0
	}
	quoteVol, ok := d["quoteVolume"].(string)
	if !ok {
		cache24hVolume[symbol] = 0
		cache24hVolumeTime[symbol] = now
		return 0
	}
	vol, err := strconv.ParseFloat(quoteVol, 64)
	if err != nil {
		vol = 0
	}
	cache24hVolume[symbol] = vol
	cache24hVolumeTime[symbol] = now
	return vol
}

func getKlines(symbol, interval string, limit int, refreshInterval int64) []float64 {
	mu.Lock()
	defer mu.Unlock()
	now := time.Now().Unix()
	cacheKey := symbol + "_" + interval + "_" + strconv.Itoa(limit)
	if kl, ok := cacheKlines[cacheKey]; ok {
		lastT := cacheKlinesTime[cacheKey]
		if (now - lastT) < refreshInterval {
			return kl
		}
	}
	u := baseURL + "/api/v3/klines?symbol=" + symbol + "&interval=" + interval + "&limit=" + strconv.Itoa(limit)
	arr := safeGetArray(u)
	if arr == nil {
		cacheKlines[cacheKey] = []float64{}
		cacheKlinesTime[cacheKey] = now
		return []float64{}
	}
	var closes []float64
	for _, v := range arr {
		vv, ok := v.([]interface{})
		if ok && len(vv) >= 5 {
			cs, _ := vv[4].(string)
			cf, err := strconv.ParseFloat(cs, 64)
			if err == nil {
				closes = append(closes, cf)
			}
		}
	}
	cacheKlines[cacheKey] = closes
	cacheKlinesTime[cacheKey] = now
	return closes
}

func safeGetArray(url string) []interface{} {
	for i := 0; i < 3; i++ {
		resp, err := client.Get(url)
		if err == nil && resp.StatusCode == 200 {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			var result []interface{}
			e := json.Unmarshal(b, &result)
			if e == nil {
				return result
			}
		}
		time.Sleep(2 * time.Second)
	}
	return nil
}

func getCurrentPrice(symbol string) float64 {
	u := baseURL + "/api/v3/ticker/price?symbol=" + symbol
	d := safeGet(u)
	if d == nil {
		return 0
	}
	ps, ok := d["price"].(string)
	if !ok {
		return 0
	}
	val, err := strconv.ParseFloat(ps, 64)
	if err != nil {
		return 0
	}
	return val
}

func getBalance(asset string) float64 {
	t := time.Now().UnixMilli()
	q := "timestamp=" + strconv.FormatInt(t, 10)
	sign := createSignature(q, apiSecret)
	u := baseURL + "/api/v3/account?" + q + "&signature=" + sign
	h := map[string]string{"X-MBX-APIKEY": apiKey}
	resp := safeGetWithHeaders(u, h)
	if resp == nil {
		return 0
	}
	var bResp BalanceResp
	j, _ := json.Marshal(resp)
	json.Unmarshal(j, &bResp)
	for _, b := range bResp.Balances {
		if b.Asset == asset {
			f, _ := strconv.ParseFloat(b.Free, 64)
			return f
		}
	}
	return 0
}

func placeMarketOrder(symbol, side string, quantity float64) (OrderResp, float64, float64) {
	ts := time.Now().UnixMilli()
	qs := "symbol=" + symbol + "&side=" + side + "&type=MARKET&quantity=" + strconv.FormatFloat(quantity, 'f', -1, 64) +
		"&timestamp=" + strconv.FormatInt(ts, 10)
	s := createSignature(qs, apiSecret)
	u := baseURL + "/api/v3/order?" + qs + "&signature=" + s
	h := map[string]string{"X-MBX-APIKEY": apiKey}
	log.Printf("Placing %s order on %s, qty=%.5f", side, symbol, quantity)
	r := safePostWithHeaders(u, h)
	if r == nil {
		log.Printf("Order response is nil for %s %s", symbol, side)
		return OrderResp{Code: -999}, 0, 0
	}
	var o OrderResp
	j, _ := json.Marshal(r)
	json.Unmarshal(j, &o)
	if o.Code < 0 {
		log.Printf("Binance error code: %d, msg: %s", o.Code, o.Msg)
		return o, 0, 0
	}
	ap, tq := parseOrderFills(o)
	log.Printf("Order result: avg_price=%.5f, filled_qty=%.5f", ap, tq)
	return o, ap, tq
}

func parseOrderFills(o OrderResp) (float64, float64) {
	var totalCost float64
	var totalQty float64
	for _, f := range o.Fills {
		p, _ := strconv.ParseFloat(f.Price, 64)
		q, _ := strconv.ParseFloat(f.Qty, 64)
		totalCost += p * q
		totalQty += q
	}
	if totalQty > 0 {
		return totalCost / totalQty, totalQty
	}
	return 0, 0
}

func calcRSI(data []float64, period int) float64 {
	if len(data) < period+1 {
		return 50
	}
	var gains float64
	var losses float64
	for i := 0; i < period; i++ {
		diff := data[len(data)-1-i] - data[len(data)-2-i]
		if diff > 0 {
			gains += diff
		} else {
			losses -= diff
		}
	}
	ag := gains / float64(period)
	al := losses / float64(period)
	if al == 0 {
		return 100
	}
	rs := ag / al
	r := 100 - (100 / (1 + rs))
	return r
}

func calcMACD(data []float64, shortW, longW, signalW int) (float64, float64, float64) {
	s := ema(data, shortW)
	l := ema(data, longW)
	var mLine []float64
	if len(s) < len(l) {
		df := len(l) - len(s)
		var z []float64
		for i := 0; i < df; i++ {
			z = append(z, 0)
		}
		s = append(z, s...)
	} else if len(l) < len(s) {
		df := len(s) - len(l)
		var z []float64
		for i := 0; i < df; i++ {
			z = append(z, 0)
		}
		l = append(z, l...)
	}
	for i := 0; i < len(l); i++ {
		mLine = append(mLine, s[i]-l[i])
	}
	sig := ema(mLine, signalW)
	var hist []float64
	if len(sig) < len(mLine) {
		df2 := len(mLine) - len(sig)
		var z2 []float64
		for i := 0; i < df2; i++ {
			z2 = append(z2, 0)
		}
		sig = append(z2, sig...)
	}
	for i := 0; i < len(sig); i++ {
		hist = append(hist, mLine[i]-sig[i])
	}
	if len(mLine) == 0 || len(sig) == 0 || len(hist) == 0 {
		return 0, 0, 0
	}
	return mLine[len(mLine)-1], sig[len(sig)-1], hist[len(hist)-1]
}

func ema(data []float64, window int) []float64 {
	if len(data) < window || window <= 0 {
		return []float64{}
	}
	k := 2.0 / (float64(window) + 1)
	var res []float64
	s := 0.0
	for i := 0; i < window; i++ {
		s += data[i]
	}
	f := s / float64(window)
	res = append(res, f)
	for i := window; i < len(data); i++ {
		val := data[i]*k + res[len(res)-1]*(1-k)
		res = append(res, val)
	}
	return res
}

func safeGet(url string) map[string]interface{} {
	for i := 0; i < 3; i++ {
		resp, err := client.Get(url)
		if err == nil && resp.StatusCode == 200 {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			var result map[string]interface{}
			_ = json.Unmarshal(b, &result)
			if result != nil {
				return result
			}
		}
		time.Sleep(2 * time.Second)
	}
	return nil
}

func safeGetWithHeaders(url string, headers map[string]string) map[string]interface{} {
	for i := 0; i < 3; i++ {
		req, _ := http.NewRequest("GET", url, nil)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err == nil && resp.StatusCode == 200 {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			var r map[string]interface{}
			_ = json.Unmarshal(b, &r)
			if r != nil {
				return r
			}
		}
		time.Sleep(2 * time.Second)
	}
	return nil
}

func safePostWithHeaders(url string, headers map[string]string) map[string]interface{} {
	for i := 0; i < 3; i++ {
		req, _ := http.NewRequest("POST", url, nil)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err == nil && (resp.StatusCode == 200 || resp.StatusCode == 201) {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			var r map[string]interface{}
			_ = json.Unmarshal(b, &r)
			if r != nil {
				return r
			}
		}
		time.Sleep(2 * time.Second)
	}
	return nil
}

func createSignature(query, secret string) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(query))
	return hex.EncodeToString(h.Sum(nil))
}

func roundDown(val float64, decimals int) float64 {
	p := math.Pow10(decimals)
	return math.Floor(val*p) / p
}
