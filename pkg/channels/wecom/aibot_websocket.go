package wecom

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/identity"
	"github.com/sipeed/picoclaw/pkg/logger"
)

const (
	wecomAIBotDefaultWSURL        = "wss://openws.work.weixin.qq.com"
	wecomAIBotWSCmdSubscribe      = "aibot_subscribe"
	wecomAIBotWSCmdHeartbeat      = "ping"
	wecomAIBotWSCmdResponse       = "aibot_respond_msg"
	wecomAIBotWSCmdWelcome        = "aibot_respond_welcome_msg"
	wecomAIBotWSCmdSendMsg        = "aibot_send_msg"
	wecomAIBotWSCmdCallback       = "aibot_msg_callback"
	wecomAIBotWSCmdEventCallback  = "aibot_event_callback"
	wecomAIBotWSChunkBytes        = 16 * 1024
	wecomAIBotWSWriteTimeout      = 10 * time.Second
	wecomAIBotWSReplyAckTimeout   = 5 * time.Second
	wecomAIBotWSCleanupInterval   = 5 * time.Minute
	wecomAIBotWSTaskMaxLifetime   = 1 * time.Hour
	wecomAIBotWSResponseTimeout   = 15 * time.Second
	wecomAIBotWSDefaultScene      = 1
	wecomAIBotWSDefaultQueueSize  = 500
	wecomAIBotWSDefaultReconnects = 10
	wecomAIBotWSDefaultAuthFails  = 5
)

type wecomAIBotWSHeaders struct {
	ReqID string `json:"req_id,omitempty"`
}

type wecomAIBotWSIncomingFrame struct {
	Cmd     string              `json:"cmd,omitempty"`
	Headers wecomAIBotWSHeaders `json:"headers,omitempty"`
	Body    json.RawMessage     `json:"body,omitempty"`
	ErrCode int                 `json:"errcode,omitempty"`
	ErrMsg  string              `json:"errmsg,omitempty"`
}

type wecomAIBotWSOutgoingFrame struct {
	Cmd     string              `json:"cmd,omitempty"`
	Headers wecomAIBotWSHeaders `json:"headers,omitempty"`
	Body    any                 `json:"body,omitempty"`
}

type wecomAIBotWSReplyItem struct {
	frame    wecomAIBotWSOutgoingFrame
	resultCh chan error
}

type wecomAIBotWSPendingAck struct {
	item  *wecomAIBotWSReplyItem
	seq   uint64
	timer *time.Timer
}

type wecomAIBotWSTask struct {
	ReqID       string
	StreamID    string
	MsgID       string
	ChatID      string
	ChatType    string
	UserID      string
	ResponseURL string
	CreatedTime time.Time
	ctx         context.Context
	cancel      context.CancelFunc
	Finished    bool
}

// WeComAIBotWebSocketChannel implements the WebSocket long-connection version of WeCom AI Bot.
type WeComAIBotWebSocketChannel struct {
	*channels.BaseChannel
	config        config.WeComAIBotConfig
	httpClient    *http.Client
	processedMsgs *MessageDeduplicator

	ctx    context.Context
	cancel context.CancelFunc

	conn    *websocket.Conn
	connMu  sync.RWMutex
	writeMu sync.Mutex

	authenticated        atomic.Bool
	manualClose          atomic.Bool
	suppressReconnect    atomic.Bool
	lastCloseAuthFailure atomic.Bool
	missedPongCount      atomic.Int32
	reqCounter           atomic.Uint64

	reconnectAttempts   int
	authFailureAttempts int
	reconnectMu         sync.Mutex
	reconnectTimer      *time.Timer

	replyMu       sync.Mutex
	replyQueues   map[string][]*wecomAIBotWSReplyItem
	pendingAcks   map[string]*wecomAIBotWSPendingAck
	pendingAckSeq uint64

	taskMu    sync.RWMutex
	reqTasks  map[string]*wecomAIBotWSTask
	chatTasks map[string][]*wecomAIBotWSTask
}

func NewWeComAIBotWebSocketChannel(
	cfg config.WeComAIBotConfig,
	messageBus *bus.MessageBus,
) (*WeComAIBotWebSocketChannel, error) {
	if cfg.BotID == "" || cfg.Secret == "" {
		return nil, fmt.Errorf("bot_id and secret are required for WeCom AI Bot websocket mode")
	}

	base := channels.NewBaseChannel("wecom_aibot", cfg, messageBus, cfg.AllowFrom,
		channels.WithReasoningChannelID(cfg.ReasoningChannelID),
	)

	return &WeComAIBotWebSocketChannel{
		BaseChannel:   base,
		config:        cfg,
		httpClient:    &http.Client{Timeout: wecomAIBotWSResponseTimeout},
		processedMsgs: NewMessageDeduplicator(wecomMaxProcessedMessages),
		replyQueues:   make(map[string][]*wecomAIBotWSReplyItem),
		pendingAcks:   make(map[string]*wecomAIBotWSPendingAck),
		reqTasks:      make(map[string]*wecomAIBotWSTask),
		chatTasks:     make(map[string][]*wecomAIBotWSTask),
	}, nil
}

func (c *WeComAIBotWebSocketChannel) Name() string {
	return "wecom_aibot"
}

func (c *WeComAIBotWebSocketChannel) Start(ctx context.Context) error {
	logger.InfoC("wecom_aibot_ws", "Starting WeCom AI Bot WebSocket channel...")
	c.ctx, c.cancel = context.WithCancel(ctx)
	c.manualClose.Store(false)
	c.suppressReconnect.Store(false)
	c.lastCloseAuthFailure.Store(false)
	c.authenticated.Store(false)
	c.missedPongCount.Store(0)

	go c.cleanupLoop()

	if err := c.connect(); err != nil {
		logger.WarnCF("wecom_aibot_ws", "Initial WebSocket connection failed, will retry in background", map[string]any{
			"error": err.Error(),
		})
		c.scheduleReconnect(false)
	}

	c.SetRunning(true)
	logger.InfoC("wecom_aibot_ws", "WeCom AI Bot WebSocket channel started")
	return nil
}

func (c *WeComAIBotWebSocketChannel) Stop(ctx context.Context) error {
	logger.InfoC("wecom_aibot_ws", "Stopping WeCom AI Bot WebSocket channel...")
	c.SetRunning(false)
	c.manualClose.Store(true)

	if c.cancel != nil {
		c.cancel()
	}
	c.stopReconnectTimer()
	c.closeCurrentConnection()
	c.clearPendingReplies(fmt.Errorf("channel stopped"))
	c.clearTasks()
	logger.InfoC("wecom_aibot_ws", "WeCom AI Bot WebSocket channel stopped")
	return nil
}

func (c *WeComAIBotWebSocketChannel) HealthPath() string {
	return "/health/wecom-aibot"
}

func (c *WeComAIBotWebSocketChannel) HealthHandler(w http.ResponseWriter, r *http.Request) {
	status := map[string]any{
		"status":        "ok",
		"running":       c.IsRunning(),
		"authenticated": c.authenticated.Load(),
		"connected":     c.isConnected(),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
}

func (c *WeComAIBotWebSocketChannel) Send(ctx context.Context, msg bus.OutboundMessage) error {
	if !c.IsRunning() {
		return channels.ErrNotRunning
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	logger.InfoCF("wecom_aibot_ws", "Sending outbound reply", map[string]any{
		"chat_id":     msg.ChatID,
		"content_len": len(msg.Content),
	})

	task := c.peekTaskByChatID(msg.ChatID)
	if task == nil {
		logger.WarnCF("wecom_aibot_ws", "No active task for chat, falling back to proactive send", map[string]any{
			"chat_id":     msg.ChatID,
			"content_len": len(msg.Content),
		})
		if c.authenticated.Load() && c.isConnected() {
			if proactiveErr := c.sendActiveMarkdown(msg.ChatID, msg.Content); proactiveErr == nil {
				logger.InfoCF("wecom_aibot_ws", "Proactive fallback send succeeded", map[string]any{
					"chat_id": msg.ChatID,
				})
				return nil
			} else {
				logger.WarnCF("wecom_aibot_ws", "Proactive fallback send failed", map[string]any{
					"chat_id": msg.ChatID,
					"error":   proactiveErr.Error(),
				})
				return proactiveErr
			}
		}
		return fmt.Errorf("no active websocket task for chat_id=%s: %w", msg.ChatID, channels.ErrTemporary)
	}

	logger.InfoCF("wecom_aibot_ws", "Matched outbound reply to websocket task", map[string]any{
		"chat_id":   task.ChatID,
		"req_id":    task.ReqID,
		"stream_id": task.StreamID,
		"msg_id":    task.MsgID,
	})

	if err := c.sendStreamReply(task, msg.Content); err == nil {
		logger.InfoCF("wecom_aibot_ws", "Passive websocket reply sent successfully", map[string]any{
			"chat_id":   task.ChatID,
			"req_id":    task.ReqID,
			"stream_id": task.StreamID,
		})
		c.removeTask(task)
		return nil
	} else {
		logger.WarnCF("wecom_aibot_ws", "WebSocket passive reply failed, trying fallbacks", map[string]any{
			"chat_id":   task.ChatID,
			"req_id":    task.ReqID,
			"stream_id": task.StreamID,
			"error":     err.Error(),
		})
		if task.ResponseURL != "" {
			if respErr := c.sendViaResponseURL(task.ResponseURL, msg.Content); respErr == nil {
				logger.InfoCF("wecom_aibot_ws", "response_url fallback succeeded", map[string]any{
					"chat_id":   task.ChatID,
					"req_id":    task.ReqID,
					"stream_id": task.StreamID,
				})
				c.removeTask(task)
				return nil
			} else {
				logger.WarnCF("wecom_aibot_ws", "response_url fallback failed", map[string]any{
					"chat_id":   task.ChatID,
					"req_id":    task.ReqID,
					"stream_id": task.StreamID,
					"error":     respErr.Error(),
				})
			}
		}
		if c.authenticated.Load() && c.isConnected() {
			if proactiveErr := c.sendActiveMarkdown(task.ChatID, msg.Content); proactiveErr == nil {
				logger.InfoCF("wecom_aibot_ws", "Active markdown fallback succeeded", map[string]any{
					"chat_id": task.ChatID,
				})
				c.removeTask(task)
				return nil
			} else {
				logger.WarnCF("wecom_aibot_ws", "Active markdown fallback failed", map[string]any{
					"chat_id": task.ChatID,
					"error":   proactiveErr.Error(),
				})
			}
		}
		return err
	}
}

func (c *WeComAIBotWebSocketChannel) connect() error {
	wsURL := c.config.WSURL
	if wsURL == "" {
		wsURL = wecomAIBotDefaultWSURL
	}
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	conn, resp, err := dialer.DialContext(c.ctx, wsURL, nil)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return err
	}

	conn.SetPingHandler(func(appData string) error {
		c.writeMu.Lock()
		defer c.writeMu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(wecomAIBotWSWriteTimeout))
		err := conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(wecomAIBotWSWriteTimeout))
		_ = conn.SetWriteDeadline(time.Time{})
		return err
	})

	c.connMu.Lock()
	c.conn = conn
	c.connMu.Unlock()

	go c.readLoop(conn)
	if err := c.sendAuth(); err != nil {
		c.closeCurrentConnection()
		return err
	}
	return nil
}

func (c *WeComAIBotWebSocketChannel) readLoop(conn *websocket.Conn) {
	defer c.handleConnectionClosed(conn)
	for {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				logger.WarnCF("wecom_aibot_ws", "WebSocket read error", map[string]any{
					"error": err.Error(),
				})
			}
			return
		}

		var frame wecomAIBotWSIncomingFrame
		if err := json.Unmarshal(payload, &frame); err != nil {
			logger.WarnCF("wecom_aibot_ws", "Failed to parse WebSocket frame", map[string]any{
				"error":   err.Error(),
				"payload": string(payload),
			})
			continue
		}

		c.handleIncomingFrame(frame)
	}
}

func (c *WeComAIBotWebSocketChannel) handleIncomingFrame(frame wecomAIBotWSIncomingFrame) {
	reqID := frame.Headers.ReqID
	switch frame.Cmd {
	case wecomAIBotWSCmdCallback:
		go c.handleMessageCallback(frame)
		return
	case wecomAIBotWSCmdEventCallback:
		go c.handleEventCallback(frame)
		return
	}

	switch {
	case strings.HasPrefix(reqID, wecomAIBotWSCmdSubscribe):
		c.handleAuthAck(frame)
	case strings.HasPrefix(reqID, wecomAIBotWSCmdHeartbeat):
		if frame.ErrCode == 0 {
			c.missedPongCount.Store(0)
		}
	case reqID != "":
		c.handleReplyAck(reqID, frame)
	default:
		logger.DebugCF("wecom_aibot_ws", "Ignoring unknown WebSocket frame", map[string]any{
			"cmd": frame.Cmd,
		})
	}
}

func (c *WeComAIBotWebSocketChannel) handleAuthAck(frame wecomAIBotWSIncomingFrame) {
	if frame.ErrCode != 0 {
		logger.ErrorCF("wecom_aibot_ws", "WebSocket authentication failed", map[string]any{
			"errcode": frame.ErrCode,
			"errmsg":  frame.ErrMsg,
		})
		c.lastCloseAuthFailure.Store(true)
		c.closeCurrentConnection()
		return
	}
	logger.InfoC("wecom_aibot_ws", "WebSocket authentication successful")
	c.reconnectMu.Lock()
	c.reconnectAttempts = 0
	c.authFailureAttempts = 0
	c.reconnectMu.Unlock()
	c.authenticated.Store(true)
	c.missedPongCount.Store(0)
	go c.heartbeatLoop()
}

func (c *WeComAIBotWebSocketChannel) handleMessageCallback(frame wecomAIBotWSIncomingFrame) {
	var msg WeComAIBotMessage
	if err := json.Unmarshal(frame.Body, &msg); err != nil {
		logger.ErrorCF("wecom_aibot_ws", "Failed to parse message callback body", map[string]any{
			"error": err.Error(),
		})
		return
	}
	if !c.processedMsgs.MarkMessageProcessed(msg.MsgID) {
		logger.DebugCF("wecom_aibot_ws", "Skipping duplicate message callback", map[string]any{
			"msg_id": msg.MsgID,
		})
		return
	}

	switch msg.MsgType {
	case "text":
		c.handleTextCallback(frame.Headers.ReqID, msg)
	default:
		streamID := c.generateWSStreamID()
		if err := c.sendStreamFrame(frame.Headers.ReqID, streamID, "Unsupported message type: "+msg.MsgType, true); err != nil {
			logger.WarnCF("wecom_aibot_ws", "Failed to reply unsupported message", map[string]any{
				"msg_type": msg.MsgType,
				"error":    err.Error(),
			})
		}
	}
}

func (c *WeComAIBotWebSocketChannel) handleTextCallback(reqID string, msg WeComAIBotMessage) {
	if msg.Text == nil {
		logger.WarnC("wecom_aibot_ws", "Text callback missing text field")
		return
	}
	content := msg.Text.Content
	userID := msg.From.UserID
	if userID == "" {
		userID = "unknown"
	}
	chatID := msg.ChatID
	if chatID == "" {
		chatID = userID
	}
	sender := bus.SenderInfo{
		Platform:    "wecom_aibot",
		PlatformID:  userID,
		CanonicalID: identity.BuildCanonicalID("wecom_aibot", userID),
		DisplayName: userID,
	}
	if !c.IsAllowedSender(sender) {
		logger.DebugCF("wecom_aibot_ws", "Message rejected by allowlist", map[string]any{
			"user_id": userID,
		})
		return
	}

	taskCtx, taskCancel := context.WithCancel(c.ctx)
	task := &wecomAIBotWSTask{
		ReqID:       reqID,
		StreamID:    c.generateWSStreamID(),
		MsgID:       msg.MsgID,
		ChatID:      chatID,
		ChatType:    msg.ChatType,
		UserID:      userID,
		ResponseURL: msg.ResponseURL,
		CreatedTime: time.Now(),
		ctx:         taskCtx,
		cancel:      taskCancel,
	}
	c.addTask(task)
	logger.InfoCF("wecom_aibot_ws", "Created websocket task for inbound message", map[string]any{
		"chat_id":      task.ChatID,
		"user_id":      task.UserID,
		"req_id":       task.ReqID,
		"stream_id":    task.StreamID,
		"msg_id":       task.MsgID,
		"response_url": task.ResponseURL != "",
	})

	if err := c.sendStreamFrame(task.ReqID, task.StreamID, "", false); err != nil {
		logger.WarnCF("wecom_aibot_ws", "Failed to send initial stream frame", map[string]any{
			"req_id":    task.ReqID,
			"stream_id": task.StreamID,
			"error":     err.Error(),
		})
	}

	go func() {
		peerKind := "direct"
		if msg.ChatType == "group" {
			peerKind = "group"
		}
		peer := bus.Peer{Kind: peerKind, ID: chatID}
		metadata := map[string]string{
			"channel":      "wecom_aibot",
			"transport":    "websocket",
			"chat_type":    msg.ChatType,
			"msg_type":     "text",
			"msgid":        msg.MsgID,
			"aibotid":      msg.AIBotID,
			"stream_id":    task.StreamID,
			"ws_req_id":    task.ReqID,
			"response_url": msg.ResponseURL,
		}
		c.HandleMessage(task.ctx, peer, msg.MsgID, userID, chatID, content, nil, metadata, sender)
	}()
}

func (c *WeComAIBotWebSocketChannel) handleEventCallback(frame wecomAIBotWSIncomingFrame) {
	var msg WeComAIBotMessage
	if err := json.Unmarshal(frame.Body, &msg); err != nil {
		logger.ErrorCF("wecom_aibot_ws", "Failed to parse event callback body", map[string]any{
			"error": err.Error(),
		})
		return
	}
	if msg.Event == nil {
		return
	}
	eventType := msg.Event.EventType
	logger.DebugCF("wecom_aibot_ws", "Received event callback", map[string]any{
		"event_type": eventType,
	})

	if eventType == "disconnected_event" {
		c.suppressReconnect.Store(true)
		c.closeCurrentConnection()
		return
	}
	if eventType == "enter_chat" && c.config.WelcomeMessage != "" {
		body := map[string]any{
			"msgtype": "text",
			"text": map[string]string{
				"content": c.config.WelcomeMessage,
			},
		}
		if err := c.sendFrameAndWait(frame.Headers.ReqID, wecomAIBotWSCmdWelcome, body); err != nil {
			logger.WarnCF("wecom_aibot_ws", "Failed to send welcome message", map[string]any{
				"error": err.Error(),
			})
		}
	}
}

func (c *WeComAIBotWebSocketChannel) heartbeatLoop() {
	interval := time.Duration(c.config.HeartbeatInterval) * time.Second
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			if !c.authenticated.Load() || !c.isConnected() {
				return
			}
			if c.missedPongCount.Load() >= 2 {
				logger.WarnC("wecom_aibot_ws", "Heartbeat ack missed twice, closing WebSocket")
				c.closeCurrentConnection()
				return
			}
			c.missedPongCount.Add(1)
			frame := wecomAIBotWSOutgoingFrame{
				Cmd:     wecomAIBotWSCmdHeartbeat,
				Headers: wecomAIBotWSHeaders{ReqID: c.generateWSReqID(wecomAIBotWSCmdHeartbeat)},
			}
			if err := c.writeFrame(frame); err != nil {
				logger.WarnCF("wecom_aibot_ws", "Failed to send heartbeat", map[string]any{
					"error": err.Error(),
				})
				c.closeCurrentConnection()
				return
			}
		}
	}
}

func (c *WeComAIBotWebSocketChannel) sendAuth() error {
	body := map[string]any{
		"bot_id": c.config.BotID,
		"secret": c.config.Secret,
	}
	scene := c.config.Scene
	if scene <= 0 {
		scene = wecomAIBotWSDefaultScene
	}
	body["scene"] = scene
	if c.config.PlugVersion != "" {
		body["plug_version"] = c.config.PlugVersion
	}
	return c.writeFrame(wecomAIBotWSOutgoingFrame{
		Cmd:     wecomAIBotWSCmdSubscribe,
		Headers: wecomAIBotWSHeaders{ReqID: c.generateWSReqID(wecomAIBotWSCmdSubscribe)},
		Body:    body,
	})
}

func (c *WeComAIBotWebSocketChannel) sendStreamReply(task *wecomAIBotWSTask, content string) error {
	chunks := splitTextByBytes(content, wecomAIBotWSChunkBytes)
	for i, chunk := range chunks {
		finish := i == len(chunks)-1
		if err := c.sendStreamFrame(task.ReqID, task.StreamID, chunk, finish); err != nil {
			return err
		}
	}
	return nil
}

func (c *WeComAIBotWebSocketChannel) sendStreamFrame(reqID, streamID, content string, finish bool) error {
	body := WeComAIBotStreamResponse{
		MsgType: "stream",
		Stream: WeComAIBotStreamInfo{
			ID:      streamID,
			Finish:  finish,
			Content: content,
		},
	}
	return c.sendFrameAndWait(reqID, wecomAIBotWSCmdResponse, body)
}

func (c *WeComAIBotWebSocketChannel) sendActiveMarkdown(chatID, content string) error {
	body := map[string]any{
		"chatid":  chatID,
		"msgtype": "markdown",
		"markdown": map[string]string{
			"content": content,
		},
	}
	return c.sendFrameAndWait(c.generateWSReqID(wecomAIBotWSCmdSendMsg), wecomAIBotWSCmdSendMsg, body)
}

func (c *WeComAIBotWebSocketChannel) sendFrameAndWait(reqID, cmd string, body any) error {
	item := &wecomAIBotWSReplyItem{
		frame: wecomAIBotWSOutgoingFrame{
			Cmd:     cmd,
			Headers: wecomAIBotWSHeaders{ReqID: reqID},
			Body:    body,
		},
		resultCh: make(chan error, 1),
	}

	queueWasEmpty := false
	c.replyMu.Lock()
	if _, ok := c.replyQueues[reqID]; !ok {
		c.replyQueues[reqID] = nil
	}
	if len(c.replyQueues[reqID]) == 0 {
		queueWasEmpty = true
	}
	maxQueue := c.config.MaxReplyQueueSize
	if maxQueue <= 0 {
		maxQueue = wecomAIBotWSDefaultQueueSize
	}
	if len(c.replyQueues[reqID]) >= maxQueue {
		c.replyMu.Unlock()
		return fmt.Errorf("reply queue overflow for req_id=%s: %w", reqID, channels.ErrTemporary)
	}
	c.replyQueues[reqID] = append(c.replyQueues[reqID], item)
	c.replyMu.Unlock()

	if queueWasEmpty {
		go c.processReplyQueue(reqID)
	}
	if err := <-item.resultCh; err != nil {
		return err
	}
	return nil
}

func (c *WeComAIBotWebSocketChannel) processReplyQueue(reqID string) {
	c.replyMu.Lock()
	queue := c.replyQueues[reqID]
	if len(queue) == 0 {
		delete(c.replyQueues, reqID)
		c.replyMu.Unlock()
		return
	}
	if _, exists := c.pendingAcks[reqID]; exists {
		c.replyMu.Unlock()
		return
	}
	item := queue[0]
	c.pendingAckSeq++
	seq := c.pendingAckSeq
	ackTimeout := time.Duration(c.config.ReplyAckTimeout) * time.Second
	if ackTimeout <= 0 {
		ackTimeout = wecomAIBotWSReplyAckTimeout
	}
	logger.DebugCF("wecom_aibot_ws", "Sending websocket frame and waiting for ack", map[string]any{
		"req_id":       reqID,
		"cmd":          item.frame.Cmd,
		"queue_length": len(queue),
		"ack_timeout":  ackTimeout.String(),
	})
	timer := time.AfterFunc(ackTimeout, func() {
		c.handleReplyTimeout(reqID, seq)
	})
	c.pendingAcks[reqID] = &wecomAIBotWSPendingAck{item: item, seq: seq, timer: timer}
	c.replyMu.Unlock()

	if err := c.writeFrame(item.frame); err != nil {
		c.replyMu.Lock()
		pending := c.pendingAcks[reqID]
		if pending != nil && pending.seq == seq {
			delete(c.pendingAcks, reqID)
			pending.timer.Stop()
			if len(c.replyQueues[reqID]) > 0 {
				c.replyQueues[reqID] = c.replyQueues[reqID][1:]
			}
			if len(c.replyQueues[reqID]) == 0 {
				delete(c.replyQueues, reqID)
			}
		}
		c.replyMu.Unlock()
		logger.WarnCF("wecom_aibot_ws", "Failed to write websocket frame", map[string]any{
			"req_id": reqID,
			"cmd":    item.frame.Cmd,
			"error":  err.Error(),
		})
		item.resultCh <- fmt.Errorf("websocket write failed: %w", channels.ErrTemporary)
		go c.processReplyQueue(reqID)
	}
}

func (c *WeComAIBotWebSocketChannel) handleReplyAck(reqID string, frame wecomAIBotWSIncomingFrame) {
	c.replyMu.Lock()
	pending := c.pendingAcks[reqID]
	if pending == nil {
		c.replyMu.Unlock()
		return
	}
	delete(c.pendingAcks, reqID)
	pending.timer.Stop()
	if len(c.replyQueues[reqID]) > 0 {
		c.replyQueues[reqID] = c.replyQueues[reqID][1:]
	}
	if len(c.replyQueues[reqID]) == 0 {
		delete(c.replyQueues, reqID)
	}
	c.replyMu.Unlock()

	if frame.ErrCode != 0 {
		logger.WarnCF("wecom_aibot_ws", "Received websocket ack with error", map[string]any{
			"req_id":   reqID,
			"err_code": frame.ErrCode,
			"err_msg":  frame.ErrMsg,
		})
		pending.item.resultCh <- fmt.Errorf("ws ack error (%d): %s: %w", frame.ErrCode, frame.ErrMsg, channels.ErrSendFailed)
	} else {
		logger.InfoCF("wecom_aibot_ws", "Received websocket ack successfully", map[string]any{
			"req_id": reqID,
		})
		pending.item.resultCh <- nil
	}
	go c.processReplyQueue(reqID)
}

func (c *WeComAIBotWebSocketChannel) handleReplyTimeout(reqID string, seq uint64) {
	c.replyMu.Lock()
	pending := c.pendingAcks[reqID]
	if pending == nil || pending.seq != seq {
		c.replyMu.Unlock()
		return
	}
	delete(c.pendingAcks, reqID)
	if len(c.replyQueues[reqID]) > 0 {
		c.replyQueues[reqID] = c.replyQueues[reqID][1:]
	}
	if len(c.replyQueues[reqID]) == 0 {
		delete(c.replyQueues, reqID)
	}
	c.replyMu.Unlock()
	logger.WarnCF("wecom_aibot_ws", "WebSocket ack timeout", map[string]any{
		"req_id": reqID,
		"seq":    seq,
	})
	pending.item.resultCh <- fmt.Errorf("ws ack timeout for req_id=%s: %w", reqID, channels.ErrTemporary)
	go c.processReplyQueue(reqID)
}

func (c *WeComAIBotWebSocketChannel) writeFrame(frame wecomAIBotWSOutgoingFrame) error {
	c.connMu.RLock()
	conn := c.conn
	c.connMu.RUnlock()
	if conn == nil {
		return fmt.Errorf("websocket not connected: %w", channels.ErrTemporary)
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(wecomAIBotWSWriteTimeout))
	err := conn.WriteJSON(frame)
	_ = conn.SetWriteDeadline(time.Time{})
	if err != nil {
		return channels.ClassifyNetError(err)
	}
	return nil
}

func (c *WeComAIBotWebSocketChannel) handleConnectionClosed(closedConn *websocket.Conn) {
	c.connMu.Lock()
	if c.conn == closedConn {
		c.conn = nil
	}
	c.connMu.Unlock()
	_ = closedConn.Close()
	c.authenticated.Store(false)
	c.missedPongCount.Store(0)
	c.clearPendingReplies(fmt.Errorf("websocket disconnected"))

	if c.manualClose.Load() {
		return
	}
	if c.suppressReconnect.Load() {
		logger.WarnC("wecom_aibot_ws", "Reconnect suppressed after disconnected_event")
		return
	}
	authFailure := c.lastCloseAuthFailure.Swap(false)
	c.scheduleReconnect(authFailure)
}

func (c *WeComAIBotWebSocketChannel) scheduleReconnect(authFailure bool) {
	base := time.Duration(c.config.ReconnectInterval) * time.Second
	if base <= 0 {
		base = time.Second
	}
	c.reconnectMu.Lock()
	defer c.reconnectMu.Unlock()
	if c.reconnectTimer != nil {
		return
	}
	var attempt int
	var maxAttempts int
	if authFailure {
		c.authFailureAttempts++
		attempt = c.authFailureAttempts
		maxAttempts = c.config.MaxAuthFailureAttempts
		if maxAttempts <= 0 {
			maxAttempts = wecomAIBotWSDefaultAuthFails
		}
	} else {
		c.reconnectAttempts++
		attempt = c.reconnectAttempts
		maxAttempts = c.config.MaxReconnectAttempts
		if maxAttempts <= 0 {
			maxAttempts = wecomAIBotWSDefaultReconnects
		}
	}
	if maxAttempts >= 0 && attempt > maxAttempts {
		logger.ErrorCF("wecom_aibot_ws", "Reconnect attempts exhausted", map[string]any{
			"auth_failure": authFailure,
			"attempt":      attempt,
			"max_attempts": maxAttempts,
		})
		return
	}
	delay := base * time.Duration(1<<(attempt-1))
	if delay > 30*time.Second {
		delay = 30 * time.Second
	}
	logger.InfoCF("wecom_aibot_ws", "Scheduling reconnect", map[string]any{
		"auth_failure": authFailure,
		"attempt":      attempt,
		"delay":        delay.String(),
	})
	c.reconnectTimer = time.AfterFunc(delay, func() {
		c.reconnectMu.Lock()
		c.reconnectTimer = nil
		c.reconnectMu.Unlock()
		if c.manualClose.Load() || c.suppressReconnect.Load() {
			return
		}
		if err := c.connect(); err != nil {
			logger.WarnCF("wecom_aibot_ws", "Reconnect attempt failed", map[string]any{
				"error": err.Error(),
			})
			c.scheduleReconnect(false)
		}
	})
}

func (c *WeComAIBotWebSocketChannel) stopReconnectTimer() {
	c.reconnectMu.Lock()
	defer c.reconnectMu.Unlock()
	if c.reconnectTimer != nil {
		c.reconnectTimer.Stop()
		c.reconnectTimer = nil
	}
}

func (c *WeComAIBotWebSocketChannel) closeCurrentConnection() {
	c.connMu.RLock()
	conn := c.conn
	c.connMu.RUnlock()
	if conn != nil {
		_ = conn.Close()
	}
}

func (c *WeComAIBotWebSocketChannel) clearPendingReplies(reason error) {
	c.replyMu.Lock()
	defer c.replyMu.Unlock()
	for reqID, pending := range c.pendingAcks {
		pending.timer.Stop()
		pending.item.resultCh <- fmt.Errorf("%s for req_id=%s", reason.Error(), reqID)
	}
	for reqID, queue := range c.replyQueues {
		if _, ok := c.pendingAcks[reqID]; ok {
			continue
		}
		for _, item := range queue {
			item.resultCh <- fmt.Errorf("%s for req_id=%s", reason.Error(), reqID)
		}
	}
	c.pendingAcks = make(map[string]*wecomAIBotWSPendingAck)
	c.replyQueues = make(map[string][]*wecomAIBotWSReplyItem)
}

func (c *WeComAIBotWebSocketChannel) addTask(task *wecomAIBotWSTask) {
	c.taskMu.Lock()
	defer c.taskMu.Unlock()
	c.reqTasks[task.ReqID] = task
	c.chatTasks[task.ChatID] = append(c.chatTasks[task.ChatID], task)
}

func (c *WeComAIBotWebSocketChannel) peekTaskByChatID(chatID string) *wecomAIBotWSTask {
	c.taskMu.Lock()
	defer c.taskMu.Unlock()
	queue := c.chatTasks[chatID]
	for len(queue) > 0 && queue[0].Finished {
		queue = queue[1:]
	}
	c.chatTasks[chatID] = queue
	if len(queue) == 0 {
		if _, ok := c.chatTasks[chatID]; ok {
			delete(c.chatTasks, chatID)
		}
		return nil
	}
	return queue[0]
}

func (c *WeComAIBotWebSocketChannel) removeTask(task *wecomAIBotWSTask) {
	task.cancel()
	c.taskMu.Lock()
	defer c.taskMu.Unlock()
	task.Finished = true
	delete(c.reqTasks, task.ReqID)
	queue := c.chatTasks[task.ChatID]
	for i, t := range queue {
		if t == task {
			c.chatTasks[task.ChatID] = append(queue[:i], queue[i+1:]...)
			break
		}
	}
	if len(c.chatTasks[task.ChatID]) == 0 {
		delete(c.chatTasks, task.ChatID)
	}
}

func (c *WeComAIBotWebSocketChannel) clearTasks() {
	c.taskMu.Lock()
	defer c.taskMu.Unlock()
	for _, task := range c.reqTasks {
		task.cancel()
		task.Finished = true
	}
	c.reqTasks = make(map[string]*wecomAIBotWSTask)
	c.chatTasks = make(map[string][]*wecomAIBotWSTask)
}

func (c *WeComAIBotWebSocketChannel) cleanupLoop() {
	ticker := time.NewTicker(wecomAIBotWSCleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			c.cleanupExpiredTasks()
		case <-c.ctx.Done():
			return
		}
	}
}

func (c *WeComAIBotWebSocketChannel) cleanupExpiredTasks() {
	cutoff := time.Now().Add(-wecomAIBotWSTaskMaxLifetime)
	c.taskMu.Lock()
	defer c.taskMu.Unlock()
	for reqID, task := range c.reqTasks {
		if task.CreatedTime.Before(cutoff) {
			task.cancel()
			task.Finished = true
			delete(c.reqTasks, reqID)
		}
	}
	for chatID, queue := range c.chatTasks {
		filtered := queue[:0]
		for _, task := range queue {
			if task.Finished || task.CreatedTime.Before(cutoff) {
				task.cancel()
				task.Finished = true
				delete(c.reqTasks, task.ReqID)
				continue
			}
			filtered = append(filtered, task)
		}
		if len(filtered) == 0 {
			delete(c.chatTasks, chatID)
		} else {
			c.chatTasks[chatID] = filtered
		}
	}
}

func (c *WeComAIBotWebSocketChannel) isConnected() bool {
	c.connMu.RLock()
	defer c.connMu.RUnlock()
	return c.conn != nil
}

func (c *WeComAIBotWebSocketChannel) sendViaResponseURL(responseURL, content string) error {
	payload := map[string]any{
		"msgtype": "markdown",
		"markdown": map[string]string{
			"content": content,
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}
	ctx, cancel := context.WithTimeout(c.ctx, wecomAIBotWSResponseTimeout)
	defer cancel()
	if c.ctx == nil {
		ctx, cancel = context.WithTimeout(context.Background(), wecomAIBotWSResponseTimeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, responseURL, bytes.NewBuffer(body))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return channels.ClassifyNetError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	respBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return channels.ClassifyNetError(readErr)
	}
	return channels.ClassifySendError(resp.StatusCode, fmt.Errorf("response_url returned %d: %s", resp.StatusCode, respBody))
}

func (c *WeComAIBotWebSocketChannel) generateWSReqID(prefix string) string {
	letters := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 8)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(letters))))
		b[i] = letters[n.Int64()]
	}
	seq := c.reqCounter.Add(1)
	return fmt.Sprintf("%s_%d_%s", prefix, seq, string(b))
}

func (c *WeComAIBotWebSocketChannel) generateWSStreamID() string {
	return c.generateWSReqID("stream")
}

func splitTextByBytes(content string, maxBytes int) []string {
	if maxBytes <= 0 {
		return []string{content}
	}
	if len(content) == 0 {
		return []string{""}
	}
	var chunks []string
	var builder strings.Builder
	currentBytes := 0
	for _, r := range content {
		runeBytes := utf8.RuneLen(r)
		if runeBytes < 0 {
			runeBytes = len(string(r))
		}
		if currentBytes > 0 && currentBytes+runeBytes > maxBytes {
			chunks = append(chunks, builder.String())
			builder.Reset()
			currentBytes = 0
		}
		builder.WriteRune(r)
		currentBytes += runeBytes
	}
	if builder.Len() > 0 {
		chunks = append(chunks, builder.String())
	}
	if len(chunks) == 0 {
		return []string{""}
	}
	return chunks
}
