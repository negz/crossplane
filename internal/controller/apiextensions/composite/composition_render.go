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
	"github.com/crossplane/crossplane-runtime/pkg/errors"
	"github.com/crossplane/crossplane-runtime/pkg/meta"
	"github.com/crossplane/crossplane-runtime/pkg/resource"

	"github.com/crossplane/crossplane/internal/xcrd"
)

// Error strings.
const (
	errUnmarshalJSON      = "cannot unmarshal JSON data"
	errMarshalProtoStruct = "cannot marshal protobuf Struct to JSON"
	errSetControllerRef   = "cannot set controller reference"

	errFmtKindChanged     = "cannot change the kind of a composed resource from %s to %s (possible composed resource template mismatch)"
	errFmtNamePrefixLabel = "cannot find top-level composite resource name label %q in composite resource metadata"
)

// RenderComposedResourceMetadata derives composed resource metadata from the
// supplied composite resource. It makes the composite resource the controller
// of the composed resource. It should run toward the end of a render pipeline
// to ensure that a Composition cannot influence the controller reference.
func RenderComposedResourceMetadata(cd, xr resource.Object, n ResourceName) error {
	// Fail early if the supplied composite resource is missing the name prefix
	// label.
	if xr.GetLabels()[xcrd.LabelKeyNamePrefixForComposed] == "" {
		return errors.Errorf(errFmtNamePrefixLabel, xcrd.LabelKeyNamePrefixForComposed)
	}

	//  We also set generate name in case we
	// haven't yet named this composed resource.
	cd.SetGenerateName(xr.GetLabels()[xcrd.LabelKeyNamePrefixForComposed] + "-")

	if n != "" {
		SetCompositionResourceName(cd, n)
	}

	meta.AddLabels(cd, map[string]string{
		xcrd.LabelKeyNamePrefixForComposed: xr.GetLabels()[xcrd.LabelKeyNamePrefixForComposed],
	})

	or := meta.AsController(meta.TypedReferenceTo(xr, xr.GetObjectKind().GroupVersionKind()))
	return errors.Wrap(meta.AddControllerReference(cd, or), errSetControllerRef)
}

// TODO(negz): It's simple enough that we should just inline it into the
// PTComposer, which is now the only consumer.
