package stats

import (
	"context"
	"sync"
	"time"

	"github.com/charmbracelet/log"
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

	// If this is the first subscriber, start fetching
	firstSubscriber := len(sb.subscribers) == 0
	sb.subscribers[ch] = true

	if firstSubscriber {
		log.Info("First subscriber connected, starting stats fetching")
		sb.startFetching()
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

	// If no more subscribers, stop fetching
	if len(sb.subscribers) == 0 {
		log.Info("No more subscribers, stopping stats fetching")
		sb.stopFetching()
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
	log.Info("Stats updated", "stats", newStats)
	sb.Broadcast(newStats)
}

// startFetching begins periodic stats fetching
func (sb *StatsBroadcaster) startFetching() {
	log.Info("Stats fetching stopped")

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

// Shutdown properly cleans up the broadcaster
func (sb *StatsBroadcaster) Shutdown() {
	sb.stopFetching()

	// Close all subscriber channels
	sb.subscribersMutex.Lock()
	for ch := range sb.subscribers {
		close(ch)
	}
	sb.subscribers = make(map[chan *PublicStats]bool)
	sb.subscribersMutex.Unlock()
}
