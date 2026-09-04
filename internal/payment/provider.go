package payment

import (
	"context"
	"net/url"
)

// Order 是支付层需要的最小订单信息，避免直接依赖 db 包。
type Order struct {
	OrderNo string
	Amount  int32 // 单位：元
	Subject string
}

// CreatePaymentResult 创建支付的结果。
// 对于二维码渠道（支付宝当面付），QrCode 为二维码内容，前端渲染二维码扫码付款；
// 对于跳转型渠道，PayURL 为跳转地址。
type CreatePaymentResult struct {
	Channel string `json:"channel"`
	PayURL  string `json:"pay_url,omitempty"`
	QrCode  string `json:"qr_code,omitempty"`
}

// Provider 抽象支付渠道。mock / alipay 都实现该接口，
// 上层 handler 只依赖接口，不感知具体渠道。
type Provider interface {
	// Name 返回渠道标识，如 "mock" / "alipay"。
	Name() string

	// CreatePayment 创建支付（生成支付跳转 URL 或直连完成支付）。
	CreatePayment(ctx context.Context, order Order, notifyURL string, returnURL string) (*CreatePaymentResult, error)

	// QueryOrder 主动查询订单支付状态。
	// 返回 paid=true 表示买家已完成付款（渠道侧 TRADE_SUCCESS/TRADE_FINISHED）。
	QueryOrder(ctx context.Context, orderNo string) (paid bool, tradeNo string, err error)

	// VerifyNotify 验签并解析异步通知，返回商户订单号与是否支付成功。
	// 验签失败返回 error，调用方必须拒绝处理该通知。
	VerifyNotify(values url.Values) (orderNo string, paid bool, err error)

	// Refund 退款：用于"已扣款但订单已关"的钱悬空兜底。
	// amount 单位：元；reason 退款原因。实现须保证幂等（同一订单重复调用不重复退款）。
	Refund(ctx context.Context, orderNo string, amount int32, reason string) error
}
