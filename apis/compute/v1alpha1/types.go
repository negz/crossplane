/*
Copyright 2019 The Crossplane Authors.

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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	runtimev1alpha1 "github.com/crossplaneio/crossplane-runtime/apis/core/v1alpha1"
	"github.com/crossplaneio/crossplane-runtime/pkg/resource"
)

// KubernetesClusterSpec specifies the desired state of a KubernetesCluster.
type KubernetesClusterSpec struct {
	runtimev1alpha1.ResourceClaimSpec `json:",inline"`

	// ClusterVersion specifies the desired Kubernetes version, e.g. 1.15.
	ClusterVersion string `json:"clusterVersion,omitempty"`
}

// +kubebuilder:object:root=true

// A KubernetesCluster is a portable resource claim that may be satisfied by
// binding to a Kubernetes cluster managed resource such as an AWS EKS cluster
// or an Azure AKS cluster.
// +kubebuilder:printcolumn:name="STATUS",type="string",JSONPath=".status.bindingPhase"
// +kubebuilder:printcolumn:name="CLUSTER-CLASS",type="string",JSONPath=".spec.classRef.name"
// +kubebuilder:printcolumn:name="CLUSTER-REF",type="string",JSONPath=".spec.resourceName.name"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:subresource:status
type KubernetesCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KubernetesClusterSpec               `json:"spec,omitempty"`
	Status runtimev1alpha1.ResourceClaimStatus `json:"status,omitempty"`
}

// SetBindingPhase of this KubernetesCluster.
func (kc *KubernetesCluster) SetBindingPhase(p runtimev1alpha1.BindingPhase) {
	kc.Status.SetBindingPhase(p)
}

// GetBindingPhase of this KubernetesCluster.
func (kc *KubernetesCluster) GetBindingPhase() runtimev1alpha1.BindingPhase {
	return kc.Status.GetBindingPhase()
}

// SetConditions of this KubernetesCluster.
func (kc *KubernetesCluster) SetConditions(c ...runtimev1alpha1.Condition) {
	kc.Status.SetConditions(c...)
}

// SetClassReference of this KubernetesCluster.
func (kc *KubernetesCluster) SetClassReference(r *corev1.ObjectReference) {
	kc.Spec.ClassReference = r
}

// GetClassReference of this KubernetesCluster.
func (kc *KubernetesCluster) GetClassReference() *corev1.ObjectReference {
	return kc.Spec.ClassReference
}

// SetResourceReference of this KubernetesCluster.
func (kc *KubernetesCluster) SetResourceReference(r *corev1.ObjectReference) {
	kc.Spec.ResourceReference = r
}

// GetResourceReference of this KubernetesCluster.
func (kc *KubernetesCluster) GetResourceReference() *corev1.ObjectReference {
	return kc.Spec.ResourceReference
}

// SetWriteConnectionSecretToReference of this KubernetesCluster.
func (kc *KubernetesCluster) SetWriteConnectionSecretToReference(r corev1.LocalObjectReference) {
	kc.Spec.WriteConnectionSecretToReference = r
}

// GetWriteConnectionSecretToReference of this KubernetesCluster.
func (kc *KubernetesCluster) GetWriteConnectionSecretToReference() corev1.LocalObjectReference {
	return kc.Spec.WriteConnectionSecretToReference
}

// +kubebuilder:object:root=true

// KubernetesClusterList contains a list of KubernetesCluster.
type KubernetesClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KubernetesCluster `json:"items"`
}

// All policies must satisfy the Policy interface
var _ resource.Policy = &KubernetesClusterPolicy{}

// +kubebuilder:object:root=true

// KubernetesClusterPolicy contains a namespace-scoped policy for KubernetesCluster
type KubernetesClusterPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	runtimev1alpha1.Policy `json:",inline"`
}

// All policy lists must satisfy the PolicyList interface
var _ resource.PolicyList = &KubernetesClusterPolicyList{}

// +kubebuilder:object:root=true

// KubernetesClusterPolicyList contains a list of KubernetesClusterPolicy.
type KubernetesClusterPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KubernetesClusterPolicy `json:"items"`
}
