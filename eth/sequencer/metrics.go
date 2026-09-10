package sequencer

import "github.com/ethereum/go-ethereum/metrics"

// Publisher state gauge values; 0 is reserved for "off" —
// a disabled node has no publisher and never reports. contending refines
// degraded: the store is healthy, we are just losing head races to another
// publisher.
const (
	gaugeLive       = 1
	gaugeDegraded   = 2
	gaugeResyncing  = 3
	gaugeFailed     = 4
	gaugeContending = 5
)

var (
	publishAckTimer        = metrics.NewRegisteredTimer("sequencer/publish/ack", nil)
	publishedCounter       = metrics.NewRegisteredCounter("sequencer/publish/entries", nil)
	publishDropMeter       = metrics.NewRegisteredMeter("sequencer/publish/dropped", nil)
	publishFailedGauge     = metrics.NewRegisteredGauge("sequencer/publish/failed", nil)
	publishStateGauge      = metrics.NewRegisteredGauge("sequencer/publish/state", nil)
	publishQueueGauge      = metrics.NewRegisteredGauge("sequencer/publish/queue", nil)
	publishStaleCount      = metrics.NewRegisteredCounter("sequencer/publish/stale", nil)
	publishMutedCount      = metrics.NewRegisteredCounter("sequencer/publish/muted", nil)
	publishRecoverCount    = metrics.NewRegisteredCounter("sequencer/publish/recovered", nil)
	readHeadMismatch       = metrics.NewRegisteredCounter("sequencer/read/headmismatch", nil)
	readUnexplained        = metrics.NewRegisteredCounter("sequencer/read/unexplained", nil)
	barrierDivergedCount   = metrics.NewRegisteredCounter("sequencer/publish/barrierdiverged", nil)
	publishCatchupSkip     = metrics.NewRegisteredCounter("sequencer/publish/catchupskip", nil)
	backfillBatchTimer     = metrics.NewRegisteredTimer("sequencer/backfill/batch", nil)
	windowDisplacedRecords = metrics.NewRegisteredCounter("sequencer/reconcile/displacedrecords", nil)
	publishBarrierTimeout  = metrics.NewRegisteredCounter("sequencer/publish/barriertimeout", nil)
	publishMidDrainMirror  = metrics.NewRegisteredCounter("sequencer/publish/middrainmirror", nil)
	publishRedialCount     = metrics.NewRegisteredCounter("sequencer/publish/redial", nil)

	// The seal gate's verdicts: confirmed seals broadcast, refused ones are
	// discarded (another producer won the height), unknown means the budget
	// expired and liveness broadcast anyway.
	gateConfirmedCount = metrics.NewRegisteredCounter("sequencer/gate/confirmed", nil)
	gateRefusedCount   = metrics.NewRegisteredCounter("sequencer/gate/refused", nil)
	gateUnknownCount   = metrics.NewRegisteredCounter("sequencer/gate/unknown", nil)
	gateRecheckRefused = metrics.NewRegisteredCounter("sequencer/gate/recheckrefused", nil)

	// Consumer-side preconfirmation pipeline: per-tx re-execution latency
	// and receipts served to RPC readers before canonical import.
	preconfApplyTimer         = metrics.NewRegisteredTimer("sequencer/preconf/apply", nil)
	preconfPublishedReceipts  = metrics.NewRegisteredCounter("sequencer/preconf/publishedreceipts", nil)
	preconfCanonicalReceipts  = metrics.NewRegisteredCounter("sequencer/preconf/canonicalreceipts", nil)
	preconfSenderCacheHit     = metrics.NewRegisteredCounter("sequencer/preconf/sendercachehit", nil)
	preconfSenderCacheMiss    = metrics.NewRegisteredCounter("sequencer/preconf/sendercachemiss", nil)
	preconfServedMeter        = metrics.NewRegisteredMeter("sequencer/preconf/served", nil)
	preconfPendingLogsDropped = metrics.NewRegisteredCounter("sequencer/preconf/pendinglogsdropped", nil)
	preconfPendingEntries     = metrics.NewRegisteredGauge("sequencer/preconf/pendingentries", nil)

	reconcileGapfill     = metrics.NewRegisteredCounter("sequencer/reconcile/gapfill", nil)
	reconcileAdopt       = metrics.NewRegisteredCounter("sequencer/reconcile/adopt", nil)
	reconcileSupersede   = metrics.NewRegisteredCounter("sequencer/reconcile/supersede", nil)
	reconcileForwardJump = metrics.NewRegisteredCounter("sequencer/reconcile/forwardjump", nil)
	reconcileYield       = metrics.NewRegisteredCounter("sequencer/reconcile/yield", nil)
	reconcileResync      = metrics.NewRegisteredCounter("sequencer/reconcile/resync", nil)
	reconcileTimer       = metrics.NewRegisteredTimer("sequencer/reconcile/duration", nil)
)
