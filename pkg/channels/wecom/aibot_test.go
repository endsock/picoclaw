package wecom

import (
	"context"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
)

func TestNewWeComAIBotChannel(t *testing.T) {
	t.Run("success with valid config", func(t *testing.T) {
		cfg := config.WeComAIBotConfig{
			Enabled:        true,
			Token:          "test_token",
			EncodingAESKey: "testkey1234567890123456789012345678901234567",
			WebhookPath:    "/webhook/test",
		}

		messageBus := bus.NewMessageBus()
		ch, err := NewWeComAIBotChannel(cfg, messageBus)
		if err != nil {
			t.Fatalf("Expected no error, got %v", err)
		}

		if ch == nil {
			t.Fatal("Expected channel to be created")
		}

		if ch.Name() != "wecom_aibot" {
			t.Errorf("Expected name 'wecom_aibot', got '%s'", ch.Name())
		}
	})

	t.Run("error with missing token", func(t *testing.T) {
		cfg := config.WeComAIBotConfig{
			Enabled:        true,
			EncodingAESKey: "testkey1234567890123456789012345678901234567",
		}

		messageBus := bus.NewMessageBus()
		_, err := NewWeComAIBotChannel(cfg, messageBus)

		if err == nil {
			t.Fatal("Expected error for missing token, got nil")
		}
	})

	t.Run("error with missing encoding key", func(t *testing.T) {
		cfg := config.WeComAIBotConfig{
			Enabled: true,
			Token:   "test_token",
		}

		messageBus := bus.NewMessageBus()
		_, err := NewWeComAIBotChannel(cfg, messageBus)

		if err == nil {
			t.Fatal("Expected error for missing encoding key, got nil")
		}
	})
}

func TestNewWeComAIBotWebSocketChannel(t *testing.T) {
	t.Run("success with valid websocket config", func(t *testing.T) {
		cfg := config.WeComAIBotConfig{
			Enabled: true,
			Mode:    "websocket",
			BotID:   "bot-123",
			Secret:  "secret-123",
		}

		messageBus := bus.NewMessageBus()
		ch, err := NewWeComAIBotWebSocketChannel(cfg, messageBus)
		if err != nil {
			t.Fatalf("Expected no error, got %v", err)
		}
		if ch == nil {
			t.Fatal("Expected websocket channel to be created")
		}
		if ch.Name() != "wecom_aibot" {
			t.Errorf("Expected name 'wecom_aibot', got '%s'", ch.Name())
		}
		if ch.BaseChannel.MaxMessageLength() != 0 {
			t.Errorf("Expected websocket channel max message length 0, got %d", ch.BaseChannel.MaxMessageLength())
		}
	})

	t.Run("error with missing bot id", func(t *testing.T) {
		cfg := config.WeComAIBotConfig{
			Enabled: true,
			Mode:    "websocket",
			Secret:  "secret-123",
		}

		messageBus := bus.NewMessageBus()
		_, err := NewWeComAIBotWebSocketChannel(cfg, messageBus)
		if err == nil {
			t.Fatal("Expected error for missing bot_id, got nil")
		}
	})

	t.Run("error with missing secret", func(t *testing.T) {
		cfg := config.WeComAIBotConfig{
			Enabled: true,
			Mode:    "websocket",
			BotID:   "bot-123",
		}

		messageBus := bus.NewMessageBus()
		_, err := NewWeComAIBotWebSocketChannel(cfg, messageBus)
		if err == nil {
			t.Fatal("Expected error for missing secret, got nil")
		}
	})
}

func TestWeComAIBotChannelStartStop(t *testing.T) {
	cfg := config.WeComAIBotConfig{
		Enabled:        true,
		Token:          "test_token",
		EncodingAESKey: "testkey1234567890123456789012345678901234567",
	}

	messageBus := bus.NewMessageBus()
	ch, err := NewWeComAIBotChannel(cfg, messageBus)
	if err != nil {
		t.Fatalf("Failed to create channel: %v", err)
	}

	ctx := context.Background()

	if err := ch.Start(ctx); err != nil {
		t.Fatalf("Failed to start channel: %v", err)
	}

	if !ch.IsRunning() {
		t.Error("Expected channel to be running")
	}

	if err := ch.Stop(ctx); err != nil {
		t.Fatalf("Failed to stop channel: %v", err)
	}

	if ch.IsRunning() {
		t.Error("Expected channel to be stopped")
	}
}

func TestWeComAIBotWebSocketChannelStartStopWithoutServer(t *testing.T) {
	cfg := config.WeComAIBotConfig{
		Enabled: true,
		Mode:    "websocket",
		BotID:   "bot-123",
		Secret:  "secret-123",
		WSURL:   "ws://127.0.0.1:1",
	}

	messageBus := bus.NewMessageBus()
	ch, err := NewWeComAIBotWebSocketChannel(cfg, messageBus)
	if err != nil {
		t.Fatalf("Failed to create websocket channel: %v", err)
	}

	ctx := context.Background()
	if err := ch.Start(ctx); err != nil {
		t.Fatalf("Failed to start websocket channel: %v", err)
	}
	if !ch.IsRunning() {
		t.Fatal("Expected websocket channel to be running")
	}
	if err := ch.Stop(ctx); err != nil {
		t.Fatalf("Failed to stop websocket channel: %v", err)
	}
	if ch.IsRunning() {
		t.Fatal("Expected websocket channel to be stopped")
	}
}

func TestWeComAIBotChannelWebhookPath(t *testing.T) {
	t.Run("default path", func(t *testing.T) {
		cfg := config.WeComAIBotConfig{
			Enabled:        true,
			Token:          "test_token",
			EncodingAESKey: "testkey1234567890123456789012345678901234567",
		}

		messageBus := bus.NewMessageBus()
		ch, _ := NewWeComAIBotChannel(cfg, messageBus)

		expectedPath := "/webhook/wecom-aibot"
		if ch.WebhookPath() != expectedPath {
			t.Errorf("Expected webhook path '%s', got '%s'", expectedPath, ch.WebhookPath())
		}
	})

	t.Run("custom path", func(t *testing.T) {
		customPath := "/custom/webhook"
		cfg := config.WeComAIBotConfig{
			Enabled:        true,
			Token:          "test_token",
			EncodingAESKey: "testkey1234567890123456789012345678901234567",
			WebhookPath:    customPath,
		}

		messageBus := bus.NewMessageBus()
		ch, _ := NewWeComAIBotChannel(cfg, messageBus)

		if ch.WebhookPath() != customPath {
			t.Errorf("Expected webhook path '%s', got '%s'", customPath, ch.WebhookPath())
		}
	})
}

func TestGenerateStreamID(t *testing.T) {
	cfg := config.WeComAIBotConfig{
		Enabled:        true,
		Token:          "test_token",
		EncodingAESKey: "testkey1234567890123456789012345678901234567",
	}

	messageBus := bus.NewMessageBus()
	ch, _ := NewWeComAIBotChannel(cfg, messageBus)

	ids := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id := ch.generateStreamID()

		if len(id) != 10 {
			t.Errorf("Expected stream ID length 10, got %d", len(id))
		}

		if ids[id] {
			t.Errorf("Duplicate stream ID generated: %s", id)
		}
		ids[id] = true
	}
}

func TestGenerateWSReqID(t *testing.T) {
	cfg := config.WeComAIBotConfig{
		Enabled: true,
		Mode:    "websocket",
		BotID:   "bot-123",
		Secret:  "secret-123",
	}

	messageBus := bus.NewMessageBus()
	ch, _ := NewWeComAIBotWebSocketChannel(cfg, messageBus)

	ids := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id := ch.generateWSReqID("ping")
		if id == "" {
			t.Fatal("Generated ws req id is empty")
		}
		if ids[id] {
			t.Fatalf("Duplicate ws req id generated: %s", id)
		}
		ids[id] = true
	}
}

func TestSplitTextByBytes(t *testing.T) {
	t.Run("empty string", func(t *testing.T) {
		chunks := splitTextByBytes("", 10)
		if len(chunks) != 1 || chunks[0] != "" {
			t.Fatalf("Expected [\"\"], got %#v", chunks)
		}
	})

	t.Run("ascii split", func(t *testing.T) {
		chunks := splitTextByBytes("abcdefghij", 4)
		expected := []string{"abcd", "efgh", "ij"}
		if len(chunks) != len(expected) {
			t.Fatalf("Expected %d chunks, got %d: %#v", len(expected), len(chunks), chunks)
		}
		for i := range expected {
			if chunks[i] != expected[i] {
				t.Fatalf("Expected chunk %d = %q, got %q", i, expected[i], chunks[i])
			}
		}
	})

	t.Run("utf8 split keeps rune boundary", func(t *testing.T) {
		chunks := splitTextByBytes("你好世界", 6)
		expected := []string{"你好", "世界"}
		if len(chunks) != len(expected) {
			t.Fatalf("Expected %d chunks, got %d: %#v", len(expected), len(chunks), chunks)
		}
		for i := range expected {
			if chunks[i] != expected[i] {
				t.Fatalf("Expected chunk %d = %q, got %q", i, expected[i], chunks[i])
			}
		}
	})
}

func TestWeComAIBotWebSocketHandleReplyAck(t *testing.T) {
	cfg := config.WeComAIBotConfig{
		Enabled: true,
		Mode:    "websocket",
		BotID:   "bot-123",
		Secret:  "secret-123",
	}

	messageBus := bus.NewMessageBus()
	ch, err := NewWeComAIBotWebSocketChannel(cfg, messageBus)
	if err != nil {
		t.Fatalf("Failed to create websocket channel: %v", err)
	}

	reqID := "req-1"
	item := &wecomAIBotWSReplyItem{resultCh: make(chan error, 1)}
	timer := time.NewTimer(time.Minute)
	defer timer.Stop()

	ch.replyQueues[reqID] = []*wecomAIBotWSReplyItem{item}
	ch.pendingAcks[reqID] = &wecomAIBotWSPendingAck{
		item:  item,
		seq:   1,
		timer: timer,
	}

	ch.handleReplyAck(reqID, wecomAIBotWSIncomingFrame{
		Headers: wecomAIBotWSHeaders{ReqID: reqID},
		ErrCode: 0,
		ErrMsg:  "ok",
	})

	select {
	case err := <-item.resultCh:
		if err != nil {
			t.Fatalf("Expected nil ack result, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Timed out waiting for ack result")
	}

	if _, ok := ch.pendingAcks[reqID]; ok {
		t.Fatal("Expected pending ack to be removed")
	}
	if _, ok := ch.replyQueues[reqID]; ok {
		t.Fatal("Expected reply queue to be removed")
	}
}

func TestEncryptDecrypt(t *testing.T) {
	cfg := config.WeComAIBotConfig{
		Enabled:        true,
		Token:          "test_token",
		EncodingAESKey: "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG",
	}

	messageBus := bus.NewMessageBus()
	ch, _ := NewWeComAIBotChannel(cfg, messageBus)

	plaintext := "Hello, World!"
	receiveid := ""

	encrypted, err := ch.encryptMessage(plaintext, receiveid)
	if err != nil {
		t.Fatalf("Failed to encrypt message: %v", err)
	}

	if encrypted == "" {
		t.Fatal("Encrypted message is empty")
	}

	decrypted, err := decryptMessageWithVerify(encrypted, cfg.EncodingAESKey, receiveid)
	if err != nil {
		t.Fatalf("Failed to decrypt message: %v", err)
	}

	if decrypted != plaintext {
		t.Errorf("Expected decrypted message '%s', got '%s'", plaintext, decrypted)
	}
}

func TestGenerateSignature(t *testing.T) {
	token := "test_token"
	timestamp := "1234567890"
	nonce := "test_nonce"
	encrypt := "encrypted_msg"

	signature := computeSignature(token, timestamp, nonce, encrypt)

	if signature == "" {
		t.Error("Generated signature is empty")
	}

	if !verifySignature(token, signature, timestamp, nonce, encrypt) {
		t.Error("Generated signature does not verify correctly")
	}
}
