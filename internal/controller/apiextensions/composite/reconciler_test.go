/*
Copyright 2020 The Crossplane Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package composite

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	xpv1 "github.com/crossplane/crossplane-runtime/apis/common/v1"
	"github.com/crossplane/crossplane-runtime/pkg/connection/store"
	"github.com/crossplane/crossplane-runtime/pkg/errors"
	"github.com/crossplane/crossplane-runtime/pkg/event"
	"github.com/crossplane/crossplane-runtime/pkg/meta"
	"github.com/crossplane/crossplane-runtime/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/pkg/resource"
	"github.com/crossplane/crossplane-runtime/pkg/resource/unstructured/claim"
	"github.com/crossplane/crossplane-runtime/pkg/resource/unstructured/composed"
	"github.com/crossplane/crossplane-runtime/pkg/resource/unstructured/composite"
	"github.com/crossplane/crossplane-runtime/pkg/test"

	v1 "github.com/crossplane/crossplane/apis/apiextensions/v1"
	"github.com/crossplane/crossplane/internal/engine"
)

func TestReconcile(t *testing.T) {
	errBoom := errors.New("boom")
	cd := managed.ConnectionDetails{"a": []byte("b")}

	type args struct {
		client client.Client
		of     resource.CompositeKind
		opts   []ReconcilerOption
	}
	type want struct {
		r   reconcile.Result
		err error
	}

	now := metav1.Now()

	cases := map[string]struct {
		reason string
		args   args
		want   want
	}{
		"CompositeResourceNotFound": {
			reason: "We should not return an error if the composite resource was not found.",
			args: args{
				client: &test.MockClient{
					MockGet: test.NewMockGetFn(kerrors.NewNotFound(schema.GroupResource{}, "")),
				},
			},
			want: want{
				r: reconcile.Result{Requeue: false},
			},
		},
		"GetCompositeResourceError": {
			reason: "We should return error encountered while getting the composite resource.",
			args: args{
				client: &test.MockClient{
					MockGet: test.NewMockGetFn(errBoom),
				},
			},
			want: want{
				err: errors.Wrap(errBoom, errGet),
			},
		},
		"UnpublishConnectionError": {
			reason: "We should return any error encountered while unpublishing connection details.",
			args: args{
				client: &test.MockClient{
					MockGet: WithComposite(t, NewComposite(func(cr resource.Composite) {
						cr.SetDeletionTimestamp(&now)
					})),
					MockStatusUpdate: WantComposite(t, NewComposite(func(want resource.Composite) {
						want.SetDeletionTimestamp(&now)
						want.SetConditions(xpv1.Deleting(), xpv1.ReconcileError(errors.Wrap(errBoom, errUnpublish)))
					})),
				},
				opts: []ReconcilerOption{
					WithCompositeFinalizer(resource.NewNopFinalizer()),
					WithConnectionPublisher(managed.ConnectionPublisherFns{
						UnpublishConnectionFn: func(_ context.Context, _ store.SecretOwner, _ managed.ConnectionDetails) error {
							return errBoom
						},
					}),
				},
			},
			want: want{
				r: reconcile.Result{Requeue: true},
			},
		},
		"RemoveFinalizerError": {
			reason: "We should return any error encountered while removing finalizer.",
			args: args{
				client: &test.MockClient{
					MockGet: WithComposite(t, NewComposite(func(cr resource.Composite) {
						cr.SetDeletionTimestamp(&now)
					})),
					MockStatusUpdate: WantComposite(t, NewComposite(func(cr resource.Composite) {
						cr.SetDeletionTimestamp(&now)
						cr.SetConditions(xpv1.Deleting(), xpv1.ReconcileError(errors.Wrap(errBoom, errRemoveFinalizer)))
					})),
				},
				opts: []ReconcilerOption{
					WithCompositeFinalizer(resource.FinalizerFns{
						RemoveFinalizerFn: func(_ context.Context, _ resource.Object) error {
							return errBoom
						},
					}),
					WithConnectionPublisher(managed.ConnectionPublisherFns{
						UnpublishConnectionFn: func(_ context.Context, _ store.SecretOwner, _ managed.ConnectionDetails) error {
							return nil
						},
					}),
				},
			},
			want: want{
				r: reconcile.Result{Requeue: true},
			},
		},
		"SuccessfulDelete": {
			reason: "We should return no error when deleted successfully.",
			args: args{
				client: &test.MockClient{
					MockGet: WithComposite(t, NewComposite(func(cr resource.Composite) {
						cr.SetDeletionTimestamp(&now)
					})),
					MockStatusUpdate: WantComposite(t, NewComposite(func(cr resource.Composite) {
						cr.SetDeletionTimestamp(&now)
						cr.SetConditions(xpv1.Deleting(), xpv1.ReconcileSuccess())
					})),
				},
				opts: []ReconcilerOption{
					WithCompositeFinalizer(resource.FinalizerFns{
						RemoveFinalizerFn: func(_ context.Context, _ resource.Object) error {
							return nil
						},
					}),
					WithConnectionPublisher(managed.ConnectionPublisherFns{
						UnpublishConnectionFn: func(_ context.Context, _ store.SecretOwner, _ managed.ConnectionDetails) error {
							return nil
						},
					}),
				},
			},
			want: want{
				err: nil,
			},
		},
		"AddFinalizerError": {
			reason: "We should return any error encountered while adding finalizer.",
			args: args{
				client: &test.MockClient{
					MockGet: test.NewMockGetFn(nil),
					MockStatusUpdate: WantComposite(t, NewComposite(func(cr resource.Composite) {
						cr.SetConditions(xpv1.ReconcileError(errors.Wrap(errBoom, errAddFinalizer)))
					})),
				},
				opts: []ReconcilerOption{
					WithCompositeFinalizer(resource.FinalizerFns{
						AddFinalizerFn: func(_ context.Context, _ resource.Object) error {
							return errBoom
						},
					}),
				},
			},
			want: want{
				r: reconcile.Result{Requeue: true},
			},
		},
		"SelectCompositionError": {
			reason: "We should return any error encountered while selecting a composition.",
			args: args{
				client: &test.MockClient{
					MockGet: test.NewMockGetFn(nil),
					MockStatusUpdate: WantComposite(t, NewComposite(func(cr resource.Composite) {
						cr.SetConditions(xpv1.ReconcileError(errors.Wrap(errBoom, errSelectComp)))
					})),
				},
				opts: []ReconcilerOption{
					WithCompositeFinalizer(resource.NewNopFinalizer()),
					WithCompositionSelector(CompositionSelectorFn(func(_ context.Context, _ resource.Composite) error {
						return errBoom
					})),
				},
			},
			want: want{
				r: reconcile.Result{Requeue: true},
			},
		},
		"FetchCompositionError": {
			reason: "We should return any error encountered while fetching a composition.",
			args: args{
				client: &test.MockClient{
					MockGet: test.NewMockGetFn(nil),
					MockStatusUpdate: WantComposite(t, NewComposite(func(cr resource.Composite) {
						cr.SetCompositionReference(&corev1.ObjectReference{})
						cr.SetConditions(xpv1.ReconcileError(errors.Wrap(errBoom, errFetchComp)))
					})),
				},
				opts: []ReconcilerOption{
					WithCompositeFinalizer(resource.NewNopFinalizer()),
					WithCompositionSelector(CompositionSelectorFn(func(_ context.Context, cr resource.Composite) error {
						cr.SetCompositionReference(&corev1.ObjectReference{})
						return nil
					})),
					WithCompositionRevisionFetcher(CompositionRevisionFetcherFn(func(_ context.Context, _ resource.Composite) (*v1.CompositionRevision, error) {
						return nil, errBoom
					})),
				},
			},
			want: want{
				r: reconcile.Result{Requeue: true},
			},
		},
		"ValidateCompositionError": {
			reason: "We should return any error encountered while validating our Composition.",
			args: args{
				client: &test.MockClient{
					MockGet: test.NewMockGetFn(nil),
					MockStatusUpdate: WantComposite(t, NewComposite(func(cr resource.Composite) {
						cr.SetCompositionReference(&corev1.ObjectReference{})
						cr.SetConditions(xpv1.ReconcileError(errors.Wrap(errBoom, errValidate)))
					})),
				},
				opts: []ReconcilerOption{
					WithCompositeFinalizer(resource.NewNopFinalizer()),
					WithCompositionSelector(CompositionSelectorFn(func(_ context.Context, cr resource.Composite) error {
						cr.SetCompositionReference(&corev1.ObjectReference{})
						return nil
					})),
					WithCompositionRevisionFetcher(CompositionRevisionFetcherFn(func(_ context.Context, _ resource.Composite) (*v1.CompositionRevision, error) {
						return &v1.CompositionRevision{}, nil
					})),
					WithCompositionRevisionValidator(CompositionRevisionValidatorFn(func(_ *v1.CompositionRevision) error {
						return errBoom
					})),
				},
			},
			want: want{
				r: reconcile.Result{Requeue: true},
			},
		},
		"ConfigureCompositeError": {
			reason: "We should return any error encountered while configuring the composite resource.",
			args: args{
				client: &test.MockClient{
					MockGet: test.NewMockGetFn(nil),
					MockStatusUpdate: WantComposite(t, NewComposite(func(cr resource.Composite) {
						cr.SetCompositionReference(&corev1.ObjectReference{})
						cr.SetConditions(xpv1.ReconcileError(errors.Wrap(errBoom, errConfigure)))
					})),
				},
				opts: []ReconcilerOption{
					WithCompositeFinalizer(resource.NewNopFinalizer()),
					WithCompositionSelector(CompositionSelectorFn(func(_ context.Context, cr resource.Composite) error {
						cr.SetCompositionReference(&corev1.ObjectReference{})
						return nil
					})),
					WithCompositionRevisionFetcher(CompositionRevisionFetcherFn(func(_ context.Context, _ resource.Composite) (*v1.CompositionRevision, error) {
						return &v1.CompositionRevision{}, nil
					})),
					WithCompositionRevisionValidator(CompositionRevisionValidatorFn(func(_ *v1.CompositionRevision) error { return nil })),
					WithConfigurator(ConfiguratorFn(func(_ context.Context, _ resource.Composite, _ *v1.CompositionRevision) error {
						return errBoom
					})),
				},
			},
			want: want{
				r: reconcile.Result{Requeue: true},
			},
		},
		"ComposeResourcesError": {
			reason: "We should return any error encountered while composing resources.",
			args: args{
				client: &test.MockClient{
					MockGet: test.NewMockGetFn(nil),
					MockStatusUpdate: WantComposite(t, NewComposite(func(cr resource.Composite) {
						cr.SetCompositionReference(&corev1.ObjectReference{})
						cr.SetConditions(xpv1.ReconcileError(errors.Wrap(errBoom, errCompose)))
					})),
				},
				opts: []ReconcilerOption{
					WithCompositeFinalizer(resource.NewNopFinalizer()),
					WithCompositionSelector(CompositionSelectorFn(func(_ context.Context, cr resource.Composite) error {
						cr.SetCompositionReference(&corev1.ObjectReference{})
						return nil
					})),
					WithCompositionRevisionFetcher(CompositionRevisionFetcherFn(func(_ context.Context, _ resource.Composite) (*v1.CompositionRevision, error) {
						return &v1.CompositionRevision{}, nil
					})),
					WithCompositionRevisionValidator(CompositionRevisionValidatorFn(func(_ *v1.CompositionRevision) error { return nil })),
					WithConfigurator(ConfiguratorFn(func(_ context.Context, _ resource.Composite, _ *v1.CompositionRevision) error {
						return nil
					})),
					WithComposer(ComposerFn(func(_ context.Context, _ *composite.Unstructured, _ CompositionRequest) (CompositionResult, error) {
						return CompositionResult{}, errBoom
					})),
				},
			},
			want: want{
				r: reconcile.Result{Requeue: true},
			},
		},
		"PublishConnectionDetailsError": {
			reason: "We should return any error encountered while publishing connection details.",
			args: args{
				client: &test.MockClient{
					MockGet: test.NewMockGetFn(nil),
					MockStatusUpdate: WantComposite(t, NewComposite(func(cr resource.Composite) {
						cr.SetCompositionReference(&corev1.ObjectReference{})
						cr.SetConditions(xpv1.ReconcileError(errors.Wrap(errBoom, errPublish)))
					})),
				},
				opts: []ReconcilerOption{
					WithCompositeFinalizer(resource.NewNopFinalizer()),
					WithCompositionSelector(CompositionSelectorFn(func(_ context.Context, cr resource.Composite) error {
						cr.SetCompositionReference(&corev1.ObjectReference{})
						return nil
					})),
					WithCompositionRevisionFetcher(CompositionRevisionFetcherFn(func(_ context.Context, _ resource.Composite) (*v1.CompositionRevision, error) {
						return &v1.CompositionRevision{}, nil
					})),
					WithCompositionRevisionValidator(CompositionRevisionValidatorFn(func(_ *v1.CompositionRevision) error { return nil })),
					WithConfigurator(ConfiguratorFn(func(_ context.Context, _ resource.Composite, _ *v1.CompositionRevision) error {
						return nil
					})),
					WithComposer(ComposerFn(func(_ context.Context, _ *composite.Unstructured, _ CompositionRequest) (CompositionResult, error) {
						return CompositionResult{}, nil
					})),
					WithConnectionPublisher(managed.ConnectionPublisherFns{
						PublishConnectionFn: func(_ context.Context, _ store.SecretOwner, _ managed.ConnectionDetails) (published bool, err error) {
							return false, errBoom
						},
					}),
				},
			},
			want: want{
				r: reconcile.Result{Requeue: true},
			},
		},
		"CompositionWarnings": {
			reason: "We should not requeue if our Composer returned warning events.",
			args: args{
				client: &test.MockClient{
					MockGet: test.NewMockGetFn(nil),
					MockStatusUpdate: WantComposite(t, NewComposite(func(xr resource.Composite) {
						xr.SetCompositionReference(&corev1.ObjectReference{})
						xr.SetConditions(xpv1.ReconcileSuccess(), xpv1.Available())
					})),
				},
				opts: []ReconcilerOption{
					WithCompositeFinalizer(resource.NewNopFinalizer()),
					WithCompositionSelector(CompositionSelectorFn(func(_ context.Context, cr resource.Composite) error {
						cr.SetCompositionReference(&corev1.ObjectReference{})
						return nil
					})),
					WithCompositionRevisionFetcher(CompositionRevisionFetcherFn(func(_ context.Context, _ resource.Composite) (*v1.CompositionRevision, error) {
						c := &v1.CompositionRevision{Spec: v1.CompositionRevisionSpec{}}
						return c, nil
					})),
					WithCompositionRevisionValidator(CompositionRevisionValidatorFn(func(_ *v1.CompositionRevision) error { return nil })),
					WithConfigurator(ConfiguratorFn(func(_ context.Context, _ resource.Composite, _ *v1.CompositionRevision) error {
						return nil
					})),
					WithComposer(ComposerFn(func(_ context.Context, _ *composite.Unstructured, _ CompositionRequest) (CompositionResult, error) {
						return CompositionResult{
							Events: []event.Event{event.Warning("Warning", errBoom)},
						}, nil
					})),
					WithConnectionPublisher(managed.ConnectionPublisherFns{
						PublishConnectionFn: func(_ context.Context, _ store.SecretOwner, _ managed.ConnectionDetails) (published bool, err error) {
							return false, nil
						},
					}),
				},
			},
			want: want{
				r: reconcile.Result{Requeue: false},
			},
		},
		"ComposedResourcesNotReady": {
			reason: "We should requeue if any of our composed resources are not yet ready.",
			args: args{
				client: &test.MockClient{
					MockGet: test.NewMockGetFn(nil),
					MockStatusUpdate: WantComposite(t, NewComposite(func(cr resource.Composite) {
						cr.SetCompositionReference(&corev1.ObjectReference{})
						cr.SetConditions(xpv1.ReconcileSuccess(), xpv1.Creating().WithMessage("Unready resources: cat, cow, elephant, and 1 more"))
					})),
				},
				opts: []ReconcilerOption{
					WithCompositeFinalizer(resource.NewNopFinalizer()),
					WithCompositionSelector(CompositionSelectorFn(func(_ context.Context, cr resource.Composite) error {
						cr.SetCompositionReference(&corev1.ObjectReference{})
						return nil
					})),
					WithCompositionRevisionFetcher(CompositionRevisionFetcherFn(func(_ context.Context, _ resource.Composite) (*v1.CompositionRevision, error) {
						c := &v1.CompositionRevision{Spec: v1.CompositionRevisionSpec{}}
						return c, nil
					})),
					WithCompositionRevisionValidator(CompositionRevisionValidatorFn(func(_ *v1.CompositionRevision) error { return nil })),
					WithConfigurator(ConfiguratorFn(func(_ context.Context, _ resource.Composite, _ *v1.CompositionRevision) error {
						return nil
					})),
					WithComposer(ComposerFn(func(_ context.Context, _ *composite.Unstructured, _ CompositionRequest) (CompositionResult, error) {
						return CompositionResult{
							Composed: []ComposedResource{{
								ResourceName: "elephant",
								Ready:        false,
								Synced:       true,
							}, {
								ResourceName: "cow",
								Ready:        false,
								Synced:       true,
							}, {
								ResourceName: "pig",
								Ready:        true,
								Synced:       true,
							}, {
								ResourceName: "cat",
								Ready:        false,
								Synced:       true,
							}, {
								ResourceName: "dog",
								Ready:        true,
								Synced:       true,
							}, {
								ResourceName: "snake",
								Ready:        false,
								Synced:       true,
							}},
						}, nil
					})),
					WithConnectionPublisher(managed.ConnectionPublisherFns{
						PublishConnectionFn: func(_ context.Context, _ store.SecretOwner, _ managed.ConnectionDetails) (published bool, err error) {
							return false, nil
						},
					}),
				},
			},
			want: want{
				r: reconcile.Result{Requeue: true},
			},
		},
		"ComposedResourcesReady": {
			reason: "We should requeue after our poll interval if all of our composed resources are ready.",
			args: args{
				client: &test.MockClient{
					MockGet: WithComposite(t, NewComposite(func(cr resource.Composite) {
						cr.SetResourceReferences([]corev1.ObjectReference{{
							APIVersion: "example.org/v1",
							Kind:       "ComposedResource",
						}})
					})),
					MockStatusUpdate: WantComposite(t, NewComposite(func(cr resource.Composite) {
						cr.SetCompositionReference(&corev1.ObjectReference{})
						cr.SetResourceReferences([]corev1.ObjectReference{{
							APIVersion: "example.org/v1",
							Kind:       "ComposedResource",
						}})
						cr.SetConditions(xpv1.ReconcileSuccess(), xpv1.Available())
						cr.SetConnectionDetailsLastPublishedTime(&now)
					})),
				},
				opts: []ReconcilerOption{
					WithCompositeFinalizer(resource.NewNopFinalizer()),
					WithCompositionSelector(CompositionSelectorFn(func(_ context.Context, cr resource.Composite) error {
						cr.SetCompositionReference(&corev1.ObjectReference{})
						return nil
					})),
					WithCompositionRevisionFetcher(CompositionRevisionFetcherFn(func(_ context.Context, _ resource.Composite) (*v1.CompositionRevision, error) {
						c := &v1.CompositionRevision{Spec: v1.CompositionRevisionSpec{}}
						return c, nil
					})),
					WithCompositionRevisionValidator(CompositionRevisionValidatorFn(func(_ *v1.CompositionRevision) error { return nil })),
					WithConfigurator(ConfiguratorFn(func(_ context.Context, _ resource.Composite, _ *v1.CompositionRevision) error {
						return nil
					})),
					WithComposer(ComposerFn(func(_ context.Context, _ *composite.Unstructured, _ CompositionRequest) (CompositionResult, error) {
						return CompositionResult{ConnectionDetails: cd}, nil
					})),
					WithConnectionPublisher(managed.ConnectionPublisherFns{
						PublishConnectionFn: func(_ context.Context, _ store.SecretOwner, got managed.ConnectionDetails) (published bool, err error) {
							want := cd
							if diff := cmp.Diff(want, got); diff != "" {
								t.Errorf("PublishConnection(...): -want, +got:\n%s", diff)
							}
							return true, nil
						},
					}),
					WithWatchStarter("cool-controller", nil, WatchStarterFn(func(_ string, ws ...engine.Watch) error {
						cd := composed.New(composed.FromReference(corev1.ObjectReference{
							APIVersion: "example.org/v1",
							Kind:       "ComposedResource",
						}))
						want := []engine.Watch{engine.WatchFor(cd, engine.WatchTypeComposedResource, nil)}

						if diff := cmp.Diff(want, ws, cmp.AllowUnexported(engine.Watch{})); diff != "" {
							t.Errorf("StartWatches(...): -want, +got:\n%s", diff)
						}

						return nil
					})),
				},
			},
			want: want{
				r: reconcile.Result{Requeue: false},
			},
		},
		"ReconciliationPausedSuccessful": {
			reason: `If a composite resource has the pause annotation with value "true", there should be no further requeue requests.`,
			args: args{
				client: &test.MockClient{
					MockGet: WithComposite(t, NewComposite(func(cr resource.Composite) {
						cr.SetAnnotations(map[string]string{meta.AnnotationKeyReconciliationPaused: "true"})
					})),
					MockStatusUpdate: WantComposite(t, NewComposite(func(cr resource.Composite) {
						cr.SetAnnotations(map[string]string{meta.AnnotationKeyReconciliationPaused: "true"})
						cr.SetConditions(xpv1.ReconcilePaused().WithMessage(reconcilePausedMsg))
					})),
				},
			},
			want: want{
				r: reconcile.Result{},
			},
		},
		"ReconciliationPausedError": {
			reason: `If a composite resource has the pause annotation with value "true" and the status update due to reconciliation being paused fails, error should be reported causing an exponentially backed-off requeue.`,
			args: args{
				client: &test.MockClient{
					MockGet: WithComposite(t, NewComposite(func(cr resource.Composite) {
						cr.SetAnnotations(map[string]string{meta.AnnotationKeyReconciliationPaused: "true"})
					})),
					MockStatusUpdate: test.NewMockSubResourceUpdateFn(errBoom),
				},
			},
			want: want{
				err: errors.Wrap(errBoom, errUpdateStatus),
			},
		},
		"ReconciliationResumes": {
			reason: `If a composite resource has the pause annotation with some value other than "true" and the Synced=False/ReconcilePaused status condition, reconciliation should resume with requeueing.`,
			args: args{
				client: &test.MockClient{
					MockGet: WithComposite(t, NewComposite(func(cr resource.Composite) {
						cr.SetAnnotations(map[string]string{meta.AnnotationKeyReconciliationPaused: ""})
						cr.SetConditions(xpv1.ReconcilePaused())
					})),
					MockStatusUpdate: WantComposite(t, NewComposite(func(cr resource.Composite) {
						cr.SetAnnotations(map[string]string{meta.AnnotationKeyReconciliationPaused: ""})
						cr.SetConditions(xpv1.ReconcileSuccess(), xpv1.Available())
						cr.SetConnectionDetailsLastPublishedTime(&now)
						cr.SetCompositionReference(&corev1.ObjectReference{})
					})),
				},
				opts: []ReconcilerOption{
					WithCompositeFinalizer(resource.NewNopFinalizer()),
					WithCompositionSelector(CompositionSelectorFn(func(_ context.Context, cr resource.Composite) error {
						cr.SetCompositionReference(&corev1.ObjectReference{})
						return nil
					})),
					WithCompositionRevisionFetcher(CompositionRevisionFetcherFn(func(_ context.Context, _ resource.Composite) (*v1.CompositionRevision, error) {
						c := &v1.CompositionRevision{Spec: v1.CompositionRevisionSpec{}}
						return c, nil
					})),
					WithCompositionRevisionValidator(CompositionRevisionValidatorFn(func(_ *v1.CompositionRevision) error { return nil })),
					WithConfigurator(ConfiguratorFn(func(_ context.Context, _ resource.Composite, _ *v1.CompositionRevision) error {
						return nil
					})),
					WithComposer(ComposerFn(func(_ context.Context, _ *composite.Unstructured, _ CompositionRequest) (CompositionResult, error) {
						return CompositionResult{}, nil
					})),
					WithConnectionPublisher(managed.ConnectionPublisherFns{
						PublishConnectionFn: func(_ context.Context, _ store.SecretOwner, _ managed.ConnectionDetails) (published bool, err error) {
							return true, nil
						},
					}),
				},
			},
			want: want{
				r: reconcile.Result{Requeue: false},
			},
		},
		"ReconciliationResumesAfterAnnotationRemoval": {
			reason: `If a composite resource has the pause annotation removed and the Synced=False/ReconcilePaused status condition, reconciliation should resume with requeueing.`,
			args: args{
				client: &test.MockClient{
					MockGet: WithComposite(t, NewComposite(func(cr resource.Composite) {
						// no annotation atm
						// (but reconciliations were already paused)
						cr.SetConditions(xpv1.ReconcilePaused())
					})),
					MockStatusUpdate: WantComposite(t, NewComposite(func(cr resource.Composite) {
						cr.SetConditions(xpv1.ReconcileSuccess(), xpv1.Available())
						cr.SetConnectionDetailsLastPublishedTime(&now)
						cr.SetCompositionReference(&corev1.ObjectReference{})
					})),
				},
				opts: []ReconcilerOption{
					WithCompositeFinalizer(resource.NewNopFinalizer()),
					WithCompositionSelector(CompositionSelectorFn(func(_ context.Context, cr resource.Composite) error {
						cr.SetCompositionReference(&corev1.ObjectReference{})
						return nil
					})),
					WithCompositionRevisionFetcher(CompositionRevisionFetcherFn(func(_ context.Context, _ resource.Composite) (*v1.CompositionRevision, error) {
						c := &v1.CompositionRevision{Spec: v1.CompositionRevisionSpec{}}
						return c, nil
					})),
					WithCompositionRevisionValidator(CompositionRevisionValidatorFn(func(_ *v1.CompositionRevision) error { return nil })),
					WithConfigurator(ConfiguratorFn(func(_ context.Context, _ resource.Composite, _ *v1.CompositionRevision) error {
						return nil
					})),
					WithComposer(ComposerFn(func(_ context.Context, _ *composite.Unstructured, _ CompositionRequest) (CompositionResult, error) {
						return CompositionResult{}, nil
					})),
					WithConnectionPublisher(managed.ConnectionPublisherFns{
						PublishConnectionFn: func(_ context.Context, _ store.SecretOwner, _ managed.ConnectionDetails) (published bool, err error) {
							return true, nil
						},
					}),
				},
			},
			want: want{
				r: reconcile.Result{Requeue: false},
			},
		},
		"CustomEventsAndConditions": {
			reason: "We should emit custom events and set custom conditions that were returned by the composer on both the composite resource and the claim.",
			args: args{
				client: &test.MockClient{
					MockGet: test.NewMockGetFn(nil),
					MockStatusUpdate: WantComposite(t, NewComposite(func(cr resource.Composite) {
						cr.SetCompositionReference(&corev1.ObjectReference{})
						cr.SetConditions(
							xpv1.Condition{
								Type:    "DatabaseReady",
								Status:  corev1.ConditionTrue,
								Reason:  "Available",
								Message: "This is a condition for database availability.",
							},
							xpv1.Condition{
								Type:               "InternalSync",
								Status:             corev1.ConditionTrue,
								LastTransitionTime: metav1.Time{},
								Reason:             "SyncSuccess",
								Message:            "This is a condition representing an internal sync process.",
								ObservedGeneration: 0,
							},
							xpv1.ReconcileSuccess(),
							xpv1.Available(),
						)
					})),
				},
				opts: []ReconcilerOption{
					WithRecorder(newTestRecorder(
						event.Event{
							Type:        event.Type(corev1.EventTypeNormal),
							Reason:      "SelectComposition",
							Message:     "Successfully selected composition: ",
							Annotations: map[string]string{},
						},
						event.Event{
							Type:        event.TypeNormal,
							Reason:      "DatabaseAvailable",
							Message:     "Pipeline step \"some-function\": This is an event for database availability.",
							Annotations: map[string]string{},
						},
						event.Event{
							Type:        event.TypeNormal,
							Reason:      "SyncSuccess",
							Message:     "Pipeline step \"some-function\": Internal sync was successful.",
							Annotations: map[string]string{},
						},
						event.Event{
							Type:        event.Type(corev1.EventTypeNormal),
							Reason:      "ComposeResources",
							Message:     "Successfully composed resources",
							Annotations: map[string]string{},
						},
					)),
					WithCompositeFinalizer(resource.NewNopFinalizer()),
					WithCompositionSelector(CompositionSelectorFn(func(_ context.Context, cr resource.Composite) error {
						cr.SetCompositionReference(&corev1.ObjectReference{})
						return nil
					})),
					WithCompositionRevisionFetcher(CompositionRevisionFetcherFn(func(_ context.Context, _ resource.Composite) (*v1.CompositionRevision, error) {
						return &v1.CompositionRevision{}, nil
					})),
					WithCompositionRevisionValidator(CompositionRevisionValidatorFn(func(_ *v1.CompositionRevision) error { return nil })),
					WithConfigurator(ConfiguratorFn(func(_ context.Context, _ resource.Composite, _ *v1.CompositionRevision) error {
						return nil
					})),
					WithComposer(ComposerFn(func(_ context.Context, _ *composite.Unstructured, _ CompositionRequest) (CompositionResult, error) {
						return CompositionResult{
							Composed:          []ComposedResource{},
							ConnectionDetails: cd,
							Events: []event.Event{
								{
									Type:    event.TypeNormal,
									Reason:  "DatabaseAvailable",
									Message: "Pipeline step \"some-function\": This is an event for database availability.",
								},
								{
									Type:    event.TypeNormal,
									Reason:  "SyncSuccess",
									Message: "Pipeline step \"some-function\": Internal sync was successful.",
								},
							},
							Conditions: []xpv1.Condition{
								{
									Type:    "DatabaseReady",
									Status:  corev1.ConditionTrue,
									Reason:  "Available",
									Message: "This is a condition for database availability.",
								},
								{
									Type:               "InternalSync",
									Status:             corev1.ConditionTrue,
									LastTransitionTime: metav1.Time{},
									Reason:             "SyncSuccess",
									Message:            "This is a condition representing an internal sync process.",
									ObservedGeneration: 0,
								},
							},
						}, nil
					})),
				},
			},
			want: want{
				r: reconcile.Result{Requeue: false},
			},
		},
		"CustomEventsAndConditionFatal": {
			reason: "In the case of a fatal result from the composer, we should set all custom conditions that were seen. If any custom conditions were not seen, they should be marked as Unknown. The error message should be emitted as an event to the composite.",
			args: args{
				client: &test.MockClient{
					MockGet: test.NewMockGetFn(nil, func(obj client.Object) error {
						if xr, ok := obj.(*composite.Unstructured); ok {
							// non-nil claim ref to trigger claim Get()
							xr.SetConditions(xpv1.Condition{
								Type:    "DatabaseReady",
								Status:  corev1.ConditionTrue,
								Reason:  "Available",
								Message: "This is a condition for database availability.",
							})
						}
						return nil
					}),
					MockStatusUpdate: WantComposite(t, NewComposite(func(cr resource.Composite) {
						cr.SetCompositionReference(&corev1.ObjectReference{})

						cr.SetConditions(
							xpv1.Condition{
								Type:    "DatabaseReady",
								Status:  corev1.ConditionUnknown,
								Reason:  "FatalError",
								Message: "A fatal error occurred before the status of this condition could be determined.",
							},
							xpv1.ReconcileError(fmt.Errorf("cannot compose resources: %w", errBoom)),
							xpv1.Condition{
								Type:               "InternalSync",
								Status:             corev1.ConditionTrue,
								LastTransitionTime: metav1.Time{},
								Reason:             "SyncSuccess",
								Message:            "This is a condition representing an internal sync process.",
								ObservedGeneration: 0,
							},
							xpv1.Condition{
								Type:               "BucketReady",
								Status:             corev1.ConditionTrue,
								LastTransitionTime: metav1.Time{},
								Reason:             "Available",
								Message:            "This is a condition for bucket availability.",
								ObservedGeneration: 0,
							},
						)
					})),
				},
				opts: []ReconcilerOption{
					WithRecorder(newTestRecorder(
						event.Event{
							Type:    event.Type(corev1.EventTypeNormal),
							Reason:  "SelectComposition",
							Message: "Successfully selected composition: ",
						},
						event.Warning("ComposeResources", fmt.Errorf("cannot compose resources: %w", errBoom)),
						event.Event{
							Type:    event.TypeNormal,
							Reason:  "DatabaseAvailable",
							Message: "Pipeline step \"some-function\": This is an event for database availability.",
						},
						event.Event{
							Type:    event.TypeNormal,
							Reason:  "SyncSuccess",
							Message: "Pipeline step \"some-function\": Internal sync was successful.",
						},
					)),
					WithCompositeFinalizer(resource.NewNopFinalizer()),
					WithCompositionSelector(CompositionSelectorFn(func(_ context.Context, cr resource.Composite) error {
						cr.SetCompositionReference(&corev1.ObjectReference{})
						return nil
					})),
					WithCompositionRevisionFetcher(CompositionRevisionFetcherFn(func(_ context.Context, _ resource.Composite) (*v1.CompositionRevision, error) {
						return &v1.CompositionRevision{}, nil
					})),
					WithCompositionRevisionValidator(CompositionRevisionValidatorFn(func(_ *v1.CompositionRevision) error { return nil })),
					WithConfigurator(ConfiguratorFn(func(_ context.Context, _ resource.Composite, _ *v1.CompositionRevision) error {
						return nil
					})),
					WithComposer(ComposerFn(func(_ context.Context, _ *composite.Unstructured, _ CompositionRequest) (CompositionResult, error) {
						return CompositionResult{
							Composed:          []ComposedResource{},
							ConnectionDetails: cd,
							Events: []event.Event{
								{
									Type:    event.TypeNormal,
									Reason:  "DatabaseAvailable",
									Message: "Pipeline step \"some-function\": This is an event for database availability.",
								},
								{
									Type:    event.TypeNormal,
									Reason:  "SyncSuccess",
									Message: "Pipeline step \"some-function\": Internal sync was successful.",
								},
							},
							Conditions: []xpv1.Condition{
								{
									Type:               "InternalSync",
									Status:             corev1.ConditionTrue,
									LastTransitionTime: metav1.Time{},
									Reason:             "SyncSuccess",
									Message:            "This is a condition representing an internal sync process.",
									ObservedGeneration: 0,
								},
								{
									Type:               "BucketReady",
									Status:             corev1.ConditionTrue,
									LastTransitionTime: metav1.Time{},
									Reason:             "Available",
									Message:            "This is a condition for bucket availability.",
									ObservedGeneration: 0,
								},
							},
						}, errBoom
					})),
				},
			},
			want: want{
				r: reconcile.Result{Requeue: true},
			},
		},
		"CustomConditionUpdate": {
			reason: "Custom conditions should be updated if they already exist. Additionally, if a condition already exists in the status but was not included in the response, it should remain in the status.",
			args: args{
				client: &test.MockClient{
					MockGet: test.NewMockGetFn(nil, func(obj client.Object) error {
						if xr, ok := obj.(*composite.Unstructured); ok {
							// The database condition already exists on the XR.
							xr.SetConditions(xpv1.Condition{
								Type:    "DatabaseReady",
								Status:  corev1.ConditionTrue,
								Reason:  "Available",
								Message: "This is a condition for database availability.",
							})
							// The bucket began in a non-ready state.
							xr.SetConditions(xpv1.Condition{
								Type:    "BucketReady",
								Status:  corev1.ConditionFalse,
								Reason:  "Creating",
								Message: "Waiting for bucket to be created.",
							})

							return nil
						}
						if cm, ok := obj.(*claim.Unstructured); ok {
							claim.New(claim.WithGroupVersionKind(schema.GroupVersionKind{})).DeepCopyInto(cm)
							return nil
						}
						return nil
					}),
					MockStatusUpdate: WantComposite(t, NewComposite(func(cr resource.Composite) {
						cr.SetCompositionReference(&corev1.ObjectReference{})
						cr.SetConditions(
							// The database condition should exist even though it was not seen
							// during this reconcile.
							xpv1.Condition{
								Type:    "DatabaseReady",
								Status:  corev1.ConditionTrue,
								Reason:  "Available",
								Message: "This is a condition for database availability.",
							},
							// The bucket condition should be updated to reflect the latest
							// condition which is available.
							xpv1.Condition{
								Type:               "BucketReady",
								Status:             corev1.ConditionTrue,
								LastTransitionTime: metav1.Time{},
								Reason:             "Available",
								Message:            "This is a condition for bucket availability.",
								ObservedGeneration: 0,
							},
							xpv1.Condition{
								Type:               "InternalSync",
								Status:             corev1.ConditionTrue,
								LastTransitionTime: metav1.Time{},
								Reason:             "SyncSuccess",
								Message:            "This is a condition representing an internal sync process.",
								ObservedGeneration: 0,
							},
							xpv1.ReconcileSuccess(),
							xpv1.Available(),
						)
					})),
				},
				opts: []ReconcilerOption{
					WithRecorder(newTestRecorder(
						event.Event{
							Type:        event.Type(corev1.EventTypeNormal),
							Reason:      "SelectComposition",
							Message:     "Successfully selected composition: ",
							Annotations: map[string]string{},
						},
						event.Event{
							Type:        event.Type(corev1.EventTypeNormal),
							Reason:      "ComposeResources",
							Message:     "Successfully composed resources",
							Annotations: map[string]string{},
						},
					)),
					WithCompositeFinalizer(resource.NewNopFinalizer()),
					WithCompositionSelector(CompositionSelectorFn(func(_ context.Context, cr resource.Composite) error {
						cr.SetCompositionReference(&corev1.ObjectReference{})
						return nil
					})),
					WithCompositionRevisionFetcher(CompositionRevisionFetcherFn(func(_ context.Context, _ resource.Composite) (*v1.CompositionRevision, error) {
						return &v1.CompositionRevision{}, nil
					})),
					WithCompositionRevisionValidator(CompositionRevisionValidatorFn(func(_ *v1.CompositionRevision) error { return nil })),
					WithConfigurator(ConfiguratorFn(func(_ context.Context, _ resource.Composite, _ *v1.CompositionRevision) error {
						return nil
					})),
					WithComposer(ComposerFn(func(_ context.Context, _ *composite.Unstructured, _ CompositionRequest) (CompositionResult, error) {
						return CompositionResult{
							Composed:          []ComposedResource{},
							ConnectionDetails: cd,
							Conditions: []xpv1.Condition{
								// The database condition is not added to the XR again.
								{
									Type:               "InternalSync",
									Status:             corev1.ConditionTrue,
									LastTransitionTime: metav1.Time{},
									Reason:             "SyncSuccess",
									Message:            "This is a condition representing an internal sync process.",
									ObservedGeneration: 0,
								},
								// The bucket is now ready.
								{
									Type:               "BucketReady",
									Status:             corev1.ConditionTrue,
									LastTransitionTime: metav1.Time{},
									Reason:             "Available",
									Message:            "This is a condition for bucket availability.",
									ObservedGeneration: 0,
								},
							},
						}, nil
					})),
				},
			},
			want: want{
				r: reconcile.Result{Requeue: false},
			},
		},
		"SystemConditionUpdate": {
			reason: "A system condition should be updated if it is explicitly allowed to do so",
			args: args{
				client: &test.MockClient{
					MockGet: test.NewMockGetFn(nil),
					MockStatusUpdate: WantComposite(t, NewComposite(func(cr resource.Composite) {
						cr.SetCompositionReference(&corev1.ObjectReference{})
						cr.SetConditions(
							xpv1.ReconcileSuccess(),
							xpv1.Creating().WithMessage("Composite resource was explicitly marked as unready by the composer"),
						)
					})),
				},
				opts: []ReconcilerOption{
					WithRecorder(newTestRecorder(
						event.Event{
							Type:        event.Type(corev1.EventTypeNormal),
							Reason:      "SelectComposition",
							Message:     "Successfully selected composition: ",
							Annotations: map[string]string{},
						},
						event.Event{
							Type:        event.Type(corev1.EventTypeNormal),
							Reason:      "ComposeResources",
							Message:     "Successfully composed resources",
							Annotations: map[string]string{},
						},
					)),
					WithCompositeFinalizer(resource.NewNopFinalizer()),
					WithCompositionSelector(CompositionSelectorFn(func(_ context.Context, cr resource.Composite) error {
						cr.SetCompositionReference(&corev1.ObjectReference{})
						return nil
					})),
					WithCompositionRevisionFetcher(CompositionRevisionFetcherFn(func(_ context.Context, _ resource.Composite) (*v1.CompositionRevision, error) {
						return &v1.CompositionRevision{}, nil
					})),
					WithCompositionRevisionValidator(CompositionRevisionValidatorFn(func(_ *v1.CompositionRevision) error { return nil })),
					WithConfigurator(ConfiguratorFn(func(_ context.Context, _ resource.Composite, _ *v1.CompositionRevision) error {
						return nil
					})),
					WithComposer(ComposerFn(func(_ context.Context, _ *composite.Unstructured, _ CompositionRequest) (CompositionResult, error) {
						return CompositionResult{
							Composite: CompositeResource{
								Ready: ptr.To(false),
							},
							ConnectionDetails: cd,
						}, nil
					})),
				},
			},
			want: want{
				r: reconcile.Result{Requeue: false},
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := NewReconciler(tc.args.client, tc.args.of, tc.args.opts...)
			got, err := r.Reconcile(context.Background(), reconcile.Request{})

			if diff := cmp.Diff(tc.want.err, err, test.EquateErrors()); diff != "" {
				t.Errorf("\n%s\nr.Reconcile(...): -want error, +got error:\n%s", tc.reason, diff)
			}
			if diff := cmp.Diff(tc.want.r, got, test.EquateErrors()); diff != "" {
				t.Errorf("\n%s\nr.Reconcile(...): -want, +got:\n%s", tc.reason, diff)
			}

			if tr, ok := r.record.(*testRecorder); ok {
				if diff := cmp.Diff(tr.Want, tr.Got, test.EquateErrors(), cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("\n%s\nr.Reconcile(...): -want events, +got events:\n%s", tc.reason, diff)
				}
			}
		})
	}
}

type CompositeModifier func(cr resource.Composite)

func NewComposite(m ...CompositeModifier) *composite.Unstructured {
	cr := composite.New(composite.WithGroupVersionKind(schema.GroupVersionKind{}))
	for _, fn := range m {
		fn(cr)
	}
	return cr
}

// A get function that supplies the input XR.
func WithComposite(_ *testing.T, cr *composite.Unstructured) func(_ context.Context, _ client.ObjectKey, obj client.Object) error {
	return func(_ context.Context, _ client.ObjectKey, obj client.Object) error {
		if o, ok := obj.(*composite.Unstructured); ok {
			*o = *cr
		}
		return nil
	}
}

// A status update function that ensures the supplied object is the XR we want.
func WantComposite(t *testing.T, want resource.Composite) func(_ context.Context, obj client.Object, _ ...client.SubResourceUpdateOption) error {
	t.Helper()
	return func(_ context.Context, got client.Object, _ ...client.SubResourceUpdateOption) error {
		t.Helper()
		// Normally we use a custom Equal method on conditions to ignore the
		// lastTransitionTime, but we may be using unstructured types here where
		// the conditions are just a map[string]any.
		diff := cmp.Diff(want, got, cmpopts.AcyclicTransformer("StringToTime", func(s string) any {
			ts, err := time.Parse(time.RFC3339, s)
			if err != nil {
				return s
			}
			return ts
		}), cmpopts.EquateApproxTime(3*time.Second))
		if diff != "" {
			t.Errorf("WantComposite(...): -want, +got: %s", diff)
		}
		return nil
	}
}

// testRecorder allows asserting event creation.
type testRecorder struct {
	Want []event.Event
	Got  []event.Event
}

func (r *testRecorder) Event(_ runtime.Object, e event.Event) {
	r.Got = append(r.Got, e)
}

func (r *testRecorder) WithAnnotations(_ ...string) event.Recorder {
	return r
}

func newTestRecorder(expected ...event.Event) *testRecorder {
	return &testRecorder{
		Want: expected,
	}
}
