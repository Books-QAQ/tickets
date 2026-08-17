package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// DLX + TTL 死信延迟队列的组件命名。
// 用队列级 TTL（所有订单统一过期时间），避免 per-message TTL 的队头阻塞问题。
const (
	orderExpiryExchange      = "order.expire.dlx"      // 死信交换机
	orderExpiryDelayQueue    = "order.expire.delay"    // 延迟队列（TTL + DLX 绑定）
	orderExpiryBusinessQueue = "order.expire.business" // 业务队列（消费者实际消费）
	orderExpiryRoutingKey    = "order.expire"          // 死信路由 key
)

// OrderExpiryMessage 关单延迟消息内容。
type OrderExpiryMessage struct {
	OrderNo string `json:"order_no"`
}

// RabbitMQ 封装 amqp 连接与 channel。
type RabbitMQ struct {
	conn *amqp.Connection
	ch   *amqp.Channel
}

func NewRabbitMQ(url string) (*RabbitMQ, error) {
	conn, err := amqp.Dial(url)
	if err != nil {
		return nil, fmt.Errorf("failed to dial rabbitmq: %w", err)
	}
	ch, err := conn.Channel()
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("failed to open channel: %w", err)
	}
	return &RabbitMQ{conn: conn, ch: ch}, nil
}

// SetupOrderExpiry 声明死信交换机、延迟队列、业务队列。
// 延迟队列设置了 x-message-ttl（统一过期时间），消息到期变死信，
// 经死信交换机路由到业务队列，由消费者执行关单。
func (r *RabbitMQ) SetupOrderExpiry(ttl time.Duration) error {
	// ① 死信交换机（direct，持久化）
	if err := r.ch.ExchangeDeclare(orderExpiryExchange, "direct", true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare dlx exchange: %w", err)
	}

	// ② 延迟队列：TTL + 死信转发配置
	delayArgs := amqp.Table{
		"x-message-ttl":             int64(ttl.Milliseconds()),
		"x-dead-letter-exchange":    orderExpiryExchange,
		"x-dead-letter-routing-key": orderExpiryRoutingKey,
	}
	if _, err := r.ch.QueueDeclare(orderExpiryDelayQueue, true, false, false, false, delayArgs); err != nil {
		return fmt.Errorf("declare delay queue: %w", err)
	}

	// ③ 业务队列：绑定死信交换机
	if _, err := r.ch.QueueDeclare(orderExpiryBusinessQueue, true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare business queue: %w", err)
	}
	if err := r.ch.QueueBind(orderExpiryBusinessQueue, orderExpiryRoutingKey, orderExpiryExchange, false, nil); err != nil {
		return fmt.Errorf("bind business queue: %w", err)
	}
	return nil
}

// PublishOrderExpiry 发布关单延迟消息（下单时调用），消息持久化防 RabbitMQ 重启丢失。
func (r *RabbitMQ) PublishOrderExpiry(ctx context.Context, msg OrderExpiryMessage) error {
	payload, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return r.ch.PublishWithContext(ctx, "", orderExpiryDelayQueue, false, false, amqp.Publishing{
		ContentType:  "application/json",
		Body:         payload,
		DeliveryMode: amqp.Persistent,
	})
}

// ConsumeOrderExpiry 消费业务队列，返回 delivery 通道（手动 ack）。
func (r *RabbitMQ) ConsumeOrderExpiry() (<-chan amqp.Delivery, error) {
	return r.ch.Consume(orderExpiryBusinessQueue, "", false, false, false, false, nil)
}

func (r *RabbitMQ) Close() {
	if r.ch != nil {
		_ = r.ch.Close()
	}
	if r.conn != nil {
		_ = r.conn.Close()
	}
}
