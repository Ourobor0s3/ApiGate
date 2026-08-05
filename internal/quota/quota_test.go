package quota

import (
	"testing"

	"github.com/Ourobor0s3/ApiGate/internal/notify"
)

func TestExhaustedNotifierDisabledWhenNoWebhook(t *testing.T) {
	if fn := ExhaustedNotifier(nil, nil); fn != nil {
		t.Fatal("ExhaustedNotifier with no client must return nil")
	}
	if fn := ExhaustedNotifier(nil, notify.New("")); fn != nil {
		t.Fatal("ExhaustedNotifier with disabled client must return nil")
	}
}
