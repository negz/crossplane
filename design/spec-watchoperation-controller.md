# WatchOperation controller spec.

## Background

This is a spec for AI assisted implementation of the WatchOperation controller.

Important background context:

* The Operations design at design/design-doc-operations.md
* The definition (XRD) and composite (XR) controllers at
  internal/controller/apiextensions
* The ControllerEngine at internal/engine
* The CronOperation controller at internal/controller/ops

## Design

We'll implement WatchOperation as two controllers. The closest existing example
to this pattern in Crossplane is the definition and composite controllers:

* The definition controller watches CompositeResourceDefinition, which defines a
  new API type (using a CustomResourceDefinition) that we call an XR.
* The definition controller starts a composite controller for each XR
* The composite controller watches the type of XR it's responsible for

Let's call the two WatchOperation controllers WatchOperation and Watched.

The WatchOperation controller is responsible for:

* Watching and reconciling WatchOperations
* Listing Operations that belong to the WatchOperation
* Garbage collecting Operations that belong to the WatchOperation
* Updating the WatchOperation's status.runningResourceRefs
* Updating the WatchOperation's status.watchingResources count
* Using its ControllerEngine to start and stop Watched controllers for the type
  of resource the WatchOperation watches (spec.watch)

The Watched controller is responsible for:

* Watching and reconciling the WatchOperation's watched type (spec.watch)
* Creating Operations using the WatchOperation's spec.operationTemplate
* Respecting the WatchOperation's concurrency policy

The operation design proposes injecting the watched resource into the templated
Operation. We don't want to do that yet on this first iteration.

The controllers should match the CronOperation implementation as closely as
possible. For example:

* Use the same utils as the CronOperation
* The WatchOperation controller should use the same pattern as CronOperation for
  handling garbage collection, last scheduled time, last successful time, etc.
* The Watched controller should use the same pattern as CronOperation for
  handling concurrency policy

Remember the general guidance I've given you about writing controllers:

* No private methods on the Reconciler type. It should have only one method:
  Reconcile.
* If necessary use ReconcilerOptions that accept an implementation of
  an interface to inject dependencies into the Reconciler. This is a better way
  to break up the logic (e.g. for testing) than private methods.
* Use injected dependencies sparingly. I'm comfortable with the Reconcile method
  being quite long, especially if it remains under the cognitive complexity
  linter check (gocognit linter).
