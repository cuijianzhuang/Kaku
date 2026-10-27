package payment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

const USDTContract = "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"
const contractHex = "a614f803b6fd780986a42c78ec9c7f77e6ded13c"
const transferTopic = "ddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"

var txPattern = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
var ErrPending = errors.New("链上交易尚未最终确认")

func ValidTXID(v string) bool { return txPattern.MatchString(v) }
func addressHex(s string) (string, error) {
	if len(s) != 34 {
		return "", errors.New("无效 TRON 地址")
	}
	n := new(big.Int)
	alphabet := "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
	for _, c := range s {
		i := strings.IndexRune(alphabet, c)
		if i < 0 {
			return "", errors.New("无效 TRON 地址")
		}
		n.Mul(n, big.NewInt(58))
		n.Add(n, big.NewInt(int64(i)))
	}
	b := n.Bytes()
	if len(b) != 25 || b[0] != 0x41 {
		return "", errors.New("无效 TRON 主网地址")
	}
	h := sha256.Sum256(b[:21])
	h = sha256.Sum256(h[:])
	if !bytes.Equal(h[:4], b[21:]) {
		return "", errors.New("TRON 地址校验失败")
	}
	return hex.EncodeToString(b[1:21]), nil
}
func ValidTRONAddress(s string) bool { _, e := addressHex(s); return e == nil }

type Transfer struct {
	TXID, Address string
	Index         int
	Amount, At    int64
}

// TRONConfig configures the TronGrid client.
type TRONConfig struct {
	TRONAPIKey  string
	TRONBaseURL string // default https://api.trongrid.io
	AllowHTTP   bool   // tests only: no pacing, plain HTTP allowed
}

// TRON reads USDT-TRC20 transfers from TronGrid.
type TRON struct {
	cfg    TRONConfig
	client *http.Client
	mu     sync.Mutex
	next   time.Time
}

// NewTRON returns a TronGrid client.
func NewTRON(c TRONConfig) *TRON {
	if c.TRONBaseURL == "" {
		c.TRONBaseURL = "https://api.trongrid.io"
	}
	return &TRON{cfg: c, client: &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (t *TRON) request(ctx context.Context, method, path string, body any, out any) error {
	if !t.cfg.AllowHTTP {
		gap := 1100 * time.Millisecond
		if t.cfg.TRONAPIKey != "" {
			gap = 100 * time.Millisecond
		}
		t.mu.Lock()
		at := time.Now()
		if t.next.After(at) {
			at = t.next
		}
		t.next = at.Add(gap)
		t.mu.Unlock()
		if delay := time.Until(at); delay > 0 {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
			}
		}
	}

	var data io.Reader
	if body != nil {
		b, e := json.Marshal(body)
		if e != nil {
			return errors.New("无法编码链上请求")
		}
		data = bytes.NewReader(b)
	}
	req, e := http.NewRequestWithContext(ctx, method, strings.TrimRight(t.cfg.TRONBaseURL, "/")+path, data)
	if e != nil {
		return errors.New("链上接口配置无效")
	}
	req.Header.Set("Content-Type", "application/json")
	if t.cfg.TRONAPIKey != "" {
		req.Header.Set("TRON-PRO-API-KEY", t.cfg.TRONAPIKey)
	}
	resp, e := t.client.Do(req)
	if e != nil {
		return errors.New("TRON 节点暂不可用")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return errors.New("TRON 节点拒绝请求或限流")
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out) != nil {
		return errors.New("TRON 响应格式无效")
	}
	return nil
}

// Solidity RPC exposes solidified blocks. Validate the receipt and the actual
// USDT Transfer log rather than trusting an explorer's symbol or amount alone.
func (t *TRON) Verify(ctx context.Context, txid, address string) ([]Transfer, error) {
	if !ValidTXID(txid) {
		return nil, errors.New("交易哈希须为64位十六进制")
	}
	recipient, e := addressHex(address)
	if e != nil {
		return nil, e
	}
	var r struct {
		ID      string `json:"id"`
		Block   int64  `json:"blockNumber"`
		Time    int64  `json:"blockTimeStamp"`
		Receipt struct {
			Result string `json:"result"`
		} `json:"receipt"`
		Logs []struct {
			Address string   `json:"address"`
			Topics  []string `json:"topics"`
			Data    string   `json:"data"`
		} `json:"log"`
	}
	txid = strings.ToLower(txid)
	if e = t.request(ctx, "POST", "/walletsolidity/gettransactioninfobyid", map[string]string{"value": txid}, &r); e != nil {
		return nil, e
	}
	if r.ID == "" || r.Block < 1 || r.Time < 1 {
		return nil, ErrPending
	}
	if strings.ToLower(r.ID) != txid || r.Receipt.Result != "SUCCESS" {
		return nil, errors.New("链上交易失败或回执不匹配")
	}
	out := []Transfer{}
	for i, l := range r.Logs {
		a := strings.ToLower(strings.TrimPrefix(l.Address, "0x"))
		if len(a) == 42 && strings.HasPrefix(a, "41") {
			a = a[2:]
		}
		if a != contractHex || len(l.Topics) != 3 || strings.ToLower(l.Topics[0]) != transferTopic {
			continue
		}
		topic := strings.ToLower(l.Topics[2])
		if len(topic) != 64 || topic[:24] != strings.Repeat("0", 24) || topic[24:] != recipient {
			continue
		}
		if len(l.Data) != 64 {
			return nil, errors.New("USDT 转账日志金额无效")
		}
		raw, err := hex.DecodeString(l.Data)
		if err != nil {
			return nil, errors.New("USDT 转账日志不是十六进制")
		}
		n := new(big.Int).SetBytes(raw)
		if !n.IsInt64() || n.Sign() <= 0 {
			return nil, errors.New("USDT 转账金额超出范围")
		}
		out = append(out, Transfer{TXID: txid, Address: address, Index: i, Amount: n.Int64(), At: r.Time / 1000})
	}
	if len(out) == 0 {
		return nil, errors.New("交易中没有转入指定地址的主网 USDT")
	}
	return out, nil
}
func (t *TRON) Scan(ctx context.Context, address string, since int64, visit func(string) error) error {
	if !ValidTRONAddress(address) {
		return errors.New("收款地址无效")
	}
	fingerprint := ""
	seen := map[string]bool{}
	for range 10 {
		q := url.Values{"only_confirmed": {"true"}, "only_to": {"true"}, "contract_address": {USDTContract}, "limit": {"200"}, "order_by": {"block_timestamp,asc"}, "min_timestamp": {big.NewInt(since * 1000).String()}}
		if fingerprint != "" {
			q.Set("fingerprint", fingerprint)
		}
		var r struct {
			Success bool `json:"success"`
			Data    []struct {
				ID    string `json:"transaction_id"`
				To    string `json:"to"`
				Type  string `json:"type"`
				Token struct {
					Address  string `json:"address"`
					Decimals int    `json:"decimals"`
				} `json:"token_info"`
			} `json:"data"`
			Meta struct {
				Fingerprint string `json:"fingerprint"`
				Links       struct {
					Next string `json:"next"`
				} `json:"links"`
			} `json:"meta"`
		}
		if e := t.request(ctx, "GET", "/v1/accounts/"+address+"/transactions/trc20?"+q.Encode(), nil, &r); e != nil {
			return e
		}
		if !r.Success {
			return errors.New("TRON 索引查询未成功")
		}
		for _, v := range r.Data {
			if v.To != address || v.Type != "Transfer" || v.Token.Address != USDTContract || v.Token.Decimals != 6 || !ValidTXID(v.ID) {
				continue
			}
			id := strings.ToLower(v.ID)
			if seen[id] {
				continue
			}
			seen[id] = true
			if e := visit(id); e != nil {
				return e
			}
		}
		next := r.Meta.Fingerprint
		if next == "" && r.Meta.Links.Next != "" {
			if u, e := url.Parse(r.Meta.Links.Next); e == nil {
				next = u.Query().Get("fingerprint")
			}
		}
		if next == "" {
			if len(r.Data) < 200 {
				return nil
			}
			return errors.New("TRON 分页信息缺失，扫描未完成")
		}
		if len(next) > 1024 || next == fingerprint {
			return errors.New("TRON 分页标识无效")
		}
		fingerprint = next
	}
	return errors.New("自动扫描达到2000条上限，请使用交易哈希核验或人工核对")
}
