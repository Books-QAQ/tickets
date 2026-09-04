package payment

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

	"github.com/smartwalle/alipay/v3"
)

// AlipayProvider 支付宝渠道（沙箱 / 生产）实现。
// 通过 smartwalle/alipay SDK 调用支付宝开放平台 API。
type AlipayProvider struct {
	client *alipay.Client
	// 是否启用支付宝渠道：沙箱未配置密钥时为 false，走降级。
	enabled bool
	// 支付宝侧交易超时（如 "15m"），与订单过期时间对齐。
	timeoutExpress string
}

type AlipayConfig struct {
	AppID        string // 沙箱或正式 AppID
	PrivateKey   string // 应用私钥（RSA2，PKCS8 格式）
	AlipayPubKey string // 支付宝公钥（用于验签回调）
	IsProduction bool   // false=沙箱环境，true=正式环境
	// TimeoutExpress 支付宝侧交易超时（如 "15m"），应与订单过期时间对齐，
	// 使支付宝在订单过期后也关闭交易，减少"钱已付、单已关"的错位。
	TimeoutExpress string
}

// NewAlipayProvider 创建支付宝渠道。
// 若 AppID/私钥任一为空，返回 enabled=false 的实例，上层据此降级到 mock。
func NewAlipayProvider(cfg AlipayConfig) (*AlipayProvider, error) {
	p := &AlipayProvider{enabled: false}
	if cfg.AppID == "" || cfg.PrivateKey == "" {
		return p, nil
	}

	client, err := alipay.New(cfg.AppID, cfg.PrivateKey, cfg.IsProduction)
	if err != nil {
		return nil, fmt.Errorf("init alipay client: %w", err)
	}
	// 验签支付宝公钥（用于异步通知验签）
	if cfg.AlipayPubKey != "" {
		if err := client.LoadAliPayPublicKey(cfg.AlipayPubKey); err != nil {
			return nil, fmt.Errorf("load alipay public key: %w", err)
		}
	}

	p.client = client
	p.enabled = true
	p.timeoutExpress = cfg.TimeoutExpress
	return p, nil
}

func (p *AlipayProvider) Name() string {
	return "alipay"
}

func (p *AlipayProvider) IsEnabled() bool {
	return p.enabled
}

// CreatePayment 调用 alipay.trade.precreate（当面付二维码预下单）生成二维码。
// 选择 precreate 而非 page.pay：沙箱环境对 page.pay（网页跳转）支持不完整（返回 500），
// 而 precreate 走纯 API、沙箱完整支持（已实测 code=10000 Success）。
func (p *AlipayProvider) CreatePayment(ctx context.Context, order Order, notifyURL string, returnURL string) (*CreatePaymentResult, error) {
	if !p.enabled {
		return nil, fmt.Errorf("alipay channel is not configured")
	}

	var param alipay.TradePreCreate
	param.NotifyURL = notifyURL
	param.Subject = order.Subject
	param.OutTradeNo = order.OrderNo
	param.TotalAmount = yuanToStr(order.Amount)
	if p.timeoutExpress != "" {
		param.TimeoutExpress = p.timeoutExpress
	}

	result, err := p.client.TradePreCreate(ctx, param)
	if err != nil {
		return nil, fmt.Errorf("alipay trade precreate: %w", err)
	}
	if result.Code != alipay.CodeSuccess {
		return nil, fmt.Errorf("alipay trade precreate failed: %s %s", result.SubCode, result.SubMsg)
	}

	return &CreatePaymentResult{
		Channel: "alipay",
		QrCode:  result.QRCode,
	}, nil
}

// QueryOrder 调用 alipay.trade.query 主动查询订单状态。
func (p *AlipayProvider) QueryOrder(ctx context.Context, orderNo string) (bool, string, error) {
	if !p.enabled {
		return false, "", fmt.Errorf("alipay channel is not configured")
	}

	var param alipay.TradeQuery
	param.OutTradeNo = orderNo

	result, err := p.client.TradeQuery(ctx, param)
	if err != nil {
		return false, "", fmt.Errorf("alipay trade query: %w", err)
	}

	switch result.TradeStatus {
	case alipay.TradeStatusSuccess, alipay.TradeStatusFinished:
		return true, result.TradeNo, nil
	default:
		return false, result.TradeNo, nil
	}
}

// VerifyNotify 验签并解析支付宝异步通知。
func (p *AlipayProvider) VerifyNotify(values url.Values) (string, bool, error) {
	if !p.enabled {
		return "", false, fmt.Errorf("alipay channel is not configured")
	}

	noti, err := p.client.DecodeNotification(context.Background(), values)
	if err != nil {
		return "", false, fmt.Errorf("verify alipay notification: %w", err)
	}

	paid := noti.TradeStatus == alipay.TradeStatusSuccess || noti.TradeStatus == alipay.TradeStatusFinished
	return noti.OutTradeNo, paid, nil
}

// Refund 支付宝退款，用于"已扣款但订单已关"的钱悬空兜底。
// OutRequestNo 用订单号，保证同一订单重复退款请求在支付宝侧幂等（不重复退）。
func (p *AlipayProvider) Refund(ctx context.Context, orderNo string, amount int32, reason string) error {
	if !p.enabled {
		return fmt.Errorf("alipay channel is not configured")
	}

	var param alipay.TradeRefund
	param.OutTradeNo = orderNo
	param.RefundAmount = yuanToStr(amount)
	param.RefundReason = reason
	param.OutRequestNo = orderNo // 幂等：同一订单的退款请求号固定

	result, err := p.client.TradeRefund(ctx, param)
	if err != nil {
		return fmt.Errorf("alipay trade refund: %w", err)
	}
	if result.Code != alipay.CodeSuccess {
		return fmt.Errorf("alipay trade refund failed: %s %s", result.SubCode, result.SubMsg)
	}
	return nil
}

// yuanToStr 将金额（单位：元，可为整数）转换为支付宝 total_amount 要求的
// 两位小数字符串，如 88 → "88.00"。
func yuanToStr(yuan int32) string {
	return strconv.FormatFloat(float64(yuan), 'f', 2, 64)
}
