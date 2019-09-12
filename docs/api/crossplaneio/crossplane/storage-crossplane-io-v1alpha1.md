# storage.crossplane.io/v1alpha1 API Reference

Package v1alpha1 contains portable resource claims for storage services such as buckets.

This API group contains the following resources:

* [Bucket](#Bucket)
* [BucketPolicy](#BucketPolicy)

## Bucket

A Bucket is a portable resource claim that may be satisfied by binding to a storage bucket PostgreSQL managed resource such as an AWS S3 bucket or Azure storage container.

Name | Type | Description
-----|------|------------
`apiVersion` | string | `storage.crossplane.io/v1alpha1`
`kind` | string | `Bucket`
`metadata` | [meta/v1.ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.15/#objectmeta-v1-meta) | Kubernetes object metadata.
`spec` | [BucketSpec](#BucketSpec) | BucketSpec specifies the desired state of a Bucket.
`status` | [v1alpha1.ResourceClaimStatus](../crossplane-runtime/core-crossplane-io-v1alpha1.md#resourceclaimstatus) | 



## BucketPolicy

BucketPolicy contains a namespace-scoped policy for Bucket

Name | Type | Description
-----|------|------------
`apiVersion` | string | `storage.crossplane.io/v1alpha1`
`kind` | string | `BucketPolicy`
`metadata` | [meta/v1.ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.15/#objectmeta-v1-meta) | Kubernetes object metadata.

Supports all fields of [v1alpha1.Policy](../crossplane-runtime/core-crossplane-io-v1alpha1.md#policy).


## BucketSpec

BucketSpec specifies the desired state of a Bucket.

Appears in:

* [Bucket](#Bucket)

Name | Type | Description
-----|------|------------
`name` | string | Name specifies the desired name of the bucket.
`predefinedACL` | [PredefinedACL](#PredefinedACL) | PredefinedACL specifies a predefined ACL (e.g. Private, ReadWrite, etc) to be applied to the bucket.
`localPermission` | [LocalPermissionType](#LocalPermissionType) | LocalPermission specifies permissions granted to a provider specific service account for this bucket, e.g. Read, ReadWrite, or Write.

Supports all fields of [v1alpha1.ResourceClaimSpec](../crossplane-runtime/core-crossplane-io-v1alpha1.md#resourceclaimspec).


## LocalPermissionType

A LocalPermissionType is a type of permission that may be granted to a Bucket. Alias of string.

Appears in:

* [BucketSpec](#BucketSpec)


## PredefinedACL

A PredefinedACL is a predefined ACL that may be applied to a Bucket. Alias of string.

Appears in:

* [BucketSpec](#BucketSpec)


Generated with `gen-crd-api-reference-docs` on git commit `bcdc1d26`.