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
	"errors"
	"io"
	"net"
	"sync"
)

const routedDefaultBulkChannel = "__bulk__"

// NewMultiChannelRoutedMsgReadWriter multiplexes reads from the primary lane
// plus any number of named sidecar lanes while routing writes by message code
// to the named lane selected by route.
func NewMultiChannelRoutedMsgReadWriter(primary MsgReadWriter, route func(code uint64) string) MsgReadWriter {
	if route == nil {
		return primary
	}
	return &routedMsgReadWriter{
		primary: primary,
		route:   route,
		reads:   make(chan routedReadResult, 2),
		bulks:   make(map[string]*routedBulkLane),
	}
}

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
	if channel == "" {
		channel = routedDefaultBulkChannel
	}
	rw := NewMultiChannelRoutedMsgReadWriter(primary, func(code uint64) string {
		if shouldRoute(code) {
			return channel
		}
		return ""
	}).(*routedMsgReadWriter)
	rw.defaultChannel = channel
	if bulk != nil {
		rw.AttachBulkChannel(channel, bulk)
	}
	return rw
}

type routedReadResult struct {
	msg Msg
	err error
}

type routedPayload struct {
	reader    io.Reader
	remaining uint32
	done      chan<- error
	once      sync.Once
}

func (p *routedPayload) Read(buf []byte) (int, error) {
	if p.remaining == 0 {
		p.finish(nil)
		return 0, io.EOF
	}
	if uint32(len(buf)) > p.remaining {
		buf = buf[:p.remaining]
	}
	n, err := p.reader.Read(buf)
	p.remaining -= uint32(n)

	if p.remaining == 0 {
		if errors.Is(err, io.EOF) {
			err = nil
		}
		p.finish(err)
	} else if err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		p.finish(err)
	}
	return n, err
}

func (p *routedPayload) finish(err error) {
	p.once.Do(func() { p.done <- err })
}

type routedMsgReadWriter struct {
	primary        MsgReadWriter
	defaultChannel string
	route          func(code uint64) string

	start   sync.Once
	reads   chan routedReadResult
	bulkMu  sync.RWMutex
	bulks   map[string]*routedBulkLane
	bulkSeq uint64
}

type routedBulkLane struct {
	id      uint64
	channel string
	rw      MsgReadWriter
}

func (rw *routedMsgReadWriter) ReadMsg() (Msg, error) {
	rw.start.Do(func() {
		go rw.readLoop(rw.primary, true, "", 0)
	})

	result := <-rw.reads
	return result.msg, result.err
}

func (rw *routedMsgReadWriter) WriteMsg(msg Msg) error {
	if channel := rw.route(msg.Code); channel != "" {
		if bulk, ok := rw.bulk(channel); ok {
			if err := bulk.WriteMsg(msg); err == nil {
				return nil
			} else {
				bulkSidecarWriteFallbackMeter.Mark(1)
				bulkSidecarStats.markChannelWriteFallback(channel)
			}
		}
	}
	return rw.primary.WriteMsg(msg)
}

func (rw *routedMsgReadWriter) AttachBulk(bulk MsgReadWriter) {
	rw.AttachBulkChannel(rw.defaultChannel, bulk)
}

func (rw *routedMsgReadWriter) AttachBulkChannel(channel string, bulk MsgReadWriter) {
	if channel == "" || bulk == nil {
		return
	}
	lane := rw.setBulk(channel, bulk)
	go rw.readLoop(lane.rw, false, lane.channel, lane.id)
}

func (rw *routedMsgReadWriter) bulk(channel string) (MsgReadWriter, bool) {
	if channel == "" {
		return nil, false
	}
	rw.bulkMu.RLock()
	defer rw.bulkMu.RUnlock()
	lane := rw.bulks[channel]
	if lane == nil {
		return nil, false
	}
	return lane.rw, true
}

func (rw *routedMsgReadWriter) HasBulkChannel(channel string) bool {
	_, ok := rw.bulk(channel)
	return ok
}

func (rw *routedMsgReadWriter) HasBulk() bool {
	rw.bulkMu.RLock()
	defer rw.bulkMu.RUnlock()
	return len(rw.bulks) > 0
}

func (rw *routedMsgReadWriter) setBulk(channel string, bulk MsgReadWriter) *routedBulkLane {
	rw.bulkMu.Lock()
	defer rw.bulkMu.Unlock()

	rw.bulkSeq++
	lane := &routedBulkLane{
		id:      rw.bulkSeq,
		channel: channel,
		rw:      bulk,
	}
	rw.bulks[channel] = lane
	return lane
}

func (rw *routedMsgReadWriter) isCurrentBulk(channel string, id uint64) bool {
	rw.bulkMu.RLock()
	defer rw.bulkMu.RUnlock()

	lane := rw.bulks[channel]
	return lane != nil && lane.id == id
}

func (rw *routedMsgReadWriter) clearBulk(channel string, id uint64) {
	rw.bulkMu.Lock()
	defer rw.bulkMu.Unlock()

	lane := rw.bulks[channel]
	if lane != nil && lane.id == id {
		delete(rw.bulks, channel)
	}
}

func (rw *routedMsgReadWriter) readLoop(reader MsgReader, forwardErr bool, channel string, bulkID uint64) {
	for {
		msg, err := reader.ReadMsg()
		if !forwardErr && !rw.isCurrentBulk(channel, bulkID) {
			return
		}
		if err != nil {
			if !forwardErr {
				if isTimeoutError(err) {
					bulkSidecarReadTimeoutMeter.Mark(1)
					bulkSidecarStats.markChannelReadTimeout(channel)
				} else {
					bulkSidecarReadErrorMeter.Mark(1)
					bulkSidecarStats.markChannelReadError(channel)
				}
				rw.clearBulk(channel, bulkID)
				return
			}
			rw.reads <- routedReadResult{msg: msg, err: err}
			return
		}
		if msg.Size == 0 {
			rw.reads <- routedReadResult{msg: msg}
			continue
		}
		done := make(chan error, 1)
		msg.Payload = &routedPayload{reader: msg.Payload, remaining: msg.Size, done: done}
		rw.reads <- routedReadResult{msg: msg}

		if err := <-done; err != nil {
			if !forwardErr {
				if isTimeoutError(err) {
					bulkSidecarReadTimeoutMeter.Mark(1)
					bulkSidecarStats.markChannelReadTimeout(channel)
				} else {
					bulkSidecarReadErrorMeter.Mark(1)
					bulkSidecarStats.markChannelReadError(channel)
				}
				rw.clearBulk(channel, bulkID)
			}
			return
		}
	}
}

func isTimeoutError(err error) bool {
	var timeoutErr net.Error
	return errors.As(err, &timeoutErr) && timeoutErr.Timeout()
}
