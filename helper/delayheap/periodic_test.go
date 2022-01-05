package delayheap

import (
	"testing"
	"time"

	"gotest.tools/assert"
)

type wrapper struct {
	ch chan HeapNode
}

func (w *wrapper) Enqueue(h HeapNode) {
	w.ch <- h
}

func TestPeriodicOutOfOrder(t *testing.T) {
	ch := make(chan HeapNode)
	p := NewPeriodicDispatcher(&wrapper{ch})
	p.Run()

	p.Add(StringNode("1"), time.Now().Add(2*time.Second))
	p.Add(StringNode("2"), time.Now().Add(1*time.Second))

	item := <-ch
	assert.Equal(t, item.ID(), "2")

	item = <-ch
	assert.Equal(t, item.ID(), "1")
}

func TestPeriodicIdleUntilFirst(t *testing.T) {
	ch := make(chan HeapNode)
	p := NewPeriodicDispatcher(&wrapper{ch})
	p.Run()

	// idle until first item arrives
	select {
	case <-ch:
		t.Fatal("unexpected item")
	case <-time.After(2 * time.Second):
	}

	p.Add(StringNode("1"), time.Now().Add(1*time.Second))

	item := <-ch
	assert.Equal(t, item.ID(), "1")
}
