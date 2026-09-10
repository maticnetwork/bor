// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package filters

import "testing"

// TestClientNotifierDropsSubscriptionOnOverflow asserts the overflow policy: a
// client that falls further than clientNotificationBuffer behind has its
// subscription dropped and counted, rather than being served a stream with a
// silent gap it cannot detect.
func TestClientNotifierDropsSubscriptionOnOverflow(t *testing.T) {
	// Constructed directly rather than via notifyAsync: with no drain goroutine
	// nothing leaves the queue, so overflow is reached deterministically.
	c := &clientNotifier{
		id:     "test-subscription",
		queue:  make(chan any, clientNotificationBuffer),
		failed: make(chan struct{}),
	}

	before := subscriptionsDroppedCounter.Snapshot().Count()

	for i := 0; i < clientNotificationBuffer; i++ {
		c.send(i)

		select {
		case <-c.failed:
			t.Fatalf("subscription dropped after %d of %d buffered notifications", i+1, clientNotificationBuffer)
		default:
		}
	}

	c.send("overflow")

	select {
	case <-c.failed:
	default:
		t.Fatal("subscription not dropped after the client fell past the buffer")
	}

	// Later sends must not re-close the channel or re-count the drop.
	c.send("after overflow")

	if dropped := subscriptionsDroppedCounter.Snapshot().Count() - before; dropped != 1 {
		t.Fatalf("counted %d drops, want 1", dropped)
	}
}
