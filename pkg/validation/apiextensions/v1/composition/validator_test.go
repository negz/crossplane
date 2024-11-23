package composition

import (
	"context"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"

	v1 "github.com/crossplane/crossplane/apis/apiextensions/v1"
)

func TestValidatorValidate(t *testing.T) {
	type args struct {
		comp     *v1.Composition
		gkToCRDs map[schema.GroupKind]apiextensions.CustomResourceDefinition
	}
	type want struct {
		errs field.ErrorList
	}
	tests := map[string]struct {
		reason string
		args   args
		want   want
	}{
		"AcceptStrictNoCRDsNoPatches": {
			reason: "Should accept a Composition if no CRDs are available, but no patches are defined",
			want: want{
				errs: nil,
			},
			args: args{
				comp:     buildDefaultComposition(t, v1.SchemaAwareCompositionValidationModeStrict),
				gkToCRDs: nil,
			},
		},
		"AcceptStrictAllCRDs": {
			reason: "Should accept a valid Composition if all CRDs are available",
			want:   want{errs: nil},
			args: args{
				gkToCRDs: defaultGKToCRDs(),
				comp:     buildDefaultComposition(t, v1.SchemaAwareCompositionValidationModeStrict),
			},
		},
		"AcceptStrictInvalid": {
			reason: "Should accept a Composition not defining a required field in a resource if all CRDs are available",
			// TODO(phisco): this should return an error once we implement rendered validation
			want: want{errs: nil},
			args: args{
				gkToCRDs: defaultGKToCRDs(),
				comp:     buildDefaultComposition(t, v1.SchemaAwareCompositionValidationModeStrict),
			},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			v, err := NewValidator(WithCRDGetterFromMap(tc.args.gkToCRDs))
			if err != nil {
				t.Errorf("NewValidator(...) = %v", err)
				return
			}
			_, got := v.Validate(context.TODO(), tc.args.comp)
			if diff := cmp.Diff(tc.want.errs, got, sortFieldErrors(), cmpopts.IgnoreFields(field.Error{}, "Detail", "BadValue")); diff != "" {
				t.Errorf("%s\nValidate(...) = -want, +got\n%s", tc.reason, diff)
			}
		})
	}
}

// SortFieldErrors sorts the given field.ErrorList by the error message.
func sortFieldErrors() cmp.Option {
	return cmpopts.SortSlices(func(e1, e2 *field.Error) bool {
		return strings.Compare(e1.Error(), e2.Error()) < 0
	})
}

const (
	testGroup         = "resources.test.com"
	testGroupSingular = "resource.test.com"
)

func defaultCompositeCrdBuilder() *crdBuilder {
	return newCRDBuilder("Composite", "v1").withOption(specSchemaOption("v1", extv1.JSONSchemaProps{
		Type: "object",
		Required: []string{
			"someField",
		},
		Properties: map[string]extv1.JSONSchemaProps{
			"someField": {
				Type: "string",
			},
			"someNonRequiredField": {
				Type: "string",
			},
		},
	}))
}

func defaultManagedCrdBuilder() *crdBuilder {
	return newCRDBuilder("Managed", "v1").withOption(specSchemaOption("v1", extv1.JSONSchemaProps{
		Type: "object",
		Required: []string{
			"someOtherField",
		},
		Properties: map[string]extv1.JSONSchemaProps{
			"someOtherField": {
				Type: "string",
			},
			"someNonRequiredField": {
				Type: "string",
			},
		},
	}))
}

func defaultGKToCRDs() map[schema.GroupKind]apiextensions.CustomResourceDefinition {
	crds := []apiextensions.CustomResourceDefinition{*defaultManagedCrdBuilder().build(), *defaultCompositeCrdBuilder().build()}
	m := make(map[schema.GroupKind]apiextensions.CustomResourceDefinition, len(crds))
	for _, crd := range crds {
		m[schema.GroupKind{
			Group: crd.Spec.Group,
			Kind:  crd.Spec.Names.Kind,
		}] = crd
	}
	return m
}

type builderOption func(*extv1.CustomResourceDefinition)

type crdBuilder struct {
	kind, version string
	opts          []builderOption
}

func newCRDBuilder(kind, version string) *crdBuilder {
	return &crdBuilder{kind: kind, version: version}
}

func specSchemaOption(version string, schema extv1.JSONSchemaProps) builderOption {
	return func(crd *extv1.CustomResourceDefinition) {
		var storageFound bool
		for i, definitionVersion := range crd.Spec.Versions {
			storageFound = storageFound || definitionVersion.Storage
			if definitionVersion.Name == version {
				crd.Spec.Versions[i].Schema = &extv1.CustomResourceValidation{
					OpenAPIV3Schema: &extv1.JSONSchemaProps{
						Type: "object",
						Required: []string{
							"spec",
						},
						Properties: map[string]extv1.JSONSchemaProps{
							"spec": schema,
						},
					},
				}
				return
			}
		}
		crd.Spec.Versions = append(crd.Spec.Versions, extv1.CustomResourceDefinitionVersion{
			Name:    version,
			Served:  true,
			Storage: !storageFound,
			Schema: &extv1.CustomResourceValidation{
				OpenAPIV3Schema: &extv1.JSONSchemaProps{
					Type: "object",
					Required: []string{
						"spec",
					},
					Properties: map[string]extv1.JSONSchemaProps{
						"spec": schema,
					},
				},
			},
		})
	}
}

func (b *crdBuilder) withOption(f builderOption) *crdBuilder {
	b.opts = append(b.opts, f)
	return b
}

func (b *crdBuilder) build() *apiextensions.CustomResourceDefinition {
	internal := &apiextensions.CustomResourceDefinition{}
	_ = extv1.Convert_v1_CustomResourceDefinition_To_apiextensions_CustomResourceDefinition(b.buildExtV1(), internal, nil)
	return internal
}

func (b *crdBuilder) buildExtV1() *extv1.CustomResourceDefinition {
	crd := &extv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: strings.ToLower(b.kind) + "s." + testGroupSingular,
		},
		Spec: extv1.CustomResourceDefinitionSpec{
			Group: testGroup,
			Names: extv1.CustomResourceDefinitionNames{
				Kind: b.kind,
			},
			Versions: []extv1.CustomResourceDefinitionVersion{
				{
					Name:    b.version,
					Served:  true,
					Storage: true,
				},
			},
		},
	}
	for _, opt := range b.opts {
		opt(crd)
	}
	return crd
}

type compositionBuilderOption func(c *v1.Composition)

func buildDefaultComposition(t *testing.T, validationMode v1.CompositionValidationMode, opts ...compositionBuilderOption) *v1.Composition {
	t.Helper()
	c := &v1.Composition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "testComposition",
			Annotations: map[string]string{
				v1.SchemaAwareCompositionValidationModeAnnotation: string(validationMode),
			},
		},
		Spec: v1.CompositionSpec{
			CompositeTypeRef: v1.TypeReference{
				APIVersion: testGroup + "/v1",
				Kind:       "Composite",
			},
			Mode: v1.CompositionModePipeline,
			Pipeline: []v1.PipelineStep{
				{
					// TODO(negz): This probably shouldn't be valid.
				},
			},
		},
	}

	for _, opt := range opts {
		opt(c)
	}
	return c
}
