package events

import (
	"encoding/json"
	"fmt"
	"sync"
)

type Event struct {
	ID   uint64
	Name string
	Data map[string]any
}

func (e Event) Encode() []byte {
	payload, _ := json.Marshal(e.Data)
	return []byte(fmt.Sprintf("id: %d\nevent: %s\ndata: %s\n\n", e.ID, e.Name, payload))
}

type Bus struct {
	mu                     sync.Mutex
	next                   uint64
	replay                 []Event
	clients                map[chan Event]struct{}
	replaySize, clientSize int
}

func New(replaySize, clientSize int) *Bus {
	return &Bus{replaySize: replaySize, clientSize: clientSize, clients: map[chan Event]struct{}{}}
}

func (b *Bus) Publish(name string, data map[string]any) Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.next++
	event := Event{b.next, name, data}
	b.replay = append(b.replay, event)
	if len(b.replay) > b.replaySize {
		b.replay = b.replay[len(b.replay)-b.replaySize:]
	}
	for client := range b.clients {
		select {
		case client <- event:
		default:
			select {
			case <-client:
			default:
			}
			select {
			case client <- Event{event.ID, "gap", map[string]any{"reason": "slow_client"}}:
			default:
			}
		}
	}
	return event
}

func (b *Bus) Subscribe(after uint64, replayRequested bool) (<-chan Event, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	channel := make(chan Event, b.clientSize)
	if replayRequested {
		var replay []Event
		for _, event := range b.replay {
			if event.ID > after {
				replay = append(replay, event)
			}
		}
		gap := len(b.replay) > 0 && after+1 < b.replay[0].ID || len(replay) > b.clientSize
		if gap {
			channel <- Event{b.next, "gap", map[string]any{"reason": "replay_unavailable"}}
			available := b.clientSize - 1
			if available < 0 {
				available = 0
			}
			if len(replay) > available {
				replay = replay[len(replay)-available:]
			}
		}
		for _, event := range replay {
			channel <- event
		}
	}
	b.clients[channel] = struct{}{}
	return channel, func() {
		b.mu.Lock()
		if _, ok := b.clients[channel]; ok {
			delete(b.clients, channel)
			close(channel)
		}
		b.mu.Unlock()
	}
}
