package store

import (
	"slices"
	"testing"
	"time"

	"aredn-map/internal/model"
)

// Two hold-backs keep a sweep off the access point's resolver: host-table names and parked nodes.

func meshListStub(host string) *model.Node {
	return &model.Node{
		Hostname: host, Name: host, URL: "http://" + host + ".local.mesh",
		Source: "mesh-list", Hops: -1,
	}
}

func TestIDsNeedingDetailHoldsBackMeshListNames(t *testing.T) {
	s := New()
	s.UpsertNode(discoveryStub("neighbour", 1))  // reached through an observed link
	s.UpsertNode(meshListStub("far-cloud-peer")) // only named in a host table
	s.MergeNode("peer-seen-in-a-dialog", func(n *model.Node) { n.Source = "link-dialog" })

	got := s.IDsNeedingDetail(-1, time.Minute)
	if containsID(got, "far-cloud-peer") {
		t.Errorf("candidates = %v, want the host-table name held back", got)
	}
	// A peer at the end of a real link is how the tree grows outward, so it stays.
	if !containsID(got, "neighbour") || !containsID(got, "peer-seen-in-a-dialog") {
		t.Errorf("candidates = %v, want both adjacency-derived peers", got)
	}
}

// A host-table name that later turns out to be a real neighbour must not stay frozen.
func TestMeshListNameReopensWhenSeenInALink(t *testing.T) {
	s := New()
	s.UpsertNode(meshListStub("far-cloud-peer"))
	if got := s.IDsNeedingDetail(-1, time.Minute); containsID(got, "far-cloud-peer") {
		t.Fatalf("a pure host-table name was queued: %v", got)
	}
	s.UpsertNode(&model.Node{Hostname: "far-cloud-peer", Source: "link-dialog"})
	if got := s.IDsNeedingDetail(-1, time.Minute); !containsID(got, "far-cloud-peer") {
		t.Errorf("after appearing in a link dialog it is still held back: %v", got)
	}
}

func TestMarkFailureParksOnlyWhenGivenADueTime(t *testing.T) {
	s := New()
	s.UpsertNode(discoveryStub("a", 1))
	s.UpsertNode(discoveryStub("b", 1))
	s.UpsertNode(discoveryStub("c", 1))

	due := time.Now().Add(30 * time.Minute)
	s.MarkFailure("a", "dial tcp: i/o timeout", due)
	s.MarkFailure("b", "dial tcp: i/o timeout", time.Time{})

	if n := mustNode(t, s, "a"); n.RevisitAt.IsZero() || n.FailCount != 1 || n.LastError == "" {
		t.Errorf("parked node = %+v, want a due time, one failure and the reason", n)
	}
	if n := mustNode(t, s, "b"); !n.RevisitAt.IsZero() {
		t.Errorf("lane switched off but %q was parked at %v", n.Hostname, n.RevisitAt)
	}
	if n := mustNode(t, s, "b"); n.LastError == "" || n.FailCount != 1 {
		t.Errorf("switching the lane off lost the failure: %+v", n)
	}

	// A parked node is out of the detail set; an unparked one is not.
	if got := s.IDsNeedingDetail(-1, time.Minute); containsID(got, "a") || !containsID(got, "b") {
		t.Errorf("candidates = %v, want only the unparked node", got)
	}

	// An online probe releases the park whoever put it there.
	s.MarkProbe("a", ProbeOutcome{Online: true, ProbedAt: time.Now()})
	n := mustNode(t, s, "a")
	if !n.RevisitAt.IsZero() || n.FailCount != 0 {
		t.Errorf("a node that answered is still parked: %+v", n)
	}
	s.MarkFailure("c", "http 500", due)
	s.MarkReached("c")
	n = mustNode(t, s, "c")
	if !n.RevisitAt.IsZero() || n.FailCount != 0 || n.LastError != "" {
		t.Errorf("MarkReached did not fully release the park: %+v", n)
	}
}

func TestIDsDueForRevisit(t *testing.T) {
	s := New()
	for i := range 5 {
		id := []string{"a", "b", "c", "d", "e"}[i]
		s.UpsertNode(discoveryStub(id, 1))
	}
	now := time.Now()
	// Two overdue (one longer than the other), two future, one never parked.
	s.MarkFailure("a", "x", now.Add(-time.Hour))
	s.MarkFailure("b", "x", now.Add(-time.Minute))
	s.MarkFailure("c", "x", now.Add(time.Hour))
	s.MarkFailure("d", "x", now.Add(time.Minute))

	if got := s.IDsDueForRevisit(0, now); len(got) != 0 {
		t.Errorf("limit 0 gave %v, want nothing", got)
	}
	got := s.IDsDueForRevisit(10, now)
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("due list = %v, want [a b] most overdue first", got)
	}
	if one := s.IDsDueForRevisit(1, now); len(one) != 1 || one[0] != "a" {
		t.Errorf("batch of 1 = %v, want the most overdue only", one)
	}
	// The anchor is the running sweep's start, so a park it just set is not overdue for it.
	if all := s.IDsDueForRevisit(10, now.Add(24*time.Hour)); len(all) != 4 {
		t.Errorf("anchored a day later = %v, want all four parks", all)
	}
}

func containsID(ids []string, want string) bool {
	return slices.Contains(ids, want)
}
