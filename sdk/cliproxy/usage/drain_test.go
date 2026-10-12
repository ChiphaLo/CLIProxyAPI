package usage

import (
	"context"
	"testing"
)

type drainPlugin struct{ ids []string }

func (p *drainPlugin) HandleUsage(_ context.Context, record Record) {
	p.ids = append(p.ids, record.RequestID)
}

func TestStopAndWaitDrainsReceipts(t *testing.T) {
	manager := NewManager(1)
	plugin := &drainPlugin{}
	manager.Register(plugin)
	for _, id := range []string{"first", "second", "third"} {
		manager.Publish(context.Background(), Record{RequestID: id})
	}
	if err := manager.StopAndWait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(plugin.ids) != 3 || plugin.ids[2] != "third" {
		t.Fatal("queued records lost on shutdown")
	}
	if err := manager.StopAndWait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestStopAndWaitBeforeStart(t *testing.T) {
	manager := NewManager(1)
	if err := manager.StopAndWait(context.Background()); err != nil {
		t.Fatal(err)
	}
}
