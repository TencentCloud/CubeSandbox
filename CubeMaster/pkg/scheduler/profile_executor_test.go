// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package scheduler

import (
	"context"
	"errors"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/profile"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
)

type executorFilter struct {
	id   string
	keep map[string]bool
	err  error
}

func (f executorFilter) ID() string { return f.id }
func (f executorFilter) Select(selection *selctx.SelectorCtx) (node.NodeList, error) {
	if f.err != nil {
		return nil, f.err
	}
	result := make(node.NodeList, 0, len(selection.Nodes()))
	for _, candidate := range selection.Nodes() {
		if f.keep[candidate.ID()] {
			result = append(result, candidate)
		}
	}
	return result, nil
}

type executorScore struct {
	id     string
	values map[string]float64
	err    error
}

type foreignNodeFilter struct{}

func (foreignNodeFilter) ID() string { return "foreign-node" }
func (foreignNodeFilter) Select(*selctx.SelectorCtx) (node.NodeList, error) {
	return node.NodeList{{InsID: "foreign"}}, nil
}

type partialScore struct{}

func (partialScore) ID() string      { return "partial-score" }
func (partialScore) Weight() float64 { return 1 }
func (partialScore) Disable() bool   { return false }
func (partialScore) Select(selection *selctx.SelectorCtx) (node.NodeScoreList, error) {
	candidate := selection.Nodes()[0]
	return node.NodeScoreList{{InsID: candidate.ID(), OrigNode: candidate, Score: 90}}, nil
}

// foreignNodeScore returns a node outside the candidate set, which the
// pre-plugin scheduler silently merged into the aggregate.
type foreignNodeScore struct{}

func (foreignNodeScore) ID() string      { return "foreign-score" }
func (foreignNodeScore) Weight() float64 { return 1 }
func (foreignNodeScore) Disable() bool   { return false }
func (foreignNodeScore) Select(*selctx.SelectorCtx) (node.NodeScoreList, error) {
	return node.NodeScoreList{{InsID: "foreign", Score: 50}}, nil
}

// emptyScore mirrors built-in scorers such as affinity_score/image_score that
// return an empty list when the request is outside their scope.
type emptyScore struct{}

func (emptyScore) ID() string      { return "empty-score" }
func (emptyScore) Weight() float64 { return 1 }
func (emptyScore) Disable() bool   { return false }
func (emptyScore) Select(*selctx.SelectorCtx) (node.NodeScoreList, error) {
	return node.NodeScoreList{}, nil
}

func (s executorScore) ID() string      { return s.id }
func (s executorScore) Weight() float64 { return 1 }
func (s executorScore) Disable() bool   { return false }
func (s executorScore) Select(selection *selctx.SelectorCtx) (node.NodeScoreList, error) {
	if s.err != nil {
		return nil, s.err
	}
	result := make(node.NodeScoreList, 0, len(selection.Nodes()))
	for _, candidate := range selection.Nodes() {
		result = append(result, &node.NodeScore{InsID: candidate.ID(), OrigNode: candidate, Score: s.values[candidate.ID()]})
	}
	return result, nil
}

func executorContext() *selctx.SelectorCtx {
	selection := selctx.New("random")
	selection.Ctx = context.Background()
	selection.SetNodes(node.NodeList{{InsID: "n1"}, {InsID: "n2"}})
	return selection
}

func TestRunProfileFiltersFailOpenKeepsCandidateUniverse(t *testing.T) {
	selection := executorContext()
	err := runProfileFilters(selection, []profile.FilterPlugin{
		{Name: "only-n1", Selector: executorFilter{id: "only-n1", keep: map[string]bool{"n1": true}}, Failure: profile.FilterFailClosed},
		{Name: "broken", Selector: executorFilter{id: "broken", err: errors.New("boom")}, Failure: profile.FilterFailOpen},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(selection.Nodes()) != 1 || selection.Nodes()[0].ID() != "n1" {
		t.Fatalf("nodes = %v", selection.Nodes())
	}
}

func TestRunProfileFiltersFailClosed(t *testing.T) {
	selection := executorContext()
	err := runProfileFilters(selection, []profile.FilterPlugin{{
		Name: "broken", Selector: executorFilter{id: "broken", err: errors.New("boom")}, Failure: profile.FilterFailClosed,
	}})
	if err == nil {
		t.Fatal("fail-closed filter error must stop scheduling")
	}
	if isNoCandidateError(err) {
		t.Fatal("plugin failure must not be classified as an empty candidate set")
	}
}

func TestRunProfileFiltersRejectsNonCandidateNode(t *testing.T) {
	selection := executorContext()
	err := runProfileFilters(selection, []profile.FilterPlugin{{
		Name: "foreign-node", Selector: foreignNodeFilter{}, Failure: profile.FilterFailClosed,
	}})
	if err == nil {
		t.Fatal("non-candidate filter result must be rejected")
	}
}

func TestRunProfileFiltersEmptyResultIsNoCandidateError(t *testing.T) {
	selection := executorContext()
	err := runProfileFilters(selection, []profile.FilterPlugin{{
		Name: "empty", Selector: executorFilter{id: "empty", keep: map[string]bool{}}, Failure: profile.FilterFailClosed,
	}})
	if !isNoCandidateError(err) {
		t.Fatalf("error = %v, want no-candidate classification", err)
	}
}

func TestRunProfileFiltersInvalidOutputHonorsFailOpen(t *testing.T) {
	selection := executorContext()
	err := runProfileFilters(selection, []profile.FilterPlugin{{
		Name: "foreign-node", Selector: foreignNodeFilter{}, Failure: profile.FilterFailOpen,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(selection.Nodes()) != 2 {
		t.Fatalf("nodes = %v, want original candidate universe", selection.Nodes())
	}
}

func TestRunProfileScoresUsesDefaultAndStableOrder(t *testing.T) {
	selection := executorContext()
	err := runProfileScores(selection, []profile.ScorePlugin{
		{Name: "broken", Selector: executorScore{id: "broken", err: errors.New("boom")}, Weight: 1, Failure: profile.ScoreDefaultScore, DefaultScore: 20, ForceEnabled: true},
		{Name: "values", Selector: executorScore{id: "values", values: map[string]float64{"n1": 100, "n2": 0}}, Weight: 1, Failure: profile.ScoreFailClosed, ForceEnabled: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(selection.Nodes()) != 2 || selection.Nodes()[0].ID() != "n1" || selection.Nodes()[1].ID() != "n2" {
		t.Fatalf("ordered nodes = %v", selection.Nodes())
	}
	got := selection.LeastScoreNodes(-1)
	if got[0].Score != 60 || got[1].Score != 10 {
		t.Fatalf("scores = %+v", got)
	}
}

func TestRunProfileScoresInvalidOutputFailsDespiteDefaultScore(t *testing.T) {
	// Partial coverage is a validation-class error: the default-score policy
	// only absorbs transport/invocation failures, so malformed output fails
	// closed instead of being masked behind a constant default score.
	selection := executorContext()
	err := runProfileScores(selection, []profile.ScorePlugin{{
		Name: "partial", Selector: partialScore{}, Weight: 1, Failure: profile.ScoreDefaultScore,
		DefaultScore: 25, ForceEnabled: true,
	}})
	if err == nil {
		t.Fatal("partial coverage must fail closed even under the default-score policy")
	}
	if isNoCandidateError(err) {
		t.Fatal("a validation failure must not be classified as an empty candidate set")
	}
}

func TestRunProfileScoresEmptyBuiltinIsSkipped(t *testing.T) {
	// Under fail-closed the empty result must not abort scheduling, and under
	// default-score it must not dilute the other scorers with constant
	// defaults; the full-coverage scorer alone decides the order.
	for _, failure := range []profile.ScoreFailurePolicy{profile.ScoreFailClosed, profile.ScoreDefaultScore} {
		selection := executorContext()
		err := runProfileScores(selection, []profile.ScorePlugin{
			{Name: "empty", Selector: emptyScore{}, Weight: 1, Failure: failure, DefaultScore: 25, ForceEnabled: true, AllowEmpty: true},
			{Name: "values", Selector: executorScore{id: "values", values: map[string]float64{"n1": 100, "n2": 0}}, Weight: 1, Failure: profile.ScoreFailClosed, ForceEnabled: true},
		})
		if err != nil {
			t.Fatalf("policy %s: %v", failure, err)
		}
		got := selection.LeastScoreNodes(-1)
		if len(got) != 2 || got[0].Score != 100 || got[1].Score != 0 {
			t.Fatalf("policy %s: scores = %+v", failure, got)
		}
	}
}

func TestRunProfileScoresEmptyWithoutAllowEmptyFails(t *testing.T) {
	selection := executorContext()
	err := runProfileScores(selection, []profile.ScorePlugin{{
		Name: "empty", Selector: emptyScore{}, Weight: 1, Failure: profile.ScoreFailClosed, ForceEnabled: true,
	}})
	if err == nil {
		t.Fatal("empty output from a force-enabled scorer without AllowEmpty must fail")
	}
}

func TestRunProfileScoresPartialCoverageFailsClosed(t *testing.T) {
	selection := executorContext()
	err := runProfileScores(selection, []profile.ScorePlugin{{
		Name: "partial", Selector: partialScore{}, Weight: 1, Failure: profile.ScoreFailClosed, ForceEnabled: true,
	}})
	if err == nil {
		t.Fatal("partial coverage from a force-enabled scorer must fail")
	}
}

// The legacy default pipeline binds scorers with ScoreSkip and without
// ForceEnabled, so a scorer returning a non-candidate node (which the
// pre-plugin scheduler silently merged into the aggregate) is dropped
// entirely and scheduling still succeeds on the remaining scorers.
func TestRunProfileScoresLegacySkipsNonCandidateOutput(t *testing.T) {
	selection := executorContext()
	err := runProfileScores(selection, []profile.ScorePlugin{
		{Name: "foreign", Selector: foreignNodeScore{}, Weight: 1, Failure: profile.ScoreSkip},
		{Name: "values", Selector: executorScore{id: "values", values: map[string]float64{"n1": 100, "n2": 0}}, Weight: 1, Failure: profile.ScoreSkip},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := selection.LeastScoreNodes(-1)
	if len(got) != 2 || got[0].Score != 100 || got[1].Score != 0 {
		t.Fatalf("scores = %+v", got)
	}
}
