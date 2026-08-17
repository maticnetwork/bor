package p2p

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

type scriptedMsgRW struct {
	results chan scriptedResult
}

type scriptedResult struct {
	msg Msg
	err error
}

type stressMsgRW struct {
	closeOnce sync.Once
	closed    chan struct{}
	results   chan scriptedResult
}

func newStressMsgRW() *stressMsgRW {
	return &stressMsgRW{
		closed:  make(chan struct{}),
		results: make(chan scriptedResult, 128),
	}
}

func (rw *stressMsgRW) ReadMsg() (Msg, error) {
	select {
	case result := <-rw.results:
		return result.msg, result.err
	case <-rw.closed:
		return Msg{}, ErrPipeClosed
	}
}

func (rw *stressMsgRW) WriteMsg(msg Msg) error {
	select {
	case <-rw.closed:
		return ErrPipeClosed
	default:
	}
	_, err := io.Copy(io.Discard, msg.Payload)
	return err
}

func (rw *stressMsgRW) PushMsg(code uint64) {
	msg := Msg{
		Code:    code,
		Size:    1,
		Payload: bytes.NewReader([]byte{0xc0}),
	}
	select {
	case rw.results <- scriptedResult{msg: msg}:
	case <-rw.closed:
	}
}

func (rw *stressMsgRW) Close() {
	rw.closeOnce.Do(func() {
		close(rw.closed)
	})
}

func (rw *scriptedMsgRW) ReadMsg() (Msg, error) {
	result := <-rw.results
	return result.msg, result.err
}

func (rw *scriptedMsgRW) WriteMsg(msg Msg) error {
	return nil
}

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

func TestMultiChannelRoutedMsgReadWriterRoutesWritesByChannel(t *testing.T) {
	primaryApp, primaryNet := MsgPipe()
	defer primaryApp.Close()
	defer primaryNet.Close()

	controlApp, controlNet := MsgPipe()
	defer controlApp.Close()
	defer controlNet.Close()

	bulkApp, bulkNet := MsgPipe()
	defer bulkApp.Close()
	defer bulkNet.Close()

	routed, ok := NewMultiChannelRoutedMsgReadWriter(primaryNet, func(code uint64) string {
		switch code {
		case 3:
			return "eth-control"
		case 5:
			return "eth-bulk"
		default:
			return ""
		}
	}).(interface {
		AttachBulkChannel(string, MsgReadWriter)
		WriteMsg(Msg) error
	})
	if !ok {
		t.Fatal("expected multi-channel routed msg read writer")
	}
	routed.AttachBulkChannel("eth-control", controlNet)
	routed.AttachBulkChannel("eth-bulk", bulkNet)

	errc := make(chan error, 3)
	go func() { errc <- SendItems(routed, 1, uint64(11)) }()
	if err := ExpectMsg(primaryApp, 1, []uint64{11}); err != nil {
		t.Fatalf("primary lane mismatch: %v", err)
	}
	go func() { errc <- SendItems(routed, 3, uint64(33)) }()
	if err := ExpectMsg(controlApp, 3, []uint64{33}); err != nil {
		t.Fatalf("control lane mismatch: %v", err)
	}
	go func() { errc <- SendItems(routed, 5, uint64(55)) }()
	if err := ExpectMsg(bulkApp, 5, []uint64{55}); err != nil {
		t.Fatalf("bulk lane mismatch: %v", err)
	}
	for range 3 {
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

func TestMultiChannelRoutedMsgReadWriterFallsBackPerChannel(t *testing.T) {
	primaryApp, primaryNet := MsgPipe()
	defer primaryApp.Close()
	defer primaryNet.Close()

	controlApp, controlNet := MsgPipe()
	defer controlNet.Close()

	routed, ok := NewMultiChannelRoutedMsgReadWriter(primaryNet, func(code uint64) string {
		if code == 3 {
			return "eth-control"
		}
		return ""
	}).(interface {
		AttachBulkChannel(string, MsgReadWriter)
		WriteMsg(Msg) error
	})
	if !ok {
		t.Fatal("expected multi-channel routed msg read writer")
	}
	routed.AttachBulkChannel("eth-control", controlNet)

	if err := controlApp.Close(); err != nil {
		t.Fatalf("failed to close control lane: %v", err)
	}

	errc := make(chan error, 1)
	go func() { errc <- SendItems(routed, 3, uint64(33)) }()
	if err := ExpectMsg(primaryApp, 3, []uint64{33}); err != nil {
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

func TestRoutedMsgReadWriterIgnoresBulkReadTimeouts(t *testing.T) {
	primaryApp, primaryNet := MsgPipe()
	defer primaryApp.Close()
	defer primaryNet.Close()

	bulk := &scriptedMsgRW{
		results: make(chan scriptedResult, 2),
	}
	routed, ok := NewRoutedMsgReadWriter(primaryNet, bulk, func(code uint64) bool { return code == 2 }).(interface {
		HasBulk() bool
		ReadMsg() (Msg, error)
	})
	if !ok {
		t.Fatal("expected attachable routed msg read writer")
	}

	bulk.results <- scriptedResult{err: timeoutErr{}}
	bulk.results <- scriptedResult{
		msg: Msg{
			Code:    2,
			Size:    1,
			Payload: io.NopCloser(bytes.NewReader([]byte{0xc0})),
		},
	}

	msg, err := routed.ReadMsg()
	if err != nil {
		t.Fatalf("unexpected read failure after timeout: %v", err)
	}
	if msg.Code != 2 {
		t.Fatalf("unexpected message code after timeout recovery: got %d want 2", msg.Code)
	}
	if err := msg.Discard(); err != nil {
		t.Fatalf("failed to discard recovered bulk message: %v", err)
	}
	if !routed.HasBulk() {
		t.Fatal("expected bulk lane to remain attached after timeout")
	}
}

func TestMultiChannelRoutedMsgReadWriterConcurrentAttachAndTraffic(t *testing.T) {
	primary := newStressMsgRW()
	routed, ok := NewMultiChannelRoutedMsgReadWriter(primary, func(code uint64) string {
		switch code % 3 {
		case 1:
			return "control"
		case 2:
			return "bulk"
		default:
			return ""
		}
	}).(interface {
		AttachBulkChannel(string, MsgReadWriter)
		ReadMsg() (Msg, error)
		WriteMsg(Msg) error
	})
	if !ok {
		t.Fatal("expected multi-channel routed msg read writer")
	}

	control := newStressMsgRW()
	bulk := newStressMsgRW()
	routed.AttachBulkChannel("control", control)
	routed.AttachBulkChannel("bulk", bulk)

	readErrc := make(chan error, 1)
	var readWG sync.WaitGroup
	readWG.Add(1)
	go func() {
		defer readWG.Done()
		for {
			msg, err := routed.ReadMsg()
			if err != nil {
				if !errors.Is(err, ErrPipeClosed) {
					readErrc <- err
				}
				return
			}
			if err := msg.Discard(); err != nil {
				readErrc <- err
				return
			}
		}
	}()

	errc := make(chan error, 8)
	var writeWG sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		writeWG.Add(1)
		go func(offset uint64) {
			defer writeWG.Done()
			for i := uint64(0); i < 250; i++ {
				if err := SendItems(routed, (i+offset)%3, i); err != nil {
					errc <- err
					return
				}
			}
		}(uint64(worker))
	}

	var attachWG sync.WaitGroup
	attachWG.Add(1)
	go func() {
		defer attachWG.Done()

		currentControl := control
		currentBulk := bulk
		for i := 0; i < 200; i++ {
			nextControl := newStressMsgRW()
			nextBulk := newStressMsgRW()

			routed.AttachBulkChannel("control", nextControl)
			routed.AttachBulkChannel("bulk", nextBulk)

			primary.PushMsg(90)
			nextControl.PushMsg(91)
			nextBulk.PushMsg(92)

			currentControl.Close()
			currentBulk.Close()
			currentControl = nextControl
			currentBulk = nextBulk
		}
		currentControl.Close()
		currentBulk.Close()
	}()

	writeWG.Wait()
	attachWG.Wait()
	primary.Close()
	readWG.Wait()

	select {
	case err := <-readErrc:
		t.Fatalf("read failed during concurrent attach stress: %v", err)
	default:
	}
	select {
	case err := <-errc:
		t.Fatalf("write failed during concurrent attach stress: %v", err)
	default:
	}
}
