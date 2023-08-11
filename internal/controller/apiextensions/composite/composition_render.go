/*
Copyright 2022 The Crossplane Authors.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use
this file except in compliance with the License. You may obtain a copy of the
License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed
under the License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR
CONDITIONS OF ANY KIND, either express or implied. See the License for the
specific language governing permissions and limitations under the License.
*/

package composite

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/json"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/crossplane/crossplane-runtime/pkg/errors"
	"github.com/crossplane/crossplane-runtime/pkg/meta"
	"github.com/crossplane/crossplane-runtime/pkg/resource"

	v1 "github.com/crossplane/crossplane/apis/apiextensions/v1"
	env "github.com/crossplane/crossplane/internal/controller/apiextensions/composite/environment"
	"github.com/crossplane/crossplane/internal/xcrd"
)

// RenderJSON renders the supplied resource from JSON bytes.
func RenderJSON(o resource.Object, data []byte) error {
	gvk := o.GetObjectKind().GroupVersionKind()
	name := o.GetName()
	namespace := o.GetNamespace()

	if err := json.Unmarshal(data, o); err != nil {
		return errors.Wrap(err, errUnmarshal)
	}

	// TODO(negz): Should we return an error if the name or namespace change,
	// rather than just silently re-setting it? Presumably these _changing_ is a
	// sign that something has gone wrong, similar to the GVK changing.

	// Unmarshalling the template will overwrite any existing fields, so we must
	// restore the existing name, if any.
	o.SetName(name)
	o.SetNamespace(namespace)

	// This resource already had a GVK (probably because it already exists), but
	// when we rendered its template it changed. This shouldn't happen. Either
	// someone changed the kind in the template or we're trying to use the wrong
	// template (e.g. because the order of an array of anonymous templates
	// changed).
	empty := schema.GroupVersionKind{}
	if gvk != empty && o.GetObjectKind().GroupVersionKind() != gvk {
		return errors.New(errKindChanged)
	}

	return nil
}

// RenderFromCompositePatches renders the supplied composed resource by applying
// all patches that are _from_ the supplied composite resource.
func RenderFromCompositePatches(cd resource.Composed, xr resource.Composite, p []v1.Patch) error {
	for i := range p {
		if err := Apply(p[i], xr, cd, patchTypesFromXR()...); err != nil {
			return errors.Wrapf(err, errFmtPatch, i)
		}
	}
	return nil
}

// RenderFromEnvironmentPatches renders the supplied composed resource by
// applying all patches that are from the supplied environment.
func RenderFromEnvironmentPatches(cd resource.Composed, e *env.Environment, p []v1.Patch) error {
	if e == nil {
		return nil
	}
	for i := range p {
		if err := ApplyToObjects(p[i], e, cd, patchTypesFromToEnvironment()...); err != nil {
			return errors.Wrapf(err, errFmtPatch, i)
		}
	}
	return nil
}

// RenderToCompositePatches renders the supplied composite resource by applying
// all patches that are _from_ the supplied composed resource. composed resource
// and template.
func RenderToCompositePatches(xr resource.Composite, cd resource.Composed, p []v1.Patch) error {
	for i := range p {
		if err := Apply(p[i], xr, cd, patchTypesToXR()...); err != nil {
			return errors.Wrapf(err, errFmtPatch, i)
		}
	}
	return nil
}

// RenderComposedResourceMetadata derives composed resource metadata from the
// supplied composite resource. It makes the composite resource the controller
// of the composed resource. It should run toward the end of a render pipeline
// to ensure that a Composition cannot influence the controller reference.
func RenderComposedResourceMetadata(cd, xr resource.Object, n ResourceName) error {
	// Fail early if the supplied composite resource is missing the name prefix
	// label.
	if xr.GetLabels()[xcrd.LabelKeyNamePrefixForComposed] == "" {
		return errors.New(errNamePrefix)
	}

	//  We also set generate name in case we
	// haven't yet named this composed resource.
	cd.SetGenerateName(xr.GetLabels()[xcrd.LabelKeyNamePrefixForComposed] + "-")

	if n != "" {
		SetCompositionResourceName(cd, n)
	}

	meta.AddLabels(cd, map[string]string{
		xcrd.LabelKeyNamePrefixForComposed: xr.GetLabels()[xcrd.LabelKeyNamePrefixForComposed],
		xcrd.LabelKeyClaimName:             xr.GetLabels()[xcrd.LabelKeyClaimName],
		xcrd.LabelKeyClaimNamespace:        xr.GetLabels()[xcrd.LabelKeyClaimNamespace],
	})

	or := meta.AsController(meta.TypedReferenceTo(xr, xr.GetObjectKind().GroupVersionKind()))
	return errors.Wrap(meta.AddControllerReference(cd, or), errSetControllerRef)
}

// TODO(negz): Is this really a 'renderer'?

// An APIDryRunRenderer submits a resource to the API server in order to name
// and validate it.
type APIDryRunRenderer struct{ client client.Client }

// NewAPIDryRunRenderer returns a Renderer that submits a resource to
// the API server in order to name and validate it.
func NewAPIDryRunRenderer(c client.Client) *APIDryRunRenderer {
	return &APIDryRunRenderer{client: c}
}

// DryRunRender submits the resource to the API server via a dry run create in
// order to name and validate it.
func (r *APIDryRunRenderer) DryRunRender(ctx context.Context, cd resource.Object) error {
	// We don't want to dry-run create a resource that can't be named by the API
	// server due to a missing generate name. We also don't want to create one
	// that is already named, because doing so will result in an error. The API
	// server seems to respond with a 500 ServerTimeout error for all dry-run
	// failures, so we can't just perform a dry-run and ignore 409 Conflicts for
	// resources that are already named.
	if cd.GetName() != "" || cd.GetGenerateName() == "" {
		return nil
	}

	// The API server returns an available name derived from generateName when
	// we perform a dry-run create. This name is likely (but not guaranteed) to
	// be available when we create the composed resource. If the API server
	// generates a name that is unavailable it will return a 500 ServerTimeout
	// error.
	return errors.Wrap(r.client.Create(ctx, cd, client.DryRunAll), errName)
}
