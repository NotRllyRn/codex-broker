package events

import "testing"

func TestReplayIsBoundedAndReportsGap(t *testing.T) {
	bus := New(10, 3)
	for index := 0; index < 10; index++ {
		bus.Publish("update", map[string]any{"index": index})
	}
	stream, cancel := bus.Subscribe(1, true)
	defer cancel()
	first := <-stream
	if first.Name != "gap" || first.Data["reason"] != "replay_unavailable" {
		t.Fatalf("first event = %#v", first)
	}
	if (<-stream).ID != 9 || (<-stream).ID != 10 {
		t.Fatal("replay did not retain the newest events")
	}
}

func TestSlowSubscriberReceivesGap(t *testing.T) {
	bus := New(10, 1)
	stream, cancel := bus.Subscribe(0, false)
	defer cancel()
	bus.Publish("first", nil)
	bus.Publish("second", nil)
	event := <-stream
	if event.Name != "gap" || event.Data["reason"] != "slow_client" {
		t.Fatalf("overflow event = %#v", event)
	}
}
