package webui

import (
	"testing"
	"time"
)

func TestHub_RingBufferDropsOldest(t *testing.T) {
	h := NewHub(2)
	h.PublishEvent(Event{Msg: "a"})
	h.PublishEvent(Event{Msg: "b"})
	h.PublishEvent(Event{Msg: "c"})
	got := h.Events()
	if len(got) != 2 || got[0].Msg != "b" || got[1].Msg != "c" {
		t.Fatalf("ring buffer = %+v, want [b c]", got)
	}
}

func TestHub_SubscribeReceivesEventAndSnapshot(t *testing.T) {
	h := NewHub(8)
	ch, cancel := h.Subscribe()
	defer cancel()

	h.PublishEvent(Event{Time: "t", Level: "INFO", Msg: "hi"})
	f := <-ch
	if f.Kind != "event" || f.Event == nil || f.Event.Msg != "hi" {
		t.Fatalf("event frame = %+v", f)
	}

	h.SetSnapshot(Snapshot{Version: "test"})
	f = <-ch
	if f.Kind != "snapshot" || f.Snapshot == nil || f.Snapshot.Version != "test" {
		t.Fatalf("snapshot frame = %+v", f)
	}
}

func TestHub_CancelStopsDelivery(t *testing.T) {
	h := NewHub(8)
	ch, cancel := h.Subscribe()
	cancel()
	h.PublishEvent(Event{Msg: "x"})
	if _, ok := <-ch; ok {
		t.Fatal("channel should be closed after cancel")
	}
}

func TestHub_SlowSubscriberDoesNotBlock(t *testing.T) {
	h := NewHub(8)
	_, cancel := h.Subscribe() // never drained
	defer cancel()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			h.PublishEvent(Event{Msg: "flood"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("PublishEvent blocked on a slow subscriber")
	}
}
