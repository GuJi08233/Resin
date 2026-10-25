package main

import (
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/config"
	"github.com/Resinat/Resin/internal/model"
	"github.com/Resinat/Resin/internal/node"
)

func TestFlushReadersExcludeRuntimeWarpNodes(t *testing.T) {
	subs, pool := newBootstrapTestRuntime(config.NewDefaultRuntimeConfig())
	parent := node.HashFromRawOptions([]byte(`{"type":"socks","server":"parent"}`))
	raw := []byte(`{"type":"resin-warp","family":"ipv4","slot":1}`)
	entry := node.NewNodeEntry(node.HashFromRawOptions(raw), raw, time.Now(), 8)
	defer entry.LatencyTable.Close()
	entry.ParentHash, entry.WarpFamily = &parent, "ipv4"
	entry.LatencyTable.Update("cloudflare.com", time.Millisecond, time.Minute)
	pool.LoadNodeFromBootstrap(entry)
	readers := newFlushReaders(pool, subs, nil)
	if readers.ReadNodeStatic(entry.Hash.Hex()) != nil || readers.ReadNodeDynamic(entry.Hash.Hex()) != nil ||
		readers.ReadNodeLatency(model.NodeLatencyKey{NodeHash: entry.Hash.Hex(), Domain: "cloudflare.com"}) != nil {
		t.Fatal("runtime WARP metadata must not be persisted or rebuilt as a subscription node")
	}
}
