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

// Package xcrd generates CustomResourceDefinitions from Crossplane definitions.
//
// v1.JSONSchemaProps is incompatible with controller-tools (as of 0.2.4)
// because it is missing JSON tags and uses float64, which is a disallowed type.
// We thus copy the entire struct as CRDSpecTemplate. See the below issue:
// https://github.com/kubernetes-sigs/controller-tools/issues/291
package xcrd

import (
	"encoding/json"
	"fmt"

	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/crossplane/crossplane-runtime/pkg/errors"
	"github.com/crossplane/crossplane-runtime/pkg/meta"

	v1 "github.com/crossplane/crossplane/apis/apiextensions/v1"
)

// Category names for generated composite CRDs.
const (
	CategoryComposite = "composite"
)

const (
	errFmtGenCrd                   = "cannot generate CRD for %q %q"
	errParseValidation             = "cannot parse validation schema"
	errCustomResourceValidationNil = "custom resource validation cannot be nil"
)

// ForCompositeResource derives the CustomResourceDefinition for a composite
// resource from the supplied CompositeResourceDefinition.
func ForCompositeResource(xrd *v1.CompositeResourceDefinition) (*extv1.CustomResourceDefinition, error) {
	crd := &extv1.CustomResourceDefinition{
		Spec: extv1.CustomResourceDefinitionSpec{
			Scope:      extv1.ClusterScoped,
			Group:      xrd.Spec.Group,
			Names:      xrd.Spec.Names,
			Versions:   make([]extv1.CustomResourceDefinitionVersion, len(xrd.Spec.Versions)),
			Conversion: xrd.Spec.Conversion,
		},
	}

	crd.SetName(xrd.GetName())
	setCrdMetadata(crd, xrd)
	crd.SetOwnerReferences([]metav1.OwnerReference{meta.AsController(
		meta.TypedReferenceTo(xrd, v1.CompositeResourceDefinitionGroupVersionKind),
	)})

	crd.Spec.Names.Categories = append(crd.Spec.Names.Categories, CategoryComposite)

	// The composite name is used as a label value, so we must ensure it is not
	// longer.
	const maxCompositeNameLength = 63

	for i, vr := range xrd.Spec.Versions {
		crdv, err := genCrdVersion(vr, maxCompositeNameLength)
		if err != nil {
			return nil, errors.Wrapf(err, errFmtGenCrd, "Composite Resource", xrd.Name)
		}
		crdv.AdditionalPrinterColumns = append(crdv.AdditionalPrinterColumns, CompositeResourcePrinterColumns()...)
		props := CompositeResourceSpecProps()
		if xrd.Spec.DefaultCompositionUpdatePolicy != nil {
			cup := props["compositionUpdatePolicy"]
			cup.Default = &extv1.JSON{Raw: []byte(fmt.Sprintf("\"%s\"", *xrd.Spec.DefaultCompositionUpdatePolicy))}
			props["compositionUpdatePolicy"] = cup
		}
		for k, v := range props {
			crdv.Schema.OpenAPIV3Schema.Properties["spec"].Properties[k] = v
		}
		crd.Spec.Versions[i] = *crdv
	}

	return crd, nil
}

func genCrdVersion(vr v1.CompositeResourceDefinitionVersion, maxNameLength int64) (*extv1.CustomResourceDefinitionVersion, error) {
	crdv := extv1.CustomResourceDefinitionVersion{
		Name:                     vr.Name,
		Served:                   vr.Served,
		Storage:                  vr.Referenceable,
		Deprecated:               ptr.Deref(vr.Deprecated, false),
		DeprecationWarning:       vr.DeprecationWarning,
		AdditionalPrinterColumns: vr.AdditionalPrinterColumns,
		Schema: &extv1.CustomResourceValidation{
			OpenAPIV3Schema: BaseProps(),
		},
		Subresources: &extv1.CustomResourceSubresources{
			Status: &extv1.CustomResourceSubresourceStatus{},
		},
	}
	s, err := parseSchema(vr.Schema)
	if err != nil {
		return nil, errors.Wrapf(err, errParseValidation)
	}

	if s == nil {
		return nil, errors.New(errCustomResourceValidationNil)
	}

	crdv.Schema.OpenAPIV3Schema.Description = s.Description

	maxLength := maxNameLength
	if old := s.Properties["metadata"].Properties["name"].MaxLength; old != nil && *old < maxLength {
		maxLength = *old
	}
	xName := crdv.Schema.OpenAPIV3Schema.Properties["metadata"].Properties["name"]
	xName.MaxLength = ptr.To(maxLength)
	xName.Type = "string"
	xMetaData := crdv.Schema.OpenAPIV3Schema.Properties["metadata"]
	xMetaData.Properties = map[string]extv1.JSONSchemaProps{"name": xName}
	crdv.Schema.OpenAPIV3Schema.Properties["metadata"] = xMetaData

	xSpec := s.Properties["spec"]
	cSpec := crdv.Schema.OpenAPIV3Schema.Properties["spec"]
	cSpec.Required = append(cSpec.Required, xSpec.Required...)
	cSpec.XPreserveUnknownFields = xSpec.XPreserveUnknownFields
	cSpec.XValidations = append(cSpec.XValidations, xSpec.XValidations...)
	cSpec.OneOf = append(cSpec.OneOf, xSpec.OneOf...)
	cSpec.Description = xSpec.Description
	for k, v := range xSpec.Properties {
		cSpec.Properties[k] = v
	}
	crdv.Schema.OpenAPIV3Schema.Properties["spec"] = cSpec

	xStatus := s.Properties["status"]
	cStatus := crdv.Schema.OpenAPIV3Schema.Properties["status"]
	cStatus.Required = xStatus.Required
	cStatus.XValidations = xStatus.XValidations
	cStatus.Description = xStatus.Description
	cStatus.OneOf = xStatus.OneOf
	for k, v := range xStatus.Properties {
		cStatus.Properties[k] = v
	}
	for k, v := range CompositeResourceStatusProps() {
		cStatus.Properties[k] = v
	}
	crdv.Schema.OpenAPIV3Schema.Properties["status"] = cStatus
	return &crdv, nil
}

func parseSchema(v *v1.CompositeResourceValidation) (*extv1.JSONSchemaProps, error) {
	if v == nil {
		return nil, nil
	}

	s := &extv1.JSONSchemaProps{}
	if err := json.Unmarshal(v.OpenAPIV3Schema.Raw, s); err != nil {
		return nil, errors.Wrap(err, errParseValidation)
	}
	return s, nil
}

// setCrdMetadata sets the labels and annotations on the CRD.
func setCrdMetadata(crd *extv1.CustomResourceDefinition, xrd *v1.CompositeResourceDefinition) *extv1.CustomResourceDefinition {
	crd.SetLabels(xrd.GetLabels())
	if xrd.Spec.Metadata != nil {
		if xrd.Spec.Metadata.Labels != nil {
			inheritedLabels := crd.GetLabels()
			if inheritedLabels == nil {
				inheritedLabels = map[string]string{}
			}
			for k, v := range xrd.Spec.Metadata.Labels {
				inheritedLabels[k] = v
			}
			crd.SetLabels(inheritedLabels)
		}
		if xrd.Spec.Metadata.Annotations != nil {
			crd.SetAnnotations(xrd.Spec.Metadata.Annotations)
		}
	}
	return crd
}

// IsEstablished is a helper function to check whether api-server is ready
// to accept the instances of registered CRD.
func IsEstablished(s extv1.CustomResourceDefinitionStatus) bool {
	for _, c := range s.Conditions {
		if c.Type == extv1.Established {
			return c.Status == extv1.ConditionTrue
		}
	}
	return false
}
