/*
Copyright 2025 The Crossplane Authors.

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
	"math/rand"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	xpv1 "github.com/crossplane/crossplane-runtime/apis/common/v1"
	"github.com/crossplane/crossplane-runtime/pkg/errors"
	"github.com/crossplane/crossplane-runtime/pkg/resource"

	v1 "github.com/crossplane/crossplane/apis/apiextensions/v1"
)

// A RevisionFetcher selects the appropriate CompositionRevision for a composite
// resource and fetches it, and returns it as a Composition.
type RevisionFetcher struct {
	client client.Client
	xrd    *v1.CompositeResourceDefinition
}

// NewRevisionFetcher returns a RevisionFetcher that fetches the Revision
// referenced by a composite resource.
func NewRevisionFetcher(c client.Client, xrd *v1.CompositeResourceDefinition) *RevisionFetcher {
	return &RevisionFetcher{client: c, xrd: xrd}
}

// Fetch the appropriate CompositionRevision for the supplied XR.
func (f *RevisionFetcher) Fetch(ctx context.Context, xr resource.Composite) (*v1.CompositionRevision, error) {
	legacy := ptr.Deref(f.xrd.Spec.Scope, v1.CompositeResourceScopeLegacyCluster) == v1.CompositeResourceScopeLegacyCluster
	labels := ptr.Deref(xr.GetCompositionRevisionSelector(), metav1.LabelSelector{}).MatchLabels

	// TODO(negz): Handle default and enforced compositions.

	if legacy {
		if ref := xr.GetCompositionReference(); ref == nil {
			cl := &v1.CompositionList{}
			l := ptr.Deref(xr.GetCompositionSelector(), metav1.LabelSelector{}).MatchLabels
			if err := f.client.List(ctx, cl, client.MatchingLabels(l)); err != nil {
				return nil, errors.Wrap(err, "cannot list Compositions")
			}

			// Filter out any compositions that aren't compatible
			// with this kind of XR.
			candidates := make([]v1.Composition, 0)
			for _, comp := range cl.Items {
				if comp.Spec.CompositeTypeRef.APIVersion != xr.GetObjectKind().GroupVersionKind().GroupVersion().String() {
					continue
				}
				if comp.Spec.CompositeTypeRef.Kind != xr.GetObjectKind().GroupVersionKind().Kind {
					continue
				}

				candidates = append(candidates, comp)
			}
			if len(candidates) == 0 {
				return nil, errors.New("couldn't find any suitable Compositions")
			}
			random := rand.New(rand.NewSource(time.Now().UnixNano())) //nolint:gosec // We don't need this to be cryptographically random.
			selected := candidates[random.Intn(len(candidates))]
			xr.SetCompositionReference(&corev1.ObjectReference{Name: selected.GetName()})
		}

		// Use the composition's name to select revisions later.
		labels[v1.LabelCompositionName] = xr.GetCompositionReference().Name

		// If the XR already has a revision ref, just return it.
		ref := xr.GetCompositionRevisionReference()
		pol := ptr.Deref(xr.GetCompositionUpdatePolicy(), xpv1.UpdateAutomatic)
		if ref != nil && pol == xpv1.UpdateManual {
			rev := &v1.CompositionRevision{}
			return rev, errors.Wrapf(f.client.Get(ctx, client.ObjectKey{Name: ref.Name}, rev), "cannot get CompositionRevision %q", ref.Name)
		}
	}

	// At this point we're either a modern XR that only uses a composition
	// revision selector or a legacy XR with an unresolved composition
	// revision ref.

	crl := &v1.CompositionRevisionList{}
	if err := f.client.List(ctx, crl, client.MatchingLabels(labels)); err != nil {
		return nil, errors.Wrap(err, "cannot list CompositionRevisions")
	}

	// Filter out any revisions that aren't compatible with this kind of XR.
	candidates := make([]v1.CompositionRevision, 0)
	for _, rev := range crl.Items {
		if rev.Spec.CompositeTypeRef.APIVersion != xr.GetObjectKind().GroupVersionKind().GroupVersion().String() {
			continue
		}
		if rev.Spec.CompositeTypeRef.Kind != xr.GetObjectKind().GroupVersionKind().Kind {
			continue
		}

		candidates = append(candidates, rev)
	}

	if len(candidates) == 0 {
		return nil, errors.New("couldn't find any suitable CompositionRevisions")
	}

	// TODO(negz): If this is a modern XR without a composition name label
	// in its revision selector the candidate revisions could be revisions
	// of multiple different compositions. Do we really want to potentially
	// switch between different compositions if the selector wasn't
	// specific? Or should we pin one by adding it to the labels...

	slices.SortStableFunc(candidates, func(a, b v1.CompositionRevision) int {
		if a.Spec.Revision == b.Spec.Revision {
			return 0
		}
		// We want to sort highest to lowest.
		if a.Spec.Revision < b.Spec.Revision {
			return 1
		}
		return -1
	})

	selected := candidates[0]

	if legacy {
		xr.SetCompositionRevisionReference(&corev1.LocalObjectReference{Name: selected.GetName()})
	} else {
		// TODO(negz): Save revision in status?
		labels[v1.LabelCompositionName] = selected.GetLabels()[v1.LabelCompositionName]
	}

	return nil, nil
}
