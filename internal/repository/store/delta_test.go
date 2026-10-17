package store

import (
	"fmt"
	"testing"
	"time"

	"aredn-map/internal/model"
)

// The ledger's two jobs: say nothing that did not move, and say each moved row exactly once.

func TestDeltaRepeatsCollapseToOneRow(t *testing.T) {
	s := New()
	s.UpsertNode(&model.Node{Hostname: "a", Name: "A", MeshIP: "10.0.0.1"})
	s.UpsertNode(&model.Node{Hostname: "a", Name: "A", LatencyMs: 12})
	s.MarkProbe("a", ProbeOutcome{Online: true, LatencyMs: 34, Sent: 1, Recv: 1})
	s.touch("a", func(n *model.Node) { n.DetailFetched = true })

	got := s.Delta(0, s.Generation())
	if len(got.Nodes) != 1 {
		t.Fatalf("four writes to one node pushed %d rows, want the node once", len(got.Nodes))
	}
	n := got.Nodes[0]
	if n.LatencyMs != 34 || !n.Online || !n.DetailFetched || n.MeshIP != "10.0.0.1" {
		t.Errorf("row is not the merged result of the writes: %+v", n)
	}

	// The cursor the frame handed out is current, so the next read is silent.
	again := s.Delta(got.Rev, got.Gen)
	if len(again.Nodes) != 0 || again.Full {
		t.Errorf("read from the current cursor gave %+v, want nothing", again)
	}
}

func TestDeltaCursorRules(t *testing.T) {
	s := New()
	s.UpsertNode(&model.Node{Hostname: "a", Name: "A"})

	if got := s.Delta(s.Rev()+1, s.Generation()); !got.Full {
		t.Error("a cursor ahead of the store was answered with a merge")
	}
	before := s.Generation()
	s.Reset()
	s.UpsertNode(&model.Node{Hostname: "b", Name: "B"})
	got := s.Delta(s.Rev()-1, before)
	if !got.Full {
		t.Error("a cursor from a previous generation was answered with a merge")
	}
	if len(got.Nodes) != 0 {
		t.Errorf("a full answer also carried %d rows; the client re-reads those", len(got.Nodes))
	}
}

func TestDeltaDeleteThenRediscover(t *testing.T) {
	s := New()
	s.UpsertNode(&model.Node{Hostname: "a", Name: "A"})
	s.DeleteNode("a")
	s.UpsertNode(&model.Node{Hostname: "a", Name: "A again"})

	got := s.Delta(1, s.Generation())
	if len(got.Nodes) != 1 || got.Nodes[0].Name != "A again" {
		t.Fatalf("nodes = %+v, want the rediscovered row", got.Nodes)
	}
	if len(got.Removed) != 0 {
		t.Errorf("removed = %v; a node that is back must not be reported as gone", got.Removed)
	}

	// The same pair in the other order is a plain delete.
	s2 := New()
	s2.UpsertNode(&model.Node{Hostname: "a", Name: "A"})
	rev := s2.Rev()
	s2.UpsertNode(&model.Node{Hostname: "b", Name: "B"})
	s2.DeleteNode("b")
	if got := s2.Delta(rev, s2.Generation()); len(got.Removed) != 1 || got.Removed[0] != "b" {
		t.Errorf("removed = %v, want [b]", got.Removed)
	}
}

// A burst is paced over several frames, not refused and not turned into a re-read.
func TestDeltaPacesABurst(t *testing.T) {
	s := New()
	for i := range deltaMaxRows + 10 {
		id := fmt.Sprintf("n-%04d", i)
		s.UpsertNode(&model.Node{Hostname: id, Name: id})
	}

	got := s.Delta(0, s.Generation())
	if got.Full {
		t.Fatal("a backlog over the cap asked for a re-read instead of a partial delta")
	}
	if len(got.Nodes) != deltaMaxRows {
		t.Fatalf("first frame carried %d rows, want exactly the cap of %d", len(got.Nodes), deltaMaxRows)
	}
	if got.Rev >= s.Rev() {
		t.Errorf("cut frame handed back cursor %d with the store at %d; the tail would be skipped", got.Rev, s.Rev())
	}

	rest := s.Delta(got.Rev, got.Gen)
	if len(rest.Nodes) != 10 {
		t.Fatalf("tail frame carried %d rows, want the remaining 10", len(rest.Nodes))
	}
	if rest.Rev != s.Rev() {
		t.Errorf("drained frame cursor = %d, want the live rev %d", rest.Rev, s.Rev())
	}
	if again := s.Delta(rest.Rev, rest.Gen); again.Full || len(again.Nodes) != 0 {
		t.Errorf("read after the drain gave %+v, want silence", again)
	}
}

// Links move on their own; both lanes live in one ledger and must not read back as each other.
func TestDeltaKeepsLinkAndNodeLanesApart(t *testing.T) {
	s := New()
	s.UpsertNode(&model.Node{Hostname: "a", Name: "A"})
	s.UpsertLink(model.Link{From: "a", To: "b", Type: model.LinkTypeRF, MAC: "aa:bb"})
	s.UpsertLink(model.Link{From: "a", To: "b", Type: model.LinkTypeRF, MAC: "aa:bb", RTTMs: 9})

	got := s.Delta(1, s.Generation())
	if len(got.Links) != 1 {
		t.Fatalf("links = %d, want the edge once", len(got.Links))
	}
	if got.Links[0].RTTMs != 9 {
		t.Errorf("edge is the stale observation: %+v", got.Links[0])
	}
	if len(got.Nodes) != 0 {
		t.Errorf("nodes = %+v, want only the edge to have moved", got.Nodes)
	}
}

// Most merges change nothing a reader can see; UpdatedAt keeps moving for relazy.
func TestDeltaIgnoresWritesThatChangedNothing(t *testing.T) {
	s := New()
	s.UpsertNode(&model.Node{Hostname: "a", Name: "A", MeshIP: "10.0.0.1", Source: "mesh-list", Hops: -1})
	first := s.Delta(0, s.Generation())
	if len(first.Nodes) != 1 {
		t.Fatalf("a brand-new row pushed %d nodes, want it announced", len(first.Nodes))
	}

	s.MergeNode("a", func(n *model.Node) {
		if n.Name == "" {
			n.Name = "A"
		}
		if n.MeshIP == "" {
			n.MeshIP = "10.0.0.1"
		}
	})
	s.touch("a", func(n *model.Node) { n.LastError = "" })
	if got := s.Delta(first.Rev, first.Gen); len(got.Nodes) != 0 {
		t.Errorf("writes that changed nothing pushed %+v", got.Nodes)
	}

	stored, _ := s.NodeByID("a")
	if !stored.UpdatedAt.After(first.Nodes[0].UpdatedAt) {
		t.Error("UpdatedAt stopped advancing; IDsNeedingDetail would re-detail every node forever")
	}

	s.MarkMeshState("a", model.MeshStateDisconnected, time.Now())
	got := s.Delta(first.Rev, first.Gen)
	if len(got.Nodes) != 1 || !got.Nodes[0].Disconnected {
		t.Errorf("a real change pushed %+v, want the node with its new state", got.Nodes)
	}
}

// A node that only exists as a shell still has to be announced.
func TestDeltaAnnouncesBareShells(t *testing.T) {
	s := New()
	s.touch("brand-new", func(n *model.Node) {})
	got := s.Delta(0, s.Generation())
	if len(got.Nodes) != 1 || got.Nodes[0].Hostname != "brand-new" {
		t.Fatalf("nodes = %+v, want the new shell announced", got.Nodes)
	}
	// Touching it again with nothing to add is not news.
	s.touch("brand-new", func(n *model.Node) {})
	if again := s.Delta(got.Rev, got.Gen); len(again.Nodes) != 0 {
		t.Errorf("second empty touch pushed %+v", again.Nodes)
	}
}
