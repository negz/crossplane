package definition

import (
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/crossplane/crossplane/internal/xcrd"
)

// IsCompositeResourceCRD accepts any CustomResourceDefinition that represents a
// Composite Resource.
func IsCompositeResourceCRD() predicate.Funcs {
	return predicate.NewPredicateFuncs(func(obj client.Object) bool {
		crd, ok := obj.(*extv1.CustomResourceDefinition)
		if !ok {
			return false
		}
		for _, c := range crd.Spec.Names.Categories {
			if c == xcrd.CategoryComposite {
				return true
			}
		}
		return false
	})
}
