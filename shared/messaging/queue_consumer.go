package messaging

import (
	"log"
	"time"

	"encoding/json"

	"ride-sharing/shared/contracts"

	amqp "github.com/rabbitmq/amqp091-go"
)

const (
	// qcMaxConcurrent 限制单个队列同时处理的消息数：既配 RabbitMQ prefetch
	// （未 ack 上限），又作为 worker goroutine 的信号量，防消息洪峰把 goroutine 打爆。
	qcMaxConcurrent = 64

	// qcSendRetries 是 SendMessage 失败后的额外重试次数。
	qcSendRetries = 2

	// qcRetryBackoff 是每次重试前的退避。2 次重试 ≈ 总 300ms，
	// 专抓“收件人 WS 晚连上几百 ms”这种亚秒级竞态。
	qcRetryBackoff = 150 * time.Millisecond
)

type QueueConsumer struct {
	rb        *RabbitMQ
	connMgr   *ConnectionManager
	queueName string
}

func NewQueueConsumer(rb *RabbitMQ, connMgr *ConnectionManager, queueName string) *QueueConsumer {
	return &QueueConsumer{
		rb:        rb,
		connMgr:   connMgr,
		queueName: queueName,
	}
}

// Start 起一个手动 ack 的消费者。
//
// 为什么不用 autoAck：autoAck=true 时消息一投递就出队，若那一刻收件人 WS
// 还没注册进 connManager（WS-first 客户端先连后下单的亚秒级竞态），SendMessage
// 失败即永久丢失、无重投，压测实测大量 driver_assigned/trip_request 因此丢掉。
//
// 为什么不做无限 requeue：rider 只等 15s，窗口外重投无意义；且单消费者
// Nack(requeue=true) 会把消息打回队首形成热循环 + 队首阻塞。这里用
// “有界 best-effort”：短退避重试几次抓亚秒级竞态，仍失败则 ack 丢弃
// （保证消息最终出队，绝不阻塞队列）。这是 best-effort 不是可靠投递——
// 延迟 >15s 或收件人始终不连仍会丢，属预期内。
//
// ack 不变量：handleDelivery 的每一条返回路径都 **恰好 ack 一次**
// （成功 / 重试耗尽 / 毒消息 / panic 兜底），无双 ack、无漏 ack
// （漏 ack + prefetch 会逐渐占满 prefetch 名额最终卡死队列）。
func (qc *QueueConsumer) Start() error {
	// prefetch 限制未 ack 在途消息数，与 worker 信号量同量级。
	if err := qc.rb.Channel.Qos(qcMaxConcurrent, 0, false); err != nil {
		return err
	}

	msgs, err := qc.rb.Channel.Consume(
		qc.queueName,
		"",
		false, // autoAck=false：改手动 ack
		false,
		false,
		false,
		nil,
	)
	if err != nil {
		return err
	}

	sem := make(chan struct{}, qcMaxConcurrent)
	go func() {
		for msg := range msgs {
			sem <- struct{}{}
			go func(d amqp.Delivery) {
				defer func() { <-sem }()
				qc.handleDelivery(d)
			}(msg)
		}
	}()

	return nil
}

// handleDelivery 处理单条消息，保证恰好 ack 一次。
func (qc *QueueConsumer) handleDelivery(msg amqp.Delivery) {
	// panic 兜底：手动 ack 模式下，未捕获的 panic 会让消息永远不 ack，
	// prefetch 名额泄漏最终卡死队列；这里兜底 ack 并记录。
	defer func() {
		if r := recover(); r != nil {
			log.Printf("panic handling message on queue %s: %v", qc.queueName, r)
			_ = msg.Ack(false)
		}
	}()

	var msgBody contracts.AmqpMessage
	if err := json.Unmarshal(msg.Body, &msgBody); err != nil {
		log.Println("Failed to unmarshal message:", err)
		_ = msg.Ack(false) // 毒消息无法恢复，ack 丢弃避免无限占位
		return
	}

	userID := msgBody.OwnerID

	var payload any
	if msgBody.Data != nil {
		if err := json.Unmarshal(msgBody.Data, &payload); err != nil {
			log.Println("Failed to unmarshal payload:", err)
			_ = msg.Ack(false)
			return
		}
	}

	clientMsg := contracts.WSMessage{
		Type: msg.RoutingKey,
		Data: payload,
	}

	var sendErr error
	for attempt := 0; attempt <= qcSendRetries; attempt++ {
		if sendErr = qc.connMgr.SendMessage(userID, clientMsg); sendErr == nil {
			_ = msg.Ack(false)
			return
		}
		if attempt < qcSendRetries {
			time.Sleep(qcRetryBackoff)
		}
	}

	log.Printf("drop message to user %s on queue %s after %d attempts: %v",
		userID, qc.queueName, qcSendRetries+1, sendErr)
	_ = msg.Ack(false) // best-effort 给不到就丢弃，保证出队、不阻塞队列
}
