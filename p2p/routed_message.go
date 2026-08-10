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
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/log"
)

// NewRoutedMsgReadWriter multiplexes reads from the primary and bulk lanes while
// routing writes for selected message codes over the bulk lane.
//
// If bulk is nil or shouldRoute is nil, the primary lane is returned unchanged.
func NewRoutedMsgReadWriter(primary MsgReadWriter, bulk MsgReadWriter, shouldRoute func(code uint64) bool) MsgReadWriter {
	if shouldRoute == nil {
		return primary
	}
	rw := &routedMsgReadWriter{
		primary:     primary,
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
	shouldRoute func(code uint64) bool

	start      sync.Once
	bulkStart  sync.Once
	reads      chan routedReadResult
	bulkReader atomic.Value
}

func (rw *routedMsgReadWriter) ReadMsg() (Msg, error) {
	rw.start.Do(func() {
		go rw.readLoop(rw.primary, true)
	})

	result := <-rw.reads
	return result.msg, result.err
}

func (rw *routedMsgReadWriter) WriteMsg(msg Msg) error {
	if rw.shouldRoute(msg.Code) {
		if bulk, ok := rw.bulk(); ok {
			if err := bulk.WriteMsg(msg); err == nil {
				return nil
			}
		}
	}
	return rw.primary.WriteMsg(msg)
}

func (rw *routedMsgReadWriter) AttachBulk(bulk MsgReadWriter) {
	if bulk == nil {
		return
	}
	rw.bulkReader.Store(bulk)
	log.Debug("Attached bulk routed message lane", "reader_type", fmt.Sprintf("%T", bulk))
	rw.bulkStart.Do(func() {
		log.Debug("Starting bulk routed message read loop", "reader_type", fmt.Sprintf("%T", bulk))
		go rw.readLoop(bulk, false)
	})
}

func (rw *routedMsgReadWriter) bulk() (MsgReadWriter, bool) {
	reader := rw.bulkReader.Load()
	if reader == nil {
		return nil, false
	}
	return reader.(MsgReadWriter), true
}

func (rw *routedMsgReadWriter) HasBulk() bool {
	_, ok := rw.bulk()
	return ok
}

func (rw *routedMsgReadWriter) readLoop(reader MsgReader, forwardErr bool) {
	readerType := fmt.Sprintf("%T", reader)
	for {
		msg, err := reader.ReadMsg()
		if err == nil {
			payload, readErr := io.ReadAll(msg.Payload)
			if readErr != nil {
				err = readErr
			} else {
				msg.Size = uint32(len(payload))
				msg.Payload = bytes.NewReader(payload)
			}
		}
		if err == nil {
			log.Debug("Routed message read loop received message", "reader_type", readerType, "forward_err", forwardErr, "code", msg.Code, "size", msg.Size)
		}
		if err != nil && !forwardErr {
			log.Debug("Routed bulk read loop exiting", "reader_type", readerType, "err", err)
			return
		}
		rw.reads <- routedReadResult{msg: msg, err: err}
		if err != nil {
			log.Debug("Routed message read loop exiting", "reader_type", readerType, "forward_err", forwardErr, "err", err)
			return
		}
	}
}
