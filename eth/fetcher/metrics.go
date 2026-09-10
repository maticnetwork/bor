// Copyright 2025 The go-ethereum Authors
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
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/

// Contains the metrics collected by the fetcher and witness manager.

package fetcher

import (
	"github.com/ethereum/go-ethereum/metrics"
)

var (
	// Witness verification metrics
	witnessVerifyCheckMeter       = metrics.NewRegisteredMeter("eth/fetcher/witness/verify/check", nil)
	witnessVerifySuccessMeter     = metrics.NewRegisteredMeter("eth/fetcher/witness/verify/success", nil)
	witnessVerifyFailureMeter     = metrics.NewRegisteredMeter("eth/fetcher/witness/verify/failure", nil)
	witnessVerifyDropMeter        = metrics.NewRegisteredMeter("eth/fetcher/witness/verify/drop", nil)
	witnessVerifyJailMeter        = metrics.NewRegisteredMeter("eth/fetcher/witness/verify/jail", nil)
	witnessVerifyPeersInsuffMeter = metrics.NewRegisteredMeter("eth/fetcher/witness/verify/peers/insufficient", nil)
	witnessVerifyNoConsensusMeter = metrics.NewRegisteredMeter("eth/fetcher/witness/verify/consensus/none", nil)

	// witnessByteMismatchMeter tracks WIT2 byte-correctness drops: a serving
	// peer delivered bytes whose keccak256 did not match the BP-signed hash.
	witnessByteMismatchMeter = metrics.NewRegisteredMeter("eth/fetcher/witness/byte_mismatch", nil)

	// Witness page count metrics
	witnessPageCountBelowThresholdMeter = metrics.NewRegisteredMeter("eth/fetcher/witness/pagecount/below_threshold", nil)
	witnessPageCountAboveThresholdMeter = metrics.NewRegisteredMeter("eth/fetcher/witness/pagecount/above_threshold", nil)

	// Witness threshold calculation metrics
	witnessThresholdGauge = metrics.NewRegisteredGauge("eth/fetcher/witness/threshold/current", nil)

	// Transaction announce metrics
	txAnnounceInMeter          = metrics.NewRegisteredMeter("eth/fetcher/transaction/announces/in", nil)
	txAnnounceKnownMeter       = metrics.NewRegisteredMeter("eth/fetcher/transaction/announces/known", nil)
	txAnnounceUnderpricedMeter = metrics.NewRegisteredMeter("eth/fetcher/transaction/announces/underpriced", nil)
	txAnnounceDOSMeter         = metrics.NewRegisteredMeter("eth/fetcher/transaction/announces/dos", nil)

	// Transaction broadcasts metrics
	txBroadcastInMeter          = metrics.NewRegisteredMeter("eth/fetcher/transaction/broadcasts/in", nil)
	txBroadcastKnownMeter       = metrics.NewRegisteredMeter("eth/fetcher/transaction/broadcasts/known", nil)
	txBroadcastUnderpricedMeter = metrics.NewRegisteredMeter("eth/fetcher/transaction/broadcasts/underpriced", nil)
	txBroadcastOtherRejectMeter = metrics.NewRegisteredMeter("eth/fetcher/transaction/broadcasts/otherreject", nil)

	// Transaction request metrics
	txRequestOutMeter     = metrics.NewRegisteredMeter("eth/fetcher/transaction/request/out", nil)
	txRequestFailMeter    = metrics.NewRegisteredMeter("eth/fetcher/transaction/request/fail", nil)
	txRequestDoneMeter    = metrics.NewRegisteredMeter("eth/fetcher/transaction/request/done", nil)
	txRequestTimeoutMeter = metrics.NewRegisteredMeter("eth/fetcher/transaction/request/timeout", nil)

	// Transaction replies metrics
	txReplyInMeter          = metrics.NewRegisteredMeter("eth/fetcher/transaction/replies/in", nil)
	txReplyKnownMeter       = metrics.NewRegisteredMeter("eth/fetcher/transaction/replies/known", nil)
	txReplyUnderpricedMeter = metrics.NewRegisteredMeter("eth/fetcher/transaction/replies/underpriced", nil)
	txReplyOtherRejectMeter = metrics.NewRegisteredMeter("eth/fetcher/transaction/replies/otherreject", nil)

	// Transaction waiting metrics
	txFetcherWaitingPeers  = metrics.NewRegisteredGauge("eth/fetcher/transaction/waiting/peers", nil)
	txFetcherWaitingHashes = metrics.NewRegisteredGauge("eth/fetcher/transaction/waiting/hashes", nil)

	// Transaction queueing metrics
	txFetcherQueueingPeers  = metrics.NewRegisteredGauge("eth/fetcher/transaction/queueing/peers", nil)
	txFetcherQueueingHashes = metrics.NewRegisteredGauge("eth/fetcher/transaction/queueing/hashes", nil)

	// Transaction fetching metrics
	txFetcherFetchingPeers  = metrics.NewRegisteredGauge("eth/fetcher/transaction/fetching/peers", nil)
	txFetcherFetchingHashes = metrics.NewRegisteredGauge("eth/fetcher/transaction/fetching/hashes", nil)

	txFetcherSlowPeers = metrics.NewRegisteredGauge("eth/fetcher/transaction/slow/peers", nil)

	// Note: this metric does not mean that the fetching of a transaction
	// was blocked by a specific peer during this period, since we request
	// another peer to fetch the same transaction hash.
	// The purpose of this metric is to measure how long it takes for a slow peer
	// to become "unfrozen", either by eventually replying to the request
	// or by being dropped, measuring from the moment the request was sent.
	txFetcherSlowWait = metrics.NewRegisteredHistogram("eth/fetcher/transaction/slow/wait", nil, metrics.NewExpDecaySample(1028, 0.015))
)
