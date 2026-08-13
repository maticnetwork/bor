// Copyright 2026 The go-ethereum Authors
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

package p2p

import (
	"bytes"
	"errors"
	"io"
	"net"
	"sync"
)

// NewRoutedMsgReadWriter multiplexes reads from the primary and bulk lanes while
// routing writes for selected message codes over the bulk lane.
//
// If bulk is nil or shouldRoute is nil, the primary lane is returned unchanged.
func NewRoutedMsgReadWriter(primary MsgReadWriter, bulk MsgReadWriter, shouldRoute func(code uint64) bool) MsgReadWriter {
	return newRoutedMsgReadWriter(primary, bulk, "", shouldRoute)
}

// NewChannelRoutedMsgReadWriter attaches a metrics label to routed bulk-lane
// traffic so fallbacks and read failures can be surfaced per channel.
func NewChannelRoutedMsgReadWriter(primary MsgReadWriter, bulk MsgReadWriter, channel string, shouldRoute func(code uint64) bool) MsgReadWriter {
	return newRoutedMsgReadWriter(primary, bulk, channel, shouldRoute)
}

func newRoutedMsgReadWriter(primary MsgReadWriter, bulk MsgReadWriter, channel string, shouldRoute func(code uint64) bool) MsgReadWriter {
	if shouldRoute == nil {
		return primary
	}
	rw := &routedMsgReadWriter{
		primary:     primary,
		channel:     channel,
		shouldRoute: shouldRoute,
		reads:       make(chan routedReadResult, 2),
	}
	if bulk != nil {
		rw.AttachBulk(bulk)
	}
	return rw
}

type routedReadResult struct {
	msg Msg
	err error
}

type routedMsgReadWriter struct {
	primary     MsgReadWriter
	channel     string
	shouldRoute func(code uint64) bool

	start      sync.Once
	reads      chan routedReadResult
	bulkMu     sync.RWMutex
	bulkReader *routedBulkLane
	bulkSeq    uint64
}

type routedBulkLane struct {
	id uint64
	rw MsgReadWriter
}

func (rw *routedMsgReadWriter) ReadMsg() (Msg, error) {
	rw.start.Do(func() {
		go rw.readLoop(rw.primary, true, 0)
	})

	result := <-rw.reads
	return result.msg, result.err
}

func (rw *routedMsgReadWriter) WriteMsg(msg Msg) error {
	if rw.shouldRoute(msg.Code) {
		if bulk, ok := rw.bulk(); ok {
			if err := bulk.WriteMsg(msg); err == nil {
				return nil
			} else {
				bulkSidecarWriteFallbackMeter.Mark(1)
				bulkSidecarStats.markChannelWriteFallback(rw.channel)
			}
		}
	}
	return rw.primary.WriteMsg(msg)
}

func (rw *routedMsgReadWriter) AttachBulk(bulk MsgReadWriter) {
	if bulk == nil {
		return
	}
	lane := rw.setBulk(bulk)
	go rw.readLoop(lane.rw, false, lane.id)
}

func (rw *routedMsgReadWriter) bulk() (MsgReadWriter, bool) {
	rw.bulkMu.RLock()
	defer rw.bulkMu.RUnlock()
	if rw.bulkReader == nil {
		return nil, false
	}
	return rw.bulkReader.rw, true
}

func (rw *routedMsgReadWriter) HasBulk() bool {
	_, ok := rw.bulk()
	return ok
}

func (rw *routedMsgReadWriter) setBulk(bulk MsgReadWriter) *routedBulkLane {
	rw.bulkMu.Lock()
	defer rw.bulkMu.Unlock()

	rw.bulkSeq++
	rw.bulkReader = &routedBulkLane{
		id: rw.bulkSeq,
		rw: bulk,
	}
	return rw.bulkReader
}

func (rw *routedMsgReadWriter) isCurrentBulk(id uint64) bool {
	rw.bulkMu.RLock()
	defer rw.bulkMu.RUnlock()

	return rw.bulkReader != nil && rw.bulkReader.id == id
}

func (rw *routedMsgReadWriter) clearBulk(id uint64) {
	rw.bulkMu.Lock()
	defer rw.bulkMu.Unlock()

	if rw.bulkReader != nil && rw.bulkReader.id == id {
		rw.bulkReader = nil
	}
}

func (rw *routedMsgReadWriter) readLoop(reader MsgReader, forwardErr bool, bulkID uint64) {
	for {
		msg, err := reader.ReadMsg()
		if !forwardErr && !rw.isCurrentBulk(bulkID) {
			return
		}
		if err == nil {
			payload, readErr := io.ReadAll(msg.Payload)
			if readErr != nil {
				err = readErr
			} else {
				msg.Size = uint32(len(payload))
				msg.Payload = bytes.NewReader(payload)
			}
		}
		if err != nil && !forwardErr {
			if isTimeoutError(err) {
				bulkSidecarReadTimeoutMeter.Mark(1)
				bulkSidecarStats.markChannelReadTimeout(rw.channel)
				continue
			}
			bulkSidecarReadErrorMeter.Mark(1)
			bulkSidecarStats.markChannelReadError(rw.channel)
			rw.clearBulk(bulkID)
			return
		}
		rw.reads <- routedReadResult{msg: msg, err: err}
		if err != nil {
			return
		}
	}
}

func isTimeoutError(err error) bool {
	var timeoutErr net.Error
	return errors.As(err, &timeoutErr) && timeoutErr.Timeout()
}
