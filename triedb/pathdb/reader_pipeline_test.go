package pathdb

import (
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
)

type readerTestLayer struct {
	layer
	nodeFn    func(common.Hash, []byte, int) ([]byte, common.Hash, nodeLoc, error)
	accountFn func(common.Hash, int) ([]byte, error)
	storageFn func(common.Hash, common.Hash, int) ([]byte, error)
	parent    layer
}

func (l *readerTestLayer) node(owner common.Hash, path []byte, depth int) ([]byte, common.Hash, nodeLoc, error) {
	return l.nodeFn(owner, path, depth)
}

func (l *readerTestLayer) account(hash common.Hash, depth int) ([]byte, error) {
	return l.accountFn(hash, depth)
}

func (l *readerTestLayer) storage(accountHash, storageHash common.Hash, depth int) ([]byte, error) {
	return l.storageFn(accountHash, storageHash, depth)
}

func (l *readerTestLayer) parentLayer() layer {
	return l.parent
}

func (l *readerTestLayer) journal(io.Writer) error {
	return nil
}

func TestReaderNodeWalkPaths(t *testing.T) {
	t.Parallel()

	blob := []byte{0xc0}
	hash := common.HexToHash("0x1234")
	owner := common.HexToHash("0xabcd")
	path := []byte{1, 2, 3}
	boom := errors.New("read failed")

	tests := []struct {
		name        string
		noHashCheck bool
		node        func(common.Hash, []byte, int) ([]byte, common.Hash, nodeLoc, error)
		want        []byte
		wantError   string
	}{
		{
			name: "matching hash",
			node: func(common.Hash, []byte, int) ([]byte, common.Hash, nodeLoc, error) {
				return blob, hash, nodeLoc{loc: locDiffLayer}, nil
			},
			want: blob,
		},
		{
			name:        "hash check disabled",
			noHashCheck: true,
			node: func(common.Hash, []byte, int) ([]byte, common.Hash, nodeLoc, error) {
				return blob, common.Hash{}, nodeLoc{loc: locDiskLayer}, nil
			},
			want: blob,
		},
		{
			name: "layer error",
			node: func(common.Hash, []byte, int) ([]byte, common.Hash, nodeLoc, error) {
				return nil, common.Hash{}, nodeLoc{}, boom
			},
			wantError: boom.Error(),
		},
		{
			name: "dirty hash mismatch",
			node: func(common.Hash, []byte, int) ([]byte, common.Hash, nodeLoc, error) {
				return blob, common.HexToHash("0x9999"), nodeLoc{loc: locDirtyCache, depth: 2}, nil
			},
			wantError: "unexpected node:",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			r := &reader{
				noHashCheck: test.noHashCheck,
				layer: &readerTestLayer{
					nodeFn: test.node,
				},
			}
			got, err := r.nodeWalk(owner, path, hash)
			if test.wantError != "" {
				require.ErrorContains(t, err, test.wantError)
				require.Nil(t, got)
				return
			}
			require.NoError(t, err)
			require.Equal(t, test.want, got)
		})
	}
}

func TestReaderStaleFallbackHelpers(t *testing.T) {
	t.Parallel()

	db := New(rawdb.NewMemoryDatabase(), nil, false)
	stale := &readerTestLayer{
		nodeFn: func(common.Hash, []byte, int) ([]byte, common.Hash, nodeLoc, error) {
			return nil, common.Hash{}, nodeLoc{}, errSnapshotStale
		},
		accountFn: func(common.Hash, int) ([]byte, error) {
			return nil, errSnapshotStale
		},
		storageFn: func(common.Hash, common.Hash, int) ([]byte, error) {
			return nil, errSnapshotStale
		},
	}
	r := &reader{db: db, layer: stale, noHashCheck: true}

	blob, err := r.nodeWalk(common.Hash{}, []byte{1}, common.Hash{})
	require.NoError(t, err)
	require.Nil(t, blob)

	// The reader's state is not the base disk layer and not a descendant of
	// it, so the base cannot stand in for it: its flat data belongs to another
	// state and there is no hash to catch the substitution. Both fallbacks must
	// report staleness rather than serve it.
	blob, err = r.accountFallback(common.HexToHash("0x1"))
	require.ErrorIs(t, err, errSnapshotStale)
	require.Nil(t, blob)

	blob, err = r.storageFallback(common.HexToHash("0x1"), common.HexToHash("0x2"))
	require.ErrorIs(t, err, errSnapshotStale)
	require.Nil(t, blob)

	blob, got, loc, err := r.nodeFallback(common.Hash{}, []byte{1})
	require.NoError(t, err)
	require.Nil(t, blob)
	require.NotEqual(t, common.Hash{}, got)
	require.Equal(t, locDiskLayer, loc.loc)

	evictCachedNode(stale, common.Hash{}, []byte{1})
	require.Equal(t, "loc: diff, depth: 4", (nodeLoc{loc: locDiffLayer, depth: 4}).string())
}

func TestPipelineReaderIrregularBranches(t *testing.T) {
	t.Run("disk read errors and clean mismatch eviction", func(t *testing.T) {
		db := New(rawdb.NewMemoryDatabase(), nil, false)
		disk := db.tree.bottom()
		require.NotNil(t, disk.nodes)

		owner := common.HexToHash("0x1")
		path := []byte{1, 2}
		blob := []byte{0xc0}
		disk.nodes.Set(nodeCacheKey(owner, path), blob)
		r := &reader{db: db, layer: disk}
		got, ok := r.diskNode(owner, path, common.HexToHash("0xdead"))
		require.False(t, ok)
		require.Nil(t, got)
		require.False(t, disk.nodes.Has(nodeCacheKey(owner, path)))

		disk.markStale()
		got, ok = r.diskNode(owner, path, common.Hash{})
		require.False(t, ok)
		require.Nil(t, got)
	})

	t.Run("stale fallback and retry errors propagate", func(t *testing.T) {
		db := New(rawdb.NewMemoryDatabase(), nil, false)
		disk := db.tree.bottom()
		disk.markStale()
		stale := &readerTestLayer{
			nodeFn: func(common.Hash, []byte, int) ([]byte, common.Hash, nodeLoc, error) {
				return nil, common.Hash{}, nodeLoc{}, errSnapshotStale
			},
			accountFn: func(common.Hash, int) ([]byte, error) {
				return nil, errSnapshotStale
			},
			storageFn: func(common.Hash, common.Hash, int) ([]byte, error) {
				return nil, errSnapshotStale
			},
		}
		r := &reader{db: db, layer: stale}
		_, err := r.nodeWalk(common.Hash{}, nil, common.Hash{})
		require.ErrorIs(t, err, errSnapshotStale)
		_, err = r.accountFallback(common.Hash{})
		require.ErrorIs(t, err, errSnapshotStale)
		_, err = r.storageFallback(common.Hash{}, common.Hash{})
		require.ErrorIs(t, err, errSnapshotStale)
	})

	t.Run("clean cache retry error propagates", func(t *testing.T) {
		boom := errors.New("retry failed")
		calls := 0
		db := New(rawdb.NewMemoryDatabase(), nil, false)
		layer := &readerTestLayer{parent: db.tree.bottom()}
		layer.nodeFn = func(common.Hash, []byte, int) ([]byte, common.Hash, nodeLoc, error) {
			calls++
			if calls == 1 {
				return []byte{0xc0}, common.HexToHash("0x1"), nodeLoc{loc: locCleanCache}, nil
			}
			return nil, common.Hash{}, nodeLoc{}, boom
		}
		r := &reader{db: db, layer: layer}
		_, err := r.nodeWalk(common.Hash{}, nil, common.HexToHash("0x2"))
		require.ErrorIs(t, err, boom)
	})

	t.Run("unknown state readers are rejected", func(t *testing.T) {
		db := New(rawdb.NewMemoryDatabase(), nil, false)
		reader, err := db.NodeReader(common.HexToHash("0xdead"))
		require.Error(t, err)
		require.Nil(t, reader)
	})
}

func TestReaderPublicFallbackBranches(t *testing.T) {
	t.Run("Node delegates to the legacy walk", func(t *testing.T) {
		blob := []byte{0xc0}
		r := &reader{
			noHashCheck: true,
			layer: &readerTestLayer{
				nodeFn: func(common.Hash, []byte, int) ([]byte, common.Hash, nodeLoc, error) {
					return blob, common.Hash{}, nodeLoc{loc: locDiskLayer}, nil
				},
			},
		}
		got, err := r.Node(common.Hash{}, []byte{1}, common.Hash{})
		require.NoError(t, err)
		require.Equal(t, blob, got)
	})

	t.Run("stale lookup surfaces staleness for accounts and storage", func(t *testing.T) {
		db := New(rawdb.NewMemoryDatabase(), nil, false)
		stale := &readerTestLayer{
			accountFn: func(common.Hash, int) ([]byte, error) {
				return nil, errSnapshotStale
			},
			storageFn: func(common.Hash, common.Hash, int) ([]byte, error) {
				return nil, errSnapshotStale
			},
		}
		r := &reader{db: db, layer: stale, state: common.HexToHash("0xdead")}

		// A lookup-index rejection means the reader's state is neither the disk
		// layer nor a descendant of it, so the disk layer's flat data belongs to
		// a different state. Propagate the staleness instead of substituting it.
		_, err := r.AccountRLP(common.HexToHash("0x1"))
		require.ErrorIs(t, err, errSnapshotStale)
		_, err = r.Storage(common.HexToHash("0x1"), common.HexToHash("0x2"))
		require.ErrorIs(t, err, errSnapshotStale)
	})

	t.Run("located stale disk layer retries through fallback", func(t *testing.T) {
		db := New(rawdb.NewMemoryDatabase(), nil, false)
		disk := db.tree.bottom()
		root := disk.rootHash()
		r := &reader{db: db, layer: disk, state: root}
		disk.markStale()

		_, err := r.AccountRLP(common.HexToHash("0x1"))
		require.ErrorIs(t, err, errSnapshotStale)
		_, err = r.Storage(common.HexToHash("0x1"), common.HexToHash("0x2"))
		require.ErrorIs(t, err, errSnapshotStale)
	})
}
