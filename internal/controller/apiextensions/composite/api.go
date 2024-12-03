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
	"math/rand"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	xpv1 "github.com/crossplane/crossplane-runtime/apis/common/v1"
	"github.com/crossplane/crossplane-runtime/pkg/errors"
	"github.com/crossplane/crossplane-runtime/pkg/event"
	"github.com/crossplane/crossplane-runtime/pkg/meta"
	"github.com/crossplane/crossplane-runtime/pkg/resource"

	v1 "github.com/crossplane/crossplane/apis/apiextensions/v1"
	"github.com/crossplane/crossplane/internal/xcrd"
)

// Error strings.
const (
	errNoCompatibleComposition         = "no compatible Compositions found"
	errNoCompatibleCompositionRevision = "no compatible CompositionRevisions found"
	errGetComposition                  = "cannot get Composition"
	errGetCompositionRevision          = "cannot get CompositionRevision"
	errListCompositions                = "cannot list Compositions"
	errListCompositionRevisions        = "cannot list CompositionRevisions"
	errUpdateComposite                 = "cannot update composite resource"
	errCompositionNotCompatible        = "referenced composition is not compatible with this composite resource"
	errGetXRD                          = "cannot get composite resource definition"
	errFetchCompositionRevision        = "cannot fetch composition revision"
)

// Event reasons.
const (
	reasonCompositionSelection    event.Reason = "CompositionSelection"
	reasonCompositionUpdatePolicy event.Reason = "CompositionUpdatePolicy"
)

// An APIRevisionFetcher selects the appropriate CompositionRevision for a
// composite resource, fetches it, and returns it as a Composition. This is done
// for compatibility with existing Composition logic while CompositionRevisions
// are in alpha.
type APIRevisionFetcher struct {
	ca resource.ClientApplicator
}

// NewAPIRevisionFetcher returns a RevisionFetcher that fetches the
// Revision referenced by a composite resource.
func NewAPIRevisionFetcher(ca resource.ClientApplicator) *APIRevisionFetcher {
	return &APIRevisionFetcher{ca: ca}
}

// Fetch the appropriate CompositionRevision for the supplied XR. Panics if the
// composite resource's composition reference is nil, but handles setting the
// composition revision reference.
func (f *APIRevisionFetcher) Fetch(ctx context.Context, cr resource.Composite) (*v1.CompositionRevision, error) {
	current := cr.GetCompositionRevisionReference()
	pol := cr.GetCompositionUpdatePolicy()

	// We've already selected a revision, and our update policy is manual.
	// Just fetch and return the selected revision.
	if current != nil && pol != nil && *pol == xpv1.UpdateManual {
		rev := &v1.CompositionRevision{}
		err := f.ca.Get(ctx, types.NamespacedName{Name: current.Name}, rev)
		return rev, errors.Wrap(err, errGetCompositionRevision)
	}

	// We either haven't yet selected a revision, or our update policy is
	// automatic. Either way we need to determine the latest revision.

	comp := &v1.Composition{}
	if err := f.ca.Get(ctx, meta.NamespacedNameOf(cr.GetCompositionReference()), comp); err != nil {
		return nil, errors.Wrap(err, errGetComposition)
	}

	rl, err := f.getCompositionRevisionList(ctx, cr, comp)
	if err != nil {
		return nil, errors.Wrap(err, errFetchCompositionRevision)
	}

	latest := v1.LatestRevision(comp, rl.Items)
	if latest == nil {
		return nil, errors.New(errNoCompatibleCompositionRevision)
	}

	if current == nil || current.Name != latest.GetName() {
		cr.SetCompositionRevisionReference(&corev1.LocalObjectReference{Name: latest.GetName()})
		if err := f.ca.Apply(ctx, cr); err != nil {
			return nil, errors.Wrap(err, errUpdate)
		}
	}

	return latest, nil
}

func (f *APIRevisionFetcher) getCompositionRevisionList(ctx context.Context, cr resource.Composite, comp *v1.Composition) (*v1.CompositionRevisionList, error) {
	rl := &v1.CompositionRevisionList{}
	ml := client.MatchingLabels{}

	if cr.GetCompositionUpdatePolicy() != nil && *cr.GetCompositionUpdatePolicy() == xpv1.UpdateAutomatic &&
		cr.GetCompositionRevisionSelector() != nil {
		ml = cr.GetCompositionRevisionSelector().MatchLabels
	}

	ml[v1.LabelCompositionName] = comp.GetName()
	if err := f.ca.List(ctx, rl, ml); err != nil {
		return nil, errors.Wrap(err, errListCompositionRevisions)
	}
	return rl, nil
}

// NewCompositionSelectorChain returns a new CompositionSelectorChain.
func NewCompositionSelectorChain(list ...CompositionSelector) *CompositionSelectorChain {
	return &CompositionSelectorChain{list: list}
}

// CompositionSelectorChain calls the given list of CompositionSelectors in order.
type CompositionSelectorChain struct {
	list []CompositionSelector
}

// SelectComposition calls all SelectComposition functions of CompositionSelectors
// in the list.
func (r *CompositionSelectorChain) SelectComposition(ctx context.Context, cp resource.Composite) error {
	for _, cs := range r.list {
		if err := cs.SelectComposition(ctx, cp); err != nil {
			return err
		}
	}
	return nil
}

// NewAPILabelSelectorResolver returns a SelectorResolver for composite resource.
func NewAPILabelSelectorResolver(c client.Client) *APILabelSelectorResolver {
	return &APILabelSelectorResolver{client: c}
}

// APILabelSelectorResolver is used to resolve the composition selector on the instance
// to composition reference.
type APILabelSelectorResolver struct {
	client client.Client
}

// SelectComposition resolves selector to a reference if it doesn't exist.
func (r *APILabelSelectorResolver) SelectComposition(ctx context.Context, cp resource.Composite) error {
	// TODO(muvaf): need to block the deletion of composition via finalizer once
	// it's selected since it's integral to this resource.
	// TODO(muvaf): We don't rely on UID in practice. It should not be there
	// because it will make confusion if the resource is backed up and restored
	// to another cluster
	if cp.GetCompositionReference() != nil {
		return nil
	}
	labels := map[string]string{}
	sel := cp.GetCompositionSelector()
	if sel != nil {
		labels = sel.MatchLabels
	}
	list := &v1.CompositionList{}
	if err := r.client.List(ctx, list, client.MatchingLabels(labels)); err != nil {
		return errors.Wrap(err, errListCompositions)
	}

	candidates := make([]string, 0, len(list.Items))
	v, k := cp.GetObjectKind().GroupVersionKind().ToAPIVersionAndKind()

	for _, comp := range list.Items {
		if comp.Spec.CompositeTypeRef.APIVersion == v && comp.Spec.CompositeTypeRef.Kind == k {
			// This composition is compatible with our composite resource.
			candidates = append(candidates, comp.Name)
		}
	}

	if len(candidates) == 0 {
		return errors.New(errNoCompatibleComposition)
	}

	random := rand.New(rand.NewSource(time.Now().UnixNano())) //nolint:gosec // We don't need this to be cryptographically random.
	selected := candidates[random.Intn(len(candidates))]
	cp.SetCompositionReference(&corev1.ObjectReference{Name: selected})
	return errors.Wrap(r.client.Update(ctx, cp), errUpdateComposite)
}

// NewAPIDefaultCompositionSelector returns a APIDefaultCompositionSelector.
func NewAPIDefaultCompositionSelector(c client.Client, name string, r event.Recorder) *APIDefaultCompositionSelector {
	return &APIDefaultCompositionSelector{client: c, name: name, recorder: r}
}

// APIDefaultCompositionSelector selects the default composition referenced in
// the definition of the resource if neither a reference nor selector is given
// in composite resource.
type APIDefaultCompositionSelector struct {
	client   client.Client
	name     string
	recorder event.Recorder
}

// SelectComposition selects the default compositionif neither a reference nor
// selector is given in composite resource.
func (s *APIDefaultCompositionSelector) SelectComposition(ctx context.Context, xr resource.Composite) error {
	if xr.GetCompositionReference() != nil || xr.GetCompositionSelector() != nil {
		return nil
	}
	xrd := &v1.CompositeResourceDefinition{}
	if err := s.client.Get(ctx, types.NamespacedName{Name: s.name}, xrd); err != nil {
		return errors.Wrap(err, errGetXRD)
	}
	if xrd.Spec.DefaultCompositionRef == nil {
		return nil
	}
	xr.SetCompositionReference(&corev1.ObjectReference{Name: xrd.Spec.DefaultCompositionRef.Name})
	s.recorder.Event(xr, event.Normal(reasonCompositionSelection, "Default composition has been selected"))
	return nil
}

// NewEnforcedCompositionSelector returns a EnforcedCompositionSelector.
func NewEnforcedCompositionSelector(def v1.CompositeResourceDefinition, r event.Recorder) *EnforcedCompositionSelector {
	return &EnforcedCompositionSelector{def: def, recorder: r}
}

// EnforcedCompositionSelector , if it's given, selects the enforced composition
// on the definition for all composite instances.
type EnforcedCompositionSelector struct {
	def      v1.CompositeResourceDefinition
	recorder event.Recorder
}

// SelectComposition selects the enforced composition if it's given in definition.
func (s *EnforcedCompositionSelector) SelectComposition(_ context.Context, cp resource.Composite) error {
	// We don't need to fetch the CompositeResourceDefinition at every reconcile
	// because enforced composition ref is immutable as opposed to default
	// composition ref.
	if s.def.Spec.EnforcedCompositionRef == nil {
		return nil
	}
	// If the composition is already chosen, we don't need to check for compatibility
	// as its target type reference is immutable.
	if cp.GetCompositionReference() != nil && cp.GetCompositionReference().Name == s.def.Spec.EnforcedCompositionRef.Name {
		return nil
	}
	cp.SetCompositionReference(&corev1.ObjectReference{Name: s.def.Spec.EnforcedCompositionRef.Name})
	s.recorder.Event(cp, event.Normal(reasonCompositionSelection, "Enforced composition has been selected"))
	return nil
}

// NewConfiguratorChain returns a new *ConfiguratorChain.
func NewConfiguratorChain(l ...Configurator) *ConfiguratorChain {
	return &ConfiguratorChain{list: l}
}

// ConfiguratorChain executes the Configurators in given order.
type ConfiguratorChain struct {
	list []Configurator
}

// Configure calls Configure function of every Configurator in the list.
func (cc *ConfiguratorChain) Configure(ctx context.Context, cp resource.Composite, rev *v1.CompositionRevision) error {
	for _, c := range cc.list {
		if err := c.Configure(ctx, cp, rev); err != nil {
			return err
		}
	}
	return nil
}

// NewAPIConfigurator returns a Configurator that configures a
// composite resource using its composition.
func NewAPIConfigurator(c client.Client) *APIConfigurator {
	return &APIConfigurator{client: c}
}

// TODO(negz): Is this the only XR -> Composition compatibility test? If so,
// refactor it. It doesn't use its client anymore. If not, delete it.

// An APIConfigurator configures a composite resource using its
// composition.
type APIConfigurator struct {
	client client.Client
}

// Configure any required fields that were omitted from the composite resource
// by copying them from its composition.
func (c *APIConfigurator) Configure(_ context.Context, cp resource.Composite, rev *v1.CompositionRevision) error {
	apiVersion, kind := cp.GetObjectKind().GroupVersionKind().ToAPIVersionAndKind()
	if rev.Spec.CompositeTypeRef.APIVersion != apiVersion || rev.Spec.CompositeTypeRef.Kind != kind {
		return errors.New(errCompositionNotCompatible)
	}
	return nil
}

// NewAPINamingConfigurator returns a Configurator that sets the root name prefixKu
// to its own name if it is not already set.
func NewAPINamingConfigurator(c client.Client) *APINamingConfigurator {
	return &APINamingConfigurator{client: c}
}

// An APINamingConfigurator sets the root name prefix to its own name if it is not
// already set.
type APINamingConfigurator struct {
	client client.Client
}

// Configure the supplied composite resource's root name prefix.
func (c *APINamingConfigurator) Configure(ctx context.Context, cp resource.Composite, _ *v1.CompositionRevision) error {
	if cp.GetLabels()[xcrd.LabelKeyNamePrefixForComposed] != "" {
		return nil
	}
	meta.AddLabels(cp, map[string]string{xcrd.LabelKeyNamePrefixForComposed: cp.GetName()})
	return errors.Wrap(c.client.Update(ctx, cp), errUpdateComposite)
}
