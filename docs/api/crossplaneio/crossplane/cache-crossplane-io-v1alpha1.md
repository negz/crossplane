# cache.crossplane.io/v1alpha1 API Reference

Package v1alpha1 contains portable resource claims for caching services such as Redis clusters.

This API group contains the following resources:

* [RedisCluster](#RedisCluster)
* [RedisClusterPolicy](#RedisClusterPolicy)

## RedisCluster

A RedisCluster is a portable resource claim that may be satisfied by binding to a Redis managed resource such as a GCP CloudMemorystore instance or an AWS ReplicationGroup. Despite the name RedisCluster claims may bind to Redis managed resources that are a single node, or not in cluster mode.

Name | Type | Description
-----|------|------------
`apiVersion` | string | `cache.crossplane.io/v1alpha1`
`kind` | string | `RedisCluster`
`metadata` | [meta/v1.ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.15/#objectmeta-v1-meta) | Kubernetes object metadata.
`spec` | [RedisClusterSpec](#RedisClusterSpec) | RedisClusterSpec specifies the desired state of a RedisCluster.
`status` | [v1alpha1.ResourceClaimStatus](../crossplane-runtime/core-crossplane-io-v1alpha1.md#resourceclaimstatus) | 



## RedisClusterPolicy

RedisClusterPolicy contains a namespace-scoped policy for RedisCluster

Name | Type | Description
-----|------|------------
`apiVersion` | string | `cache.crossplane.io/v1alpha1`
`kind` | string | `RedisClusterPolicy`
`metadata` | [meta/v1.ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.15/#objectmeta-v1-meta) | Kubernetes object metadata.

Supports all fields of [v1alpha1.Policy](../crossplane-runtime/core-crossplane-io-v1alpha1.md#policy).


## RedisClusterSpec

RedisClusterSpec specifies the desired state of a RedisCluster.

Appears in:

* [RedisCluster](#RedisCluster)

Name | Type | Description
-----|------|------------
`engineVersion` | string | EngineVersion specifies the desired Redis version.

Supports all fields of [v1alpha1.ResourceClaimSpec](../crossplane-runtime/core-crossplane-io-v1alpha1.md#resourceclaimspec).


Generated with `gen-crd-api-reference-docs` on git commit `bcdc1d26`.