// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package localcache

import (
	"math/rand"
	"strconv"
	"sync"
	"testing"

	"k8s.io/apimachinery/pkg/util/sets"
)

func assertLocal(t *testing.T, tl *templateLocality, templateID, nodeID string, want bool) {
	t.Helper()
	state := tl.image(templateID)
	got := state != nil && state.HasNode(nodeID)
	if got != want {
		t.Fatalf("local(%s, %s) = %v, want %v", templateID, nodeID, got, want)
	}
}

func TestTemplateLocalityRegisteredSurvivesHeartbeatOmission(t *testing.T) {
	tl := newTemplateLocality()
	tl.register("tpl", "node", 1)
	for i := 0; i < 5; i++ {
		diff := tl.replaceReported("node", sets.New[string]())
		if len(diff.added)+len(diff.removed)+len(diff.keptByRegistration) != 0 {
			t.Fatalf("heartbeat that never reported tpl must be a no-op, diff=%+v", diff)
		}
		assertLocal(t, tl, "tpl", "node", true)
	}
}

func TestTemplateLocalityRegisteredSurvivesNilHeartbeat(t *testing.T) {
	tl := newTemplateLocality()
	tl.register("tpl", "node", 1)
	diff := tl.replaceReported("node", normalizeTemplateIDSet(nil))
	if len(diff.added)+len(diff.removed)+len(diff.keptByRegistration) != 0 {
		t.Fatalf("nil heartbeat must be a no-op, diff=%+v", diff)
	}
	assertLocal(t, tl, "tpl", "node", true)
}

func TestTemplateLocalityHeartbeatOmissionRemovesUnregistered(t *testing.T) {
	tl := newTemplateLocality()
	diff := tl.replaceReported("node", normalizeTemplateIDSet([]string{"tpl"}))
	if len(diff.added) != 1 || diff.added[0] != "tpl" {
		t.Fatalf("added = %v", diff.added)
	}
	assertLocal(t, tl, "tpl", "node", true)

	diff = tl.replaceReported("node", sets.New[string]())
	if len(diff.removed) != 1 || diff.removed[0] != "tpl" {
		t.Fatalf("removed = %v", diff.removed)
	}
	assertLocal(t, tl, "tpl", "node", false)
	if tl.image("tpl") != nil {
		t.Fatal("image entry must be deleted when no writer asserts the template")
	}
}

func TestTemplateLocalityHeartbeatDropKeepsRegistration(t *testing.T) {
	tl := newTemplateLocality()
	tl.register("tpl", "node", 1)
	tl.replaceReported("node", normalizeTemplateIDSet([]string{"tpl"}))

	diff := tl.replaceReported("node", sets.New[string]())
	if len(diff.keptByRegistration) != 1 || diff.keptByRegistration[0] != "tpl" {
		t.Fatalf("keptByRegistration = %v", diff.keptByRegistration)
	}
	assertLocal(t, tl, "tpl", "node", true)

	tl.deregister("tpl", "node")
	assertLocal(t, tl, "tpl", "node", false)
	if tl.image("tpl") != nil {
		t.Fatal("image entry must be deleted after the last assertion is withdrawn")
	}
}

func TestTemplateLocalityDeregisterKeepsHeartbeat(t *testing.T) {
	tl := newTemplateLocality()
	tl.register("tpl", "node", 1)
	tl.replaceReported("node", normalizeTemplateIDSet([]string{"tpl"}))

	tl.deregister("tpl", "node")
	assertLocal(t, tl, "tpl", "node", true)

	tl.replaceReported("node", sets.New[string]())
	assertLocal(t, tl, "tpl", "node", false)
	if tl.image("tpl") != nil {
		t.Fatal("image entry must be deleted after the heartbeat drops the template")
	}
}

func TestTemplateLocalityInvalidateDropsOnlyRegistration(t *testing.T) {
	tl := newTemplateLocality()
	tl.register("tpl", "node-a", 1)
	tl.register("tpl", "node-b", 1)
	tl.replaceReported("node-b", normalizeTemplateIDSet([]string{"tpl"}))
	tl.replaceReported("node-c", normalizeTemplateIDSet([]string{"tpl"}))

	tl.invalidateTemplate("tpl")

	assertLocal(t, tl, "tpl", "node-a", false)
	assertLocal(t, tl, "tpl", "node-b", true)
	assertLocal(t, tl, "tpl", "node-c", true)
	if _, ok := tl.registered["tpl"]; ok {
		t.Fatal("invalidation must clear registered assertions")
	}
}

func TestTemplateLocalityInvalidateWithoutReportersDeletesImage(t *testing.T) {
	tl := newTemplateLocality()
	tl.register("tpl", "node", 1)
	tl.invalidateTemplate("tpl")
	if tl.image("tpl") != nil {
		t.Fatal("image entry must be deleted when invalidation removes the only assertion")
	}
}

func TestTemplateLocalityRemoveNodeClearsBothWriters(t *testing.T) {
	tl := newTemplateLocality()
	tl.register("tpl", "node", 1)
	tl.register("other", "node-b", 1)
	tl.replaceReported("node", normalizeTemplateIDSet([]string{"tpl", "hb"}))

	tl.removeNode("node")

	assertLocal(t, tl, "tpl", "node", false)
	assertLocal(t, tl, "hb", "node", false)
	assertLocal(t, tl, "other", "node-b", true)
	if _, ok := tl.reported["node"]; ok {
		t.Fatal("removed node must not remain in heartbeat assertions")
	}
	if tl.registered["tpl"].Has("node") {
		t.Fatal("removed node must not remain in registered assertions")
	}
}

func TestTemplateLocalitySizeFollowsLatestPositiveRegistration(t *testing.T) {
	tl := newTemplateLocality()
	tl.register("tpl", "node", 0)
	if tl.image("tpl").Size != 0 {
		t.Fatalf("Size = %d, want 0", tl.image("tpl").Size)
	}
	tl.register("tpl", "node", 4096)
	if tl.image("tpl").Size != 4096 {
		t.Fatalf("Size = %d, want 4096", tl.image("tpl").Size)
	}
	tl.register("tpl", "node", 0)
	if tl.image("tpl").Size != 4096 {
		t.Fatalf("Size = %d, want 4096 after a zero-size refresh", tl.image("tpl").Size)
	}
	tl.register("tpl", "node", 1)
	if tl.image("tpl").Size != 1 {
		t.Fatalf("Size = %d, want 1", tl.image("tpl").Size)
	}
}

func TestTemplateLocalityProjectionInvariant(t *testing.T) {
	const (
		templateN = 4
		nodeN     = 4
		steps     = 3000
	)
	tl := newTemplateLocality()
	registered := make([]sets.Set[string], templateN)
	reported := make([]sets.Set[string], nodeN)
	rng := rand.New(rand.NewSource(1))
	templateID := func(i int) string { return "t" + strconv.Itoa(i) }
	nodeID := func(i int) string { return "n" + strconv.Itoa(i) }

	for step := 0; step < steps; step++ {
		ti := rng.Intn(templateN)
		ni := rng.Intn(nodeN)
		switch rng.Intn(5) {
		case 0:
			tl.register(templateID(ti), nodeID(ni), 1)
			if registered[ti] == nil {
				registered[ti] = sets.New[string]()
			}
			registered[ti].Insert(nodeID(ni))
		case 1:
			tl.deregister(templateID(ti), nodeID(ni))
			if registered[ti] != nil {
				registered[ti].Delete(nodeID(ni))
				if registered[ti].Len() == 0 {
					registered[ti] = nil
				}
			}
		case 2:
			tl.invalidateTemplate(templateID(ti))
			registered[ti] = nil
		case 3:
			current := sets.New[string]()
			for i := 0; i < templateN; i++ {
				if rng.Intn(2) == 0 {
					current.Insert(templateID(i))
				}
			}
			tl.replaceReported(nodeID(ni), current)
			if current.Len() == 0 {
				reported[ni] = nil
			} else {
				reported[ni] = current
			}
		default:
			tl.removeNode(nodeID(ni))
			reported[ni] = nil
			for i := range registered {
				if registered[i] == nil {
					continue
				}
				registered[i].Delete(nodeID(ni))
				if registered[i].Len() == 0 {
					registered[i] = nil
				}
			}
		}

		for i := 0; i < templateN; i++ {
			want := sets.New[string]()
			for node := range registered[i] {
				want.Insert(node)
			}
			id := templateID(i)
			for n := 0; n < nodeN; n++ {
				if reported[n].Has(id) {
					want.Insert(nodeID(n))
				}
			}
			state := tl.image(id)
			if want.Len() == 0 {
				if state != nil {
					t.Fatalf("step %d: image[%s] exists with no assertions", step, id)
				}
				continue
			}
			if state == nil {
				t.Fatalf("step %d: image[%s] missing, want %v", step, id, want)
			}
			if state.GetNumNodes() != want.Len() {
				t.Fatalf("step %d: image[%s] nodes=%d want %d", step, id, state.GetNumNodes(), want.Len())
			}
			for n := 0; n < nodeN; n++ {
				got := state.HasNode(nodeID(n))
				if got != want.Has(nodeID(n)) {
					t.Fatalf("step %d: image[%s] node %s local=%v want %v", step, id, nodeID(n), got, want.Has(nodeID(n)))
				}
			}
		}
	}
}

func TestTemplateLocalityConcurrentFirstRegistration(t *testing.T) {
	tl := newTemplateLocality()
	var wg sync.WaitGroup
	const nodes = 64
	wg.Add(nodes)
	for i := 0; i < nodes; i++ {
		nodeID := "node-" + strconv.Itoa(i)
		go func() {
			defer wg.Done()
			tl.register("tpl", nodeID, 1)
		}()
	}
	wg.Wait()
	state := tl.image("tpl")
	if state == nil {
		t.Fatalf("nodes = 0, want %d", nodes)
	}
	if state.GetNumNodes() != nodes {
		t.Fatalf("nodes = %d, want %d", state.GetNumNodes(), nodes)
	}
}

func TestTemplateLocalityConcurrentRegisterAndEmptyHeartbeat(t *testing.T) {
	tl := newTemplateLocality()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			tl.register("tpl", "node", 1)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			tl.replaceReported("node", sets.New[string]())
		}
	}()
	wg.Wait()
	assertLocal(t, tl, "tpl", "node", true)
}

func TestTemplateLocalityInvalidateDoesNotHideHeartbeat(t *testing.T) {
	isolatePackageLocality(t)
	l.locality.replaceReported("node-b", normalizeTemplateIDSet([]string{"tpl"}))

	var wg sync.WaitGroup
	failed := make(chan struct{}, 1)
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			l.locality.register("tpl", "node-a", 1)
			l.locality.invalidateTemplate("tpl")
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			if GetImageStateByNode("tpl", "node-b") == nil {
				select {
				case failed <- struct{}{}:
				default:
				}
				return
			}
		}
	}()
	wg.Wait()
	select {
	case <-failed:
		t.Fatal("heartbeat-reported locality disappeared while registration was invalidated")
	default:
	}
	assertLocal(t, l.locality, "tpl", "node-b", true)
}
