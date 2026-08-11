package p2p

import (
	"testing"
	"time"
)

func TestRoutedMsgReadWriterRoutesWrites(t *testing.T) {
	primaryApp, primaryNet := MsgPipe()
	defer primaryApp.Close()
	defer primaryNet.Close()

	bulkApp, bulkNet := MsgPipe()
	defer bulkApp.Close()
	defer bulkNet.Close()

	rw := NewRoutedMsgReadWriter(primaryNet, bulkNet, func(code uint64) bool { return code == 2 })

	errc := make(chan error, 2)
	go func() { errc <- SendItems(rw, 1, uint64(11)) }()
	if err := ExpectMsg(primaryApp, 1, []uint64{11}); err != nil {
		t.Fatalf("primary lane mismatch: %v", err)
	}
	go func() { errc <- SendItems(rw, 2, uint64(22)) }()
	if err := ExpectMsg(bulkApp, 2, []uint64{22}); err != nil {
		t.Fatalf("bulk lane mismatch: %v", err)
	}
	for range 2 {
		if err := <-errc; err != nil {
			t.Fatalf("send failed: %v", err)
		}
	}
}

func TestRoutedMsgReadWriterReadsBothLanes(t *testing.T) {
	primaryApp, primaryNet := MsgPipe()
	defer primaryApp.Close()
	defer primaryNet.Close()

	bulkApp, bulkNet := MsgPipe()
	defer bulkApp.Close()
	defer bulkNet.Close()

	rw := NewRoutedMsgReadWriter(primaryNet, bulkNet, func(code uint64) bool { return code == 2 })

	errc := make(chan error, 2)
	go func() { errc <- SendItems(bulkApp, 2, uint64(22)) }()
	go func() { errc <- SendItems(primaryApp, 1, uint64(11)) }()

	got := map[uint64]uint64{}
	for range 2 {
		msg, err := rw.ReadMsg()
		if err != nil {
			t.Fatalf("failed to read routed message: %v", err)
		}
		var payload []uint64
		if err := msg.Decode(&payload); err != nil {
			t.Fatalf("failed to decode payload: %v", err)
		}
		if len(payload) != 1 {
			t.Fatalf("unexpected payload length %d", len(payload))
		}
		got[msg.Code] = payload[0]
	}

	if got[1] != 11 {
		t.Fatalf("primary payload mismatch: got %d want 11", got[1])
	}
	if got[2] != 22 {
		t.Fatalf("bulk payload mismatch: got %d want 22", got[2])
	}
	for range 2 {
		if err := <-errc; err != nil {
			t.Fatalf("send failed: %v", err)
		}
	}
}

func TestRoutedMsgReadWriterIgnoresBulkReadErrors(t *testing.T) {
	primaryApp, primaryNet := MsgPipe()
	defer primaryApp.Close()
	defer primaryNet.Close()

	bulkApp, bulkNet := MsgPipe()
	defer bulkApp.Close()
	defer bulkNet.Close()

	rw := NewRoutedMsgReadWriter(primaryNet, bulkNet, func(code uint64) bool { return true })

	if err := bulkApp.Close(); err != nil {
		t.Fatalf("failed to close bulk lane: %v", err)
	}

	errc := make(chan error, 1)
	go func() { errc <- SendItems(primaryApp, 1, uint64(11)) }()

	msg, err := rw.ReadMsg()
	if err != nil {
		t.Fatalf("unexpected read failure after bulk close: %v", err)
	}
	var payload []uint64
	if err := msg.Decode(&payload); err != nil {
		t.Fatalf("failed to decode payload: %v", err)
	}
	if msg.Code != 1 || len(payload) != 1 || payload[0] != 11 {
		t.Fatalf("unexpected primary payload after bulk close: code=%d payload=%v", msg.Code, payload)
	}
	if err := <-errc; err != nil {
		t.Fatalf("send failed: %v", err)
	}
}

func TestRoutedMsgReadWriterAttachesBulkLate(t *testing.T) {
	primaryApp, primaryNet := MsgPipe()
	defer primaryApp.Close()
	defer primaryNet.Close()

	routed, ok := NewRoutedMsgReadWriter(primaryNet, nil, func(code uint64) bool { return code == 2 }).(interface {
		AttachBulk(MsgReadWriter)
		ReadMsg() (Msg, error)
		WriteMsg(Msg) error
	})
	if !ok {
		t.Fatal("expected attachable routed msg read writer")
	}

	bulkApp, bulkNet := MsgPipe()
	defer bulkApp.Close()
	defer bulkNet.Close()

	routed.AttachBulk(bulkNet)
	errc := make(chan error, 1)
	go func() { errc <- SendItems(routed, 2, uint64(22)) }()
	if err := ExpectMsg(bulkApp, 2, []uint64{22}); err != nil {
		t.Fatalf("bulk lane mismatch after late attach: %v", err)
	}
	if err := <-errc; err != nil {
		t.Fatalf("bulk send failed: %v", err)
	}
}

func TestRoutedMsgReadWriterFallsBackToPrimaryWhenBulkWriteFails(t *testing.T) {
	primaryApp, primaryNet := MsgPipe()
	defer primaryApp.Close()
	defer primaryNet.Close()

	bulkApp, bulkNet := MsgPipe()
	defer bulkNet.Close()

	rw := NewRoutedMsgReadWriter(primaryNet, bulkNet, func(code uint64) bool { return code == 2 })

	if err := bulkApp.Close(); err != nil {
		t.Fatalf("failed to close bulk lane: %v", err)
	}

	errc := make(chan error, 1)
	go func() { errc <- SendItems(rw, 2, uint64(22)) }()
	if err := ExpectMsg(primaryApp, 2, []uint64{22}); err != nil {
		t.Fatalf("primary fallback mismatch: %v", err)
	}
	if err := <-errc; err != nil {
		t.Fatalf("send failed after fallback: %v", err)
	}
}

func TestRoutedMsgReadWriterRestartsBulkReadsAfterReattach(t *testing.T) {
	primaryApp, primaryNet := MsgPipe()
	defer primaryApp.Close()
	defer primaryNet.Close()

	bulkApp1, bulkNet1 := MsgPipe()
	routed, ok := NewRoutedMsgReadWriter(primaryNet, bulkNet1, func(code uint64) bool { return code == 2 }).(interface {
		AttachBulk(MsgReadWriter)
		ReadMsg() (Msg, error)
	})
	if !ok {
		t.Fatal("expected attachable routed msg read writer")
	}

	if err := bulkApp1.Close(); err != nil {
		t.Fatalf("failed to close initial bulk lane: %v", err)
	}
	// Allow the first bulk read loop to observe the closed lane and exit.
	time.Sleep(10 * time.Millisecond)
	if hasBulk, ok := routed.(interface{ HasBulk() bool }); !ok || hasBulk.HasBulk() {
		t.Fatal("expected closed bulk lane to be cleared")
	}

	bulkApp2, bulkNet2 := MsgPipe()
	defer bulkApp2.Close()
	defer bulkNet2.Close()

	routed.AttachBulk(bulkNet2)

	errc := make(chan error, 1)
	go func() { errc <- SendItems(bulkApp2, 2, uint64(22)) }()

	msg, err := routed.ReadMsg()
	if err != nil {
		t.Fatalf("failed to read reattached bulk message: %v", err)
	}
	var payload []uint64
	if err := msg.Decode(&payload); err != nil {
		t.Fatalf("failed to decode reattached bulk payload: %v", err)
	}
	if msg.Code != 2 || len(payload) != 1 || payload[0] != 22 {
		t.Fatalf("unexpected reattached bulk payload: code=%d payload=%v", msg.Code, payload)
	}
	if err := <-errc; err != nil {
		t.Fatalf("send failed on reattached bulk lane: %v", err)
	}
}
