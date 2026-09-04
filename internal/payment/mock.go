package payment

import (
	"context"
	"net/url"
)

// MockProvider 模拟支付渠道：QueryOrder 恒返回已支付，
// 用于保持原有"下单后直接支付成功"的行为不变。
type MockProvider struct{}

func NewMockProvider() *MockProvider {
	return &MockProvider{}
}

func (m *MockProvider) Name() string {
	return "mock"
}

func (m *MockProvider) CreatePayment(ctx context.Context, order Order, notifyURL string, returnURL string) (*CreatePaymentResult, error) {
	return &CreatePaymentResult{Channel: "mock", PayURL: ""}, nil
}

func (m *MockProvider) QueryOrder(ctx context.Context, orderNo string) (bool, string, error) {
	return true, "", nil
}

func (m *MockProvider) VerifyNotify(values url.Values) (string, bool, error) {
	return "", false, nil
}

// Refund mock 渠道无真实资金，退款为空操作。
func (m *MockProvider) Refund(ctx context.Context, orderNo string, amount int32, reason string) error {
	return nil
}
