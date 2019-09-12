# database.crossplane.io/v1alpha1 API Reference

Package v1alpha1 contains portable resource claims for database services such as MySQL or PostgreSQL.

This API group contains the following resources:

* [MySQLInstance](#MySQLInstance)
* [MySQLInstancePolicy](#MySQLInstancePolicy)
* [PostgreSQLInstance](#PostgreSQLInstance)
* [PostgreSQLInstancePolicy](#PostgreSQLInstancePolicy)

## MySQLInstance

A MySQLInstance is a portable resource claim that may be satisfied by binding to a MySQL managed resource such as an AWS RDS instance or a GCP CloudSQL instance.

Name | Type | Description
-----|------|------------
`apiVersion` | string | `database.crossplane.io/v1alpha1`
`kind` | string | `MySQLInstance`
`metadata` | [meta/v1.ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.15/#objectmeta-v1-meta) | Kubernetes object metadata.
`spec` | [MySQLInstanceSpec](#MySQLInstanceSpec) | MySQLInstanceSpec specifies the desired state of a MySQLInstance.
`status` | [v1alpha1.ResourceClaimStatus](../crossplane-runtime/core-crossplane-io-v1alpha1.md#resourceclaimstatus) | 



## MySQLInstancePolicy

MySQLInstancePolicy contains a namespace-scoped policy for MySQLInstance

Name | Type | Description
-----|------|------------
`apiVersion` | string | `database.crossplane.io/v1alpha1`
`kind` | string | `MySQLInstancePolicy`
`metadata` | [meta/v1.ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.15/#objectmeta-v1-meta) | Kubernetes object metadata.

Supports all fields of [v1alpha1.Policy](../crossplane-runtime/core-crossplane-io-v1alpha1.md#policy).


## PostgreSQLInstance

A PostgreSQLInstance is a portable resource claim that may be satisfied by binding to a PostgreSQL managed resource such as an AWS RDS instance or a GCP CloudSQL instance. PostgreSQLInstance is the CRD type for abstract PostgreSQL database instances

Name | Type | Description
-----|------|------------
`apiVersion` | string | `database.crossplane.io/v1alpha1`
`kind` | string | `PostgreSQLInstance`
`metadata` | [meta/v1.ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.15/#objectmeta-v1-meta) | Kubernetes object metadata.
`spec` | [PostgreSQLInstanceSpec](#PostgreSQLInstanceSpec) | PostgreSQLInstanceSpec specifies the desired state of a PostgreSQLInstance. PostgreSQLInstance.
`status` | [v1alpha1.ResourceClaimStatus](../crossplane-runtime/core-crossplane-io-v1alpha1.md#resourceclaimstatus) | 



## PostgreSQLInstancePolicy

PostgreSQLInstancePolicy contains a namespace-scoped policy for PostgreSQLInstance

Name | Type | Description
-----|------|------------
`apiVersion` | string | `database.crossplane.io/v1alpha1`
`kind` | string | `PostgreSQLInstancePolicy`
`metadata` | [meta/v1.ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.15/#objectmeta-v1-meta) | Kubernetes object metadata.

Supports all fields of [v1alpha1.Policy](../crossplane-runtime/core-crossplane-io-v1alpha1.md#policy).


## MySQLInstanceSpec

MySQLInstanceSpec specifies the desired state of a MySQLInstance.

Appears in:

* [MySQLInstance](#MySQLInstance)

Name | Type | Description
-----|------|------------
`engineVersion` | string | EngineVersion specifies the desired MySQL engine version, e.g. 5.7.

Supports all fields of [v1alpha1.ResourceClaimSpec](../crossplane-runtime/core-crossplane-io-v1alpha1.md#resourceclaimspec).


## PostgreSQLInstanceSpec

PostgreSQLInstanceSpec specifies the desired state of a PostgreSQLInstance. PostgreSQLInstance.

Appears in:

* [PostgreSQLInstance](#PostgreSQLInstance)

Name | Type | Description
-----|------|------------
`engineVersion` | string | EngineVersion specifies the desired PostgreSQL engine version, e.g. 9.6.

Supports all fields of [v1alpha1.ResourceClaimSpec](../crossplane-runtime/core-crossplane-io-v1alpha1.md#resourceclaimspec).


Generated with `gen-crd-api-reference-docs` on git commit `bcdc1d26`.