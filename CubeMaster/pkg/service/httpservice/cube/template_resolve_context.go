// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"context"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
)

// templateResolveResult captures everything dealCubeboxCreateReqWithTemplate*
// has already learned about a template during the synchronous create path so
// that the post-create runtime-ref registration can reuse it instead of
// re-querying GetDefinition / ListReplicas. The struct is request-scoped and
// owned by createSandbox; callers obtain it via templateResolveResultFromContext.
type templateResolveResult struct {
	// TemplateID is the template id resolved from the create request
	// annotations. Empty for non-template creates.
	TemplateID string
	// Kind mirrors templatecenter.GetTemplateKind output for TemplateID.
	// Empty when the template path wasn't taken.
	Kind string
	// ChosenReplica is the replica selected by bindSnapshotCreateReplica
	// for snapshot templates. HasChosenReplica reports whether it was set.
	ChosenReplica    templatecenter.ReplicaStatus
	HasChosenReplica bool
}

type templateResolveResultCtxKey struct{}

func withTemplateResolveResult(ctx context.Context, r *templateResolveResult) context.Context {
	if ctx == nil || r == nil {
		return ctx
	}
	return context.WithValue(ctx, templateResolveResultCtxKey{}, r)
}

func templateResolveResultFromContext(ctx context.Context) *templateResolveResult {
	if ctx == nil {
		return nil
	}
	v, _ := ctx.Value(templateResolveResultCtxKey{}).(*templateResolveResult)
	return v
}
