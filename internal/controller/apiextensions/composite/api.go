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

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	xpv1 "github.com/crossplane/crossplane-runtime/apis/common/v1"
	"github.com/crossplane/crossplane-runtime/pkg/errors"
	"github.com/crossplane/crossplane-runtime/pkg/event"
	"github.com/crossplane/crossplane-runtime/pkg/meta"
	"github.com/crossplane/crossplane-runtime/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/pkg/resource"

	v1 "github.com/crossplane/crossplane/apis/apiextensions/v1"
	"github.com/crossplane/crossplane/internal/xcrd"
)

// Error strings.
const (
	errApplySecret              = "cannot apply connection secret"
	errUpdateComposite          = "cannot update composite resource"
	errCompositionNotCompatible = "referenced composition is not compatible with this composite resource"
	errGetXRD                   = "cannot get composite resource definition"
)

// Event reasons.
const (
	reasonCompositionSelection    event.Reason = "CompositionSelection"
	reasonCompositionUpdatePolicy event.Reason = "CompositionUpdatePolicy"
)

// APIFilteredSecretPublisher publishes ConnectionDetails content after filtering
// it through a set of permitted keys.
type APIFilteredSecretPublisher struct {
	client resource.Applicator
	filter []string
}

// NewAPIFilteredSecretPublisher returns a ConnectionPublisher that only
// publishes connection secret keys that are included in the supplied filter.
func NewAPIFilteredSecretPublisher(c client.Client, filter []string) *APIFilteredSecretPublisher {
	return &APIFilteredSecretPublisher{client: resource.NewAPIPatchingApplicator(c), filter: filter}
}

// PublishConnection publishes the supplied ConnectionDetails to the Secret
// referenced in the resource.
func (a *APIFilteredSecretPublisher) PublishConnection(ctx context.Context, o ConnectionSecretOwner, c managed.ConnectionDetails) (bool, error) {
	// This resource does not want to expose a connection secret.
	if o.GetWriteConnectionSecretToReference() == nil {
		return false, nil
	}

	s := ConnectionSecretFor(o, o.GetObjectKind().GroupVersionKind())
	m := map[string]bool{}
	for _, key := range a.filter {
		m[key] = true
	}
	for key, val := range c {
		// If the filter does not have any keys, we allow all given keys to be
		// published.
		if len(m) == 0 || m[key] {
			s.Data[key] = val
		}
	}

	err := a.client.Apply(ctx, s,
		resource.ConnectionSecretMustBeControllableBy(o.GetUID()),
		resource.AllowUpdateIf(func(current, desired runtime.Object) bool {
			// We consider the update to be a no-op and don't allow it if the
			// current and existing secret data are identical.

			//nolint:forcetypeassert // These will always be secrets.
			return !cmp.Equal(current.(*corev1.Secret).Data, desired.(*corev1.Secret).Data, cmpopts.EquateEmpty())
		}),
	)
	if resource.IsNotAllowed(err) {
		// The update was not allowed because it was a no-op.
		return false, nil
	}
	if err != nil {
		return false, errors.Wrap(err, errApplySecret)
	}

	return true, nil
}

// ConnectionSecretFor creates a connection for the supplied
// ConnectionSecretOwner, assumed to be of the supplied kind. The secret is
// written to 'default' namespace if the ConnectionSecretOwner does not specify
// a namespace.
func ConnectionSecretFor(o ConnectionSecretOwner, kind schema.GroupVersionKind) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       o.GetWriteConnectionSecretToReference().Namespace,
			Name:            o.GetWriteConnectionSecretToReference().Name,
			OwnerReferences: []metav1.OwnerReference{meta.AsController(meta.TypedReferenceTo(o, kind))},
		},
		Type: resource.SecretTypeConnection,
		Data: make(map[string][]byte),
	}
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

// An APIConfigurator configures a composite resource using its
// composition.
type APIConfigurator struct {
	client client.Client
}

// Configure any required fields that were omitted from the composite resource
// by copying them from its composition.
func (c *APIConfigurator) Configure(ctx context.Context, cp resource.Composite, rev *v1.CompositionRevision) error {
	// Only legacy XRs support writing connection secrets.
	lcp, ok := cp.(resource.LegacyComposite)
	if !ok {
		return nil
	}

	apiVersion, kind := lcp.GetObjectKind().GroupVersionKind().ToAPIVersionAndKind()
	if rev.Spec.CompositeTypeRef.APIVersion != apiVersion || rev.Spec.CompositeTypeRef.Kind != kind {
		return errors.New(errCompositionNotCompatible)
	}

	if lcp.GetWriteConnectionSecretToReference() != nil || rev.Spec.WriteConnectionSecretsToNamespace == nil {
		return nil
	}

	lcp.SetWriteConnectionSecretToReference(&xpv1.SecretReference{
		Name:      string(cp.GetUID()),
		Namespace: *rev.Spec.WriteConnectionSecretsToNamespace,
	})

	return errors.Wrap(c.client.Update(ctx, cp), errUpdateComposite)
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
