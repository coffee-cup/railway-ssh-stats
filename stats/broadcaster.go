package stats

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/log"
	"github.com/gorilla/websocket"
)

// StatsBroadcaster manages fetching stats and broadcasting them to subscribers
type StatsBroadcaster struct {
	subscribers      map[chan *PublicStats]bool
	subscribersMutex sync.Mutex
	latestStats      *PublicStats
	statsMutex       sync.RWMutex
	fetchingActive   bool
	fetchingMutex    sync.Mutex
	fetchTicker      *time.Ticker
	fetchDone        chan struct{}
	interval         time.Duration
	ctx              context.Context
	cancel           context.CancelFunc
	wsConn           *websocket.Conn
	wsConnMutex      sync.Mutex
}

// NewStatsBroadcaster creates a new stats broadcaster
func NewStatsBroadcaster(interval time.Duration) *StatsBroadcaster {
	ctx, cancel := context.WithCancel(context.Background())
	return &StatsBroadcaster{
		subscribers: make(map[chan *PublicStats]bool),
		interval:    interval,
		ctx:         ctx,
		cancel:      cancel,
		fetchDone:   make(chan struct{}),
	}
}

// Subscribe creates a new subscription channel and returns it
func (sb *StatsBroadcaster) Subscribe() chan *PublicStats {
	sb.subscribersMutex.Lock()
	defer sb.subscribersMutex.Unlock()

	ch := make(chan *PublicStats, 1)

	// If this is the first subscriber, start fetching and websocket
	firstSubscriber := len(sb.subscribers) == 0
	sb.subscribers[ch] = true

	if firstSubscriber {
		log.Info("First subscriber connected, starting stats fetching and websocket")
		sb.startFetching()
		go sb.startWebSocketSubscription()
	}

	// Send the latest stats immediately if available
	sb.statsMutex.RLock()
	if sb.latestStats != nil {
		ch <- sb.latestStats
	} else if firstSubscriber {
		// If this is the first subscriber and we don't have stats yet,
		// fetch them immediately
		sb.statsMutex.RUnlock()
		go sb.fetchAndBroadcast()
		return ch
	}
	sb.statsMutex.RUnlock()

	return ch
}

// Unsubscribe removes a subscription and closes the channel
func (sb *StatsBroadcaster) Unsubscribe(ch chan *PublicStats) {
	sb.subscribersMutex.Lock()
	defer sb.subscribersMutex.Unlock()

	delete(sb.subscribers, ch)
	close(ch)

	// If no more subscribers, stop fetching and websocket
	if len(sb.subscribers) == 0 {
		log.Info("No more subscribers, stopping stats fetching and websocket")
		sb.stopFetching()
		sb.stopWebSocketSubscription()
	}
}

// Broadcast sends stats to all subscribers
func (sb *StatsBroadcaster) Broadcast(stats *PublicStats) {
	sb.statsMutex.Lock()
	sb.latestStats = stats
	sb.statsMutex.Unlock()

	sb.subscribersMutex.Lock()
	defer sb.subscribersMutex.Unlock()

	// If no subscribers, don't bother broadcasting
	if len(sb.subscribers) == 0 {
		return
	}

	for ch := range sb.subscribers {
		// Non-blocking send to prevent slow subscribers from blocking the broadcast
		select {
		case ch <- stats:
		default:
			// Channel buffer is full, replace the old value with the new one
			select {
			case <-ch:
				ch <- stats
			default:
				ch <- stats
			}
		}
	}
}

// fetchAndBroadcast fetches the latest stats and broadcasts them
func (sb *StatsBroadcaster) fetchAndBroadcast() {
	newStats, err := GetPublicStats()
	if err != nil {
		log.Error("Could not get stats", "error", err)
		return
	}
	// log.Info("Stats updated via polling", "stats", newStats)
	sb.Broadcast(newStats)
}

// startFetching begins periodic stats fetching
func (sb *StatsBroadcaster) startFetching() {
	sb.fetchingMutex.Lock()
	defer sb.fetchingMutex.Unlock()

	if sb.fetchingActive {
		return
	}

	sb.fetchingActive = true
	sb.fetchTicker = time.NewTicker(sb.interval)

	// Start the fetching goroutine
	go func() {
		defer func() {
			sb.fetchingMutex.Lock()
			sb.fetchingActive = false
			sb.fetchingMutex.Unlock()
			close(sb.fetchDone)
		}()

		// Fetch initial stats
		sb.fetchAndBroadcast()

		for {
			select {
			case <-sb.fetchTicker.C:
				sb.fetchAndBroadcast()
			case <-sb.ctx.Done():
				return
			}
		}
	}()
}

// stopFetching stops the periodic fetching
func (sb *StatsBroadcaster) stopFetching() {
	log.Info("Stats fetching stopped")

	sb.fetchingMutex.Lock()
	defer sb.fetchingMutex.Unlock()

	if !sb.fetchingActive {
		return
	}

	if sb.fetchTicker != nil {
		sb.fetchTicker.Stop()
	}

	sb.cancel()

	// Create a new context for future fetching
	sb.ctx, sb.cancel = context.WithCancel(context.Background())

	// Create a new done channel
	sb.fetchDone = make(chan struct{})
}

// GraphQL subscription related types
type GraphQLWSMessage struct {
	ID      string          `json:"id,omitempty"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type GraphQLWSPayload struct {
	Query     string                 `json:"query"`
	Variables map[string]interface{} `json:"variables,omitempty"`
}

type GraphQLWSResponse struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

type GraphQLWSData struct {
	Data struct {
		PublicStats struct {
			Key   string `json:"key"`
			Value int    `json:"value"`
			Type  string `json:"type"`
		} `json:"publicStats"`
	} `json:"data"`
}

// GraphQLError represents a single GraphQL error
type GraphQLError struct {
	Message   string `json:"message"`
	Locations []struct {
		Line   int `json:"line"`
		Column int `json:"column"`
	} `json:"locations,omitempty"`
}

// startWebSocketSubscription starts a GraphQL subscription over WebSocket
func (sb *StatsBroadcaster) startWebSocketSubscription() {
	sb.wsConnMutex.Lock()
	defer sb.wsConnMutex.Unlock()

	if sb.wsConn != nil {
		return
	}

	log.Info("Starting WebSocket subscription")

	// Connect to the GraphQL WebSocket endpoint
	dialer := websocket.Dialer{
		// Based on the network tab, we need to use "graphql-transport-ws" protocol
		Subprotocols: []string{"graphql-transport-ws"},
		// Add a longer handshake timeout
		HandshakeTimeout: 20 * time.Second,
	}

	// Add headers that might be required
	log.Info("Connecting to WebSocket", "url", "wss://backboard.railway.app/graphql/internal", "subprotocols", dialer.Subprotocols)
	conn, resp, err := dialer.Dial("wss://backboard.railway.app/graphql/internal", nil)
	if err != nil {
		log.Error("Failed to connect to GraphQL WebSocket", "error", err)
		if resp != nil {
			log.Error("WebSocket response", "status", resp.Status, "headers", resp.Header)
		}
		return
	}

	log.Info("WebSocket connected successfully")
	sb.wsConn = conn

	// Set read deadline to detect timeouts
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))

	// Send connection init message
	initMsg := GraphQLWSMessage{
		Type:    "connection_init",
		Payload: json.RawMessage(`{}`), // Add empty payload object
	}
	log.Info("Sending connection init message", "message", initMsg)
	if err := conn.WriteJSON(initMsg); err != nil {
		log.Error("Failed to send connection init message", "error", err)
		conn.Close()
		sb.wsConn = nil
		return
	}

	// Wait for connection ack
	var ackMsg GraphQLWSMessage
	log.Info("Waiting for connection ack")
	if err := conn.ReadJSON(&ackMsg); err != nil {
		log.Error("Failed to receive connection ack", "error", err)
		conn.Close()
		sb.wsConn = nil
		return
	}

	log.Info("Received message", "type", ackMsg.Type)
	if ackMsg.Type != "connection_ack" {
		log.Error("Expected connection_ack but got", "type", ackMsg.Type)
		conn.Close()
		sb.wsConn = nil
		return
	}

	// Reset read deadline after successful connection
	conn.SetReadDeadline(time.Time{})

	// Generate a random ID like in the network tab
	subscriptionID := fmt.Sprintf("%x-%x-%x-%x-%x",
		time.Now().UnixNano()&0xffffffff,
		time.Now().UnixNano()&0xffff,
		time.Now().UnixNano()&0xffff,
		time.Now().UnixNano()&0xffff,
		time.Now().UnixNano()&0xffffffffffff)

	// Create the payload according to the graphql-transport-ws protocol
	payload := map[string]interface{}{
		"query": `
subscription publicStatsSubscription {
  publicStats {
    key
    value
    type
  }
}`,
		"variables": map[string]interface{}{},
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		log.Error("Failed to marshal subscription payload", "error", err)
		conn.Close()
		sb.wsConn = nil
		return
	}

	subMsg := GraphQLWSMessage{
		ID:      subscriptionID,
		Type:    "subscribe",
		Payload: payloadBytes,
	}

	// Log the exact message we're sending
	subMsgBytes, _ := json.MarshalIndent(subMsg, "", "  ")
	log.Info("Sending subscription message", "id", subscriptionID, "message", string(subMsgBytes))

	if err := conn.WriteJSON(subMsg); err != nil {
		log.Error("Failed to send subscription message", "error", err)
		conn.Close()
		sb.wsConn = nil
		return
	}

	// Start listening for subscription updates
	go sb.handleWebSocketMessages(conn, subscriptionID)
}

// handleWebSocketMessages processes incoming WebSocket messages
func (sb *StatsBroadcaster) handleWebSocketMessages(conn *websocket.Conn, subscriptionID string) {
	defer func() {
		log.Info("WebSocket handler exiting, closing connection")
		sb.wsConnMutex.Lock()
		if sb.wsConn == conn {
			sb.wsConn = nil
		}
		sb.wsConnMutex.Unlock()
		conn.Close()
	}()

	log.Info("Started listening for WebSocket messages")

	// Set up ping handler to respond to pings
	conn.SetPingHandler(func(data string) error {
		log.Info("Received ping, sending pong")
		return conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(10*time.Second))
	})

	// Set up pong handler to reset read deadline
	conn.SetPongHandler(func(string) error {
		log.Info("Received pong")
		return nil
	})

	// Set a longer read deadline
	conn.SetReadDeadline(time.Now().Add(60 * time.Second))

	// Send a ping every 30 seconds to keep the connection alive
	pingTicker := time.NewTicker(30 * time.Second)
	defer pingTicker.Stop()

	// Create a channel to signal when to exit
	done := make(chan struct{})
	defer close(done)

	// Start a goroutine to send pings
	go func() {
		for {
			select {
			case <-pingTicker.C:
				log.Info("Sending ping to keep connection alive")
				if err := conn.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(10*time.Second)); err != nil {
					log.Error("Failed to send ping", "error", err)
					return
				}
			case <-done:
				return
			}
		}
	}()

	for {
		// Reset read deadline for each message
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))

		// Read the raw message first
		messageType, rawMessage, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Info("WebSocket closed normally")
			} else if websocket.IsUnexpectedCloseError(err) {
				log.Error("WebSocket closed unexpectedly", "error", err)
			} else if strings.Contains(err.Error(), "deadline exceeded") {
				log.Error("WebSocket read timeout", "error", err)
			} else {
				log.Error("Error reading from WebSocket", "error", err)
			}
			return
		}

		// Log message type
		var msgTypeStr string
		switch messageType {
		case websocket.TextMessage:
			msgTypeStr = "text"
		case websocket.BinaryMessage:
			msgTypeStr = "binary"
		case websocket.CloseMessage:
			msgTypeStr = "close"
			log.Info("Received close message")
			return
		case websocket.PingMessage:
			msgTypeStr = "ping"
		case websocket.PongMessage:
			msgTypeStr = "pong"
		default:
			msgTypeStr = fmt.Sprintf("unknown(%d)", messageType)
		}

		// Log the raw message as a string
		rawMessageStr := string(rawMessage)
		log.Info("Received WebSocket message", "type", msgTypeStr, "message", rawMessageStr)

		// Parse the message
		var msg GraphQLWSResponse
		if err := json.Unmarshal(rawMessage, &msg); err != nil {
			log.Error("Failed to unmarshal WebSocket message", "error", err, "raw", rawMessageStr)
			continue
		}

		log.Info("Parsed WebSocket message", "type", msg.Type, "id", msg.ID)

		// Handle different message types
		switch msg.Type {
		case "next":
			// Try to parse the data structure
			var data GraphQLWSData
			if err := json.Unmarshal(msg.Payload, &data); err != nil {
				log.Error("Failed to unmarshal subscription data", "error", err, "payload", string(msg.Payload))
				continue
			}

			if data.Data.PublicStats.Key != "" {
				log.Info("Received stats update",
					"key", data.Data.PublicStats.Key,
					"value", data.Data.PublicStats.Value,
					"type", data.Data.PublicStats.Type)

				// Update the stats with the received delta
				sb.updateStatsWithDelta(
					data.Data.PublicStats.Key,
					data.Data.PublicStats.Value,
				)
			} else {
				log.Warn("Received empty or unexpected data structure", "payload", string(msg.Payload))
			}
		case "error":
			// Log the error payload - errors come as an array
			var errors []GraphQLError
			if err := json.Unmarshal(msg.Payload, &errors); err != nil {
				log.Error("Failed to unmarshal error payload", "error", err, "payload", string(msg.Payload))
			} else {
				for _, err := range errors {
					log.Error("GraphQL subscription error", "message", err.Message, "locations", err.Locations)
				}
			}
			// Don't return on error, just log it
			log.Warn("Continuing despite GraphQL error")
		case "complete":
			log.Info("Subscription completed", "id", msg.ID)
			return
		case "ping":
			// Respond to ping with a pong
			pongMsg := GraphQLWSMessage{
				Type: "pong",
			}
			if err := conn.WriteJSON(pongMsg); err != nil {
				log.Error("Failed to send pong message", "error", err)
				return
			}
			log.Info("Responded to ping with pong")
		case "pong":
			// Just log this, no action needed
			log.Info("Received pong message")
		case "connection_keep_alive":
			// Just log this, no action needed
			log.Info("Received keep-alive message")
		default:
			log.Info("Received unhandled message type", "type", msg.Type)
		}
	}
}

// updateStatsWithDelta updates a specific stat and broadcasts the update
func (sb *StatsBroadcaster) updateStatsWithDelta(key string, value int) {
	sb.statsMutex.Lock()
	defer sb.statsMutex.Unlock()

	// If we don't have stats yet, don't try to update
	if sb.latestStats == nil {
		log.Warn("Received realtime update but no initial stats available yet")
		return
	}

	// Update the specific field based on the key
	updated := false
	switch key {
	case "totalUsers":
		sb.latestStats.TotalUsers = value
		updated = true
	case "totalProjects":
		sb.latestStats.TotalProjects = value
		updated = true
	case "totalServices":
		sb.latestStats.TotalServices = value
		updated = true
	case "totalDeploymentsLastMonth":
		sb.latestStats.TotalDeploymentsLastMonth = value
		updated = true
	case "totalLogsLastMonth":
		sb.latestStats.TotalLogsLastMonth = value
		updated = true
	case "totalRequestsLastMonth":
		sb.latestStats.TotalRequestsLastMonth = value
		updated = true
	default:
		log.Warn("Received update for unknown stat key", "key", key)
	}

	if updated {
		log.Info(fmt.Sprintf("Realtime update: %s = %d", key, value))
		// Create a copy to broadcast
		statsCopy := *sb.latestStats
		go sb.Broadcast(&statsCopy)
	}
}

// stopWebSocketSubscription closes the WebSocket connection
func (sb *StatsBroadcaster) stopWebSocketSubscription() {
	sb.wsConnMutex.Lock()
	defer sb.wsConnMutex.Unlock()

	if sb.wsConn != nil {
		log.Info("Stopping WebSocket subscription")

		// Based on the protocol, we need to use "complete" instead of "stop"
		stopMsg := GraphQLWSMessage{
			ID:   "1", // Same ID as used for subscription
			Type: "complete",
		}
		sb.wsConn.WriteJSON(stopMsg)

		// Close the connection
		sb.wsConn.Close()
		sb.wsConn = nil
		log.Info("WebSocket subscription stopped")
	}
}

// Shutdown properly cleans up the broadcaster
func (sb *StatsBroadcaster) Shutdown() {
	sb.stopFetching()
	sb.stopWebSocketSubscription()

	// Close all subscriber channels
	sb.subscribersMutex.Lock()
	for ch := range sb.subscribers {
		close(ch)
	}
	sb.subscribers = make(map[chan *PublicStats]bool)
	sb.subscribersMutex.Unlock()
}
