package main

import (
	"bytes"
	"testing"
)

func TestPackRoundTrip(t *testing.T) {
	st := newStore("adaiversen")
	r := st.createRepo("adaiversen", "test-repo", false, "")
	blobOID := r.storeObject("blob", []byte("hello world\n"))
	treeOID := r.storeObject("tree", encodeTree([]treeEntry{{mode: "100644", name: "hello.txt", oid: blobOID}}))
	commitOID := r.storeObject("commit", buildCommitData(treeOID, nil, "Add hello"))
	pack := writePackFromRepo(r, []string{commitOID, treeOID, blobOID})
	if !verifyPackChecksum(pack) {
		t.Fatal("pack checksum failed")
	}
	// Read it back into a fresh repo and compare objects.
	r2 := st.createRepo("adaiversen", "test-repo-2", false, "")
	objs, deltas, err := readPack(r2, pack)
	if err != nil {
		t.Fatalf("readPack: %v", err)
	}
	if deltas != 0 {
		t.Fatalf("deltaCount = %d, want 0", deltas)
	}
	if len(objs) != 3 {
		t.Fatalf("read %d objects, want 3", len(objs))
	}
	for _, oid := range []string{commitOID, treeOID, blobOID} {
		o, ok := objs[oid]
		if !ok {
			t.Fatalf("missing object %s", oid)
		}
		if want := r.objects[oid]; !bytes.Equal(o.data, want.data) || o.typ != want.typ {
			t.Fatalf("object %s mismatch", oid)
		}
	}
}

func TestApplyDelta(t *testing.T) {
	// Insert-only delta producing "hello" from an empty base.
	delta := []byte{0x00, 0x05, 0x05, 'h', 'e', 'l', 'l', 'o'}
	out, err := applyDelta([]byte{}, delta)
	if err != nil {
		t.Fatalf("applyDelta: %v", err)
	}
	if string(out) != "hello" {
		t.Fatalf("got %q, want hello", out)
	}
	// Copy delta: base "hello world", take base[0:5].
	delta2 := []byte{0x0b, 0x05, 0x90, 0x05}
	out2, err := applyDelta([]byte("hello world"), delta2)
	if err != nil {
		t.Fatalf("applyDelta copy: %v", err)
	}
	if string(out2) != "hello" {
		t.Fatalf("got %q, want hello", out2)
	}
}

func TestTreeCommitRoundTrip(t *testing.T) {
	st := newStore("adaiversen")
	r := st.createRepo("adaiversen", "test-repo", false, "")
	blobOID := r.storeObject("blob", []byte("a\nb\n"))
	treeOID := r.treeSet("", "src/deep/file.txt", blobOID)
	if got := r.treeGet(treeOID, "src/deep/file.txt"); got != blobOID {
		t.Fatalf("treeGet = %q, want %q", got, blobOID)
	}
	flat := r.flattenTree(treeOID)
	if len(flat) != 1 || flat["src/deep/file.txt"] != blobOID {
		t.Fatalf("flattenTree = %v", flat)
	}
	commitOID := r.storeObject("commit", buildCommitData(treeOID, nil, "root commit"))
	tree, parents, msg, _ := parseCommit(r.objects[commitOID].data)
	if tree != treeOID || len(parents) != 0 || msg != "root commit" {
		t.Fatalf("parseCommit = (%q, %v, %q)", tree, parents, msg)
	}
	// Delete the file: the tree must become empty.
	empty := r.treeSet(treeOID, "src/deep/file.txt", "")
	if empty != "" {
		t.Fatalf("treeSet delete returned non-empty tree %q", empty)
	}
}

func TestFetchPlanning(t *testing.T) {
	st := newStore("adaiversen")
	r := st.createRepo("adaiversen", "test-repo", false, "")
	// Build a linear chain of 5 commits.
	var tip string
	for i := 0; i < 5; i++ {
		blob := r.storeObject("blob", []byte("content "+itoa(i)+"\n"))
		tree := r.storeObject("tree", encodeTree([]treeEntry{{mode: "100644", name: "f.txt", oid: blob}}))
		var parents []string
		if tip != "" {
			parents = []string{tip}
		}
		tip = r.storeObject("commit", buildCommitData(tree, parents, "commit "+itoa(i)))
	}
	r.refs["refs/heads/main"] = tip
	// Depth-1 fetch of the tip: only the tip commit is sent.
	plan := r.planFetch(fetchRequest{wants: []string{tip}, depth: 1})
	if len(plan.sendCommits) != 1 || plan.sendCommits[0] != tip {
		t.Fatalf("depth-1 send = %v, want [%s]", plan.sendCommits, tip)
	}
	if len(plan.shallowOut) != 1 || plan.shallowOut[0] != tip {
		t.Fatalf("depth-1 shallow = %v, want [%s]", plan.shallowOut, tip)
	}
	// Full fetch: all 5 commits.
	plan = r.planFetch(fetchRequest{wants: []string{tip}, depth: -1})
	if len(plan.sendCommits) != 5 {
		t.Fatalf("full send = %d commits, want 5", len(plan.sendCommits))
	}
	// Fetch-nothing-new: haves = tip.
	plan = r.planFetch(fetchRequest{wants: []string{tip}, haves: []string{tip}, depth: -1})
	if len(plan.sendCommits) != 0 {
		t.Fatalf("nothing-new send = %v, want []", plan.sendCommits)
	}
	if plan.ack != tip {
		t.Fatalf("ack = %q, want %q", plan.ack, tip)
	}
	// Deepen: client holds the tip as shallow, wants it with depth 2.
	plan = r.planFetch(fetchRequest{wants: []string{tip}, haves: []string{tip}, shallow: []string{tip}, depth: 2})
	if len(plan.sendCommits) != 1 {
		t.Fatalf("deepen send = %v, want 1 commit", plan.sendCommits)
	}
	if len(plan.unshallow) != 1 || plan.unshallow[0] != tip {
		t.Fatalf("deepen unshallow = %v, want [%s]", plan.unshallow, tip)
	}
}

func TestFetchIncrementalSharedBlob(t *testing.T) {
	st := newStore("adaiversen")
	r := st.createRepo("adaiversen", "test-repo", false, "")
	sharedBlob := r.storeObject("blob", []byte("shared content\n"))
	firstTree := r.storeObject("tree", encodeTree([]treeEntry{{mode: "100644", name: "shared.txt", oid: sharedBlob}}))
	first := r.storeObject("commit", buildCommitData(firstTree, nil, "first"))
	newBlob := r.storeObject("blob", []byte("new content\n"))
	secondTree := r.storeObject("tree", encodeTree([]treeEntry{
		{mode: "100644", name: "new.txt", oid: newBlob},
		{mode: "100644", name: "shared.txt", oid: sharedBlob},
	}))
	second := r.storeObject("commit", buildCommitData(secondTree, []string{first}, "second"))
	r.refs["refs/heads/main"] = second
	plan := r.planFetch(fetchRequest{wants: []string{second}, haves: []string{first}, depth: -1})
	for _, oid := range plan.packOIDs {
		if oid == sharedBlob {
			t.Fatalf("shared blob %s was re-sent in an incremental fetch", sharedBlob)
		}
		if oid == first || oid == firstTree {
			t.Fatalf("held object %s was re-sent", oid)
		}
	}
	found := map[string]bool{}
	for _, oid := range plan.packOIDs {
		found[oid] = true
	}
	for _, want := range []string{second, secondTree, newBlob} {
		if !found[want] {
			t.Fatalf("new object %s missing from incremental pack", want)
		}
	}
}
