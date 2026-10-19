package platform

import (
	"testing"

	"github.com/Resinat/Resin/internal/node"
)

func TestFullRebuildExcluding_PlatformScopedExclusion(t *testing.T) {
	h := makeHash(`{"type":"ss","excluder":1}`)
	entry := makeFullyRoutableEntry(h, "sub1")
	p := NewPlatform("p1", "Test", nil, nil)
	rangeNodes := func(fn func(node.Hash, *node.NodeEntry) bool) {
		fn(h, entry)
	}

	p.FullRebuildExcluding(rangeNodes, alwaysLookup, usGeoLookup, func(*node.NodeEntry) bool {
		return true
	})
	if p.View().Size() != 0 {
		t.Fatal("excluded node should not be routable")
	}

	p.FullRebuildExcluding(rangeNodes, alwaysLookup, usGeoLookup, func(*node.NodeEntry) bool {
		return false
	})
	if p.View().Size() != 1 {
		t.Fatal("node should be routable after exclusion is cleared")
	}
}

func TestNotifyDirtyExcluding_RemovesExcludedNode(t *testing.T) {
	h := makeHash(`{"type":"ss","excluder":2}`)
	entry := makeFullyRoutableEntry(h, "sub1")
	p := NewPlatform("p1", "Test", nil, nil)
	getEntry := func(node.Hash) (*node.NodeEntry, bool) { return entry, true }

	p.NotifyDirtyExcluding(h, getEntry, alwaysLookup, usGeoLookup, func(*node.NodeEntry) bool {
		return false
	})
	if !p.View().Contains(h) {
		t.Fatal("node should be added when not excluded")
	}

	p.NotifyDirtyExcluding(h, getEntry, alwaysLookup, usGeoLookup, func(*node.NodeEntry) bool {
		return true
	})
	if p.View().Contains(h) {
		t.Fatal("node should be removed when excluded")
	}
}
