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

func TestRunProfileScoresIncompleteOutputUsesDefault(t *testing.T) {
	selection := executorContext()
	err := runProfileScores(selection, []profile.ScorePlugin{{
		Name: "partial", Selector: partialScore{}, Weight: 1, Failure: profile.ScoreDefaultScore,
		DefaultScore: 25, ForceEnabled: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	got := selection.LeastScoreNodes(-1)
	if len(got) != 2 || got[0].Score != 25 || got[1].Score != 25 {
		t.Fatalf("scores = %+v", got)
	}
}
