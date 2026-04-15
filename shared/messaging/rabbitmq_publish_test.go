package messaging

import (
	"testing"

	amqp "github.com/rabbitmq/amqp091-go"
)

func TestNewPersistentJSONMessage_SetsMessageID(t *testing.T) {
	msg := newPersistentJSONMessage([]byte(`{"ok":true}`))

	if msg.MessageId == "" {
		t.Fatalf("message id should not be empty")
	}
	if msg.DeliveryMode != amqp.Persistent {
		t.Fatalf("delivery mode should be persistent (%d), got %d", amqp.Persistent, msg.DeliveryMode)
	}
	if msg.ContentType != "application/json" {
		t.Fatalf("unexpected content type: %s", msg.ContentType)
	}
	if string(msg.Body) != `{"ok":true}` {
		t.Fatalf("unexpected body: %s", string(msg.Body))
	}
}
