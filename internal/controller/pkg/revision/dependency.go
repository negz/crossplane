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

package revision

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/Masterminds/semver"
	"github.com/google/go-containerregistry/pkg/name"
	conregv1 "github.com/google/go-containerregistry/pkg/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"

	pkgmetav1 "github.com/crossplane/crossplane/v2/apis/pkg/meta/v1"
	v1 "github.com/crossplane/crossplane/v2/apis/pkg/v1"
	"github.com/crossplane/crossplane/v2/apis/pkg/v1beta1"
	"github.com/crossplane/crossplane/v2/internal/dag"
	"github.com/crossplane/crossplane/v2/internal/xpkg"
)

const (
	lockName = "lock"

	errGetOrCreateLock           = "cannot get or create lock"
	errInitDAG                   = "cannot initialize dependency graph from the packages in the lock"
	errFmtIncompatibleDependency = "incompatible dependencies: %s"
	errFmtMissingDependencies    = "missing dependencies: %+v"
	errDependencyNotInGraph      = "dependency is not present in graph"
	errDependencyNotLockPackage  = "dependency in graph is not a lock package"
)

// DependencyManager is a lock on packages.
type DependencyManager interface {
	Resolve(ctx context.Context, meta pkgmetav1.Pkg, pr v1.PackageRevision) (found, installed, invalid int, err error)
	RemoveSelf(ctx context.Context, pr v1.PackageRevision) error
}

// PackageDependencyManager is a resolver for packages.
type PackageDependencyManager struct {
	client      client.Client
	newDag      dag.NewDAGFn
	packageType schema.GroupVersionKind
	config      xpkg.ConfigStore
	log         logging.Logger
}

// NewPackageDependencyManager creates a new PackageDependencyManager.
func NewPackageDependencyManager(c client.Client, nd dag.NewDAGFn, pkgType schema.GroupVersionKind, config xpkg.ConfigStore, l logging.Logger) *PackageDependencyManager {
	return &PackageDependencyManager{
		client:      c,
		newDag:      nd,
		packageType: pkgType,
		config:      config,
		log:         l,
	}
}

// Resolve resolves package dependencies.
func (m *PackageDependencyManager) Resolve(ctx context.Context, meta pkgmetav1.Pkg, pr v1.PackageRevision) (found, installed, invalid int, err error) { //nolint:gocognit // TODO(negz): Can this be refactored for less complexity?
	// If we are inactive, we don't need to resolve dependencies.
	if pr.GetDesiredState() == v1.PackageRevisionInactive {
		return 0, 0, 0, nil
	}

	// Copy package dependencies into Lock Dependencies.
	sources := make([]v1beta1.Dependency, len(meta.GetDependencies()))
	for i, dep := range meta.GetDependencies() {
		pdep := v1beta1.Dependency{}

		switch {
		// If the GVK and package are specified explicitly they take precedence.
		case dep.APIVersion != nil && dep.Kind != nil && dep.Package != nil:
			pdep.APIVersion = dep.APIVersion
			pdep.Kind = dep.Kind
			pdep.Package = *dep.Package
		case dep.Configuration != nil:
			pdep.Package = *dep.Configuration
			pdep.Type = ptr.To(v1beta1.ConfigurationPackageType)
		case dep.Provider != nil:
			pdep.Package = *dep.Provider
			pdep.Type = ptr.To(v1beta1.ProviderPackageType)
		case dep.Function != nil:
			pdep.Package = *dep.Function
			pdep.Type = ptr.To(v1beta1.FunctionPackageType)
		default:
			return 0, 0, 0, errors.Errorf("encountered an invalid dependency: package dependencies must specify either a valid type, or an explicit apiVersion, kind, and package")
		}

		pdep.Constraints = dep.Version

		// Apply ImageConfig rewrites to get the resolved package source
		resolvedPackage := pdep.Package
		if _, rewrittenPath, err := m.config.RewritePath(ctx, pdep.Package); err == nil && rewrittenPath != "" {
			resolvedPackage = rewrittenPath
		}
		pdep.ResolvedPackage = resolvedPackage

		sources[i] = pdep
	}

	found = len(sources)

	// Get the lock.
	lock := &v1beta1.Lock{}

	err = m.client.Get(ctx, types.NamespacedName{Name: lockName}, lock)
	if kerrors.IsNotFound(err) {
		lock.Name = lockName
		err = m.client.Create(ctx, lock, &client.CreateOptions{})
	}

	if err != nil {
		return found, installed, invalid, errors.Wrap(err, errGetOrCreateLock)
	}

	prRef, err := name.ParseReference(pr.GetSource(), name.StrictValidation)
	if err != nil {
		return found, installed, invalid, err
	}

	d := m.newDag()

	implied, err := d.Init(v1beta1.ToNodes(lock.Packages...))
	if err != nil {
		return found, installed, invalid, errors.Wrap(err, errInitDAG)
	}

	source := xpkg.ParsePackageSourceFromReference(prRef)

	// Apply ImageConfig rewrites to get the resolved source (package name only, no version)
	resolvedSource := source
	// We need to pass the full image reference (with version) to RewritePath because
	// ImageConfig match prefixes can include tags (e.g., "example.org/repo:v1.0.0").
	// If we only passed the package name, such tag-specific rewrites wouldn't match.
	fullRef := fmt.Sprintf("%s:%s", source, prRef.Identifier())
	if _, rewritten, err := m.config.RewritePath(ctx, fullRef); err == nil && rewritten != "" {
		// Extract just the package name from the rewritten path (remove version/tag)
		if ref, err := name.ParseReference(rewritten, name.StrictValidation); err == nil {
			resolvedSource = ref.Context().String()
		}
	}

	// NOTE(hasheddan): consider adding health of package to lock so that it can
	// be rolled up to any dependent packages.
	self := v1beta1.LockPackage{
		APIVersion:     ptr.To(m.packageType.GroupVersion().String()),
		Kind:           ptr.To(m.packageType.Kind),
		Name:           pr.GetName(),
		Source:         source,
		Version:        prRef.Identifier(),
		ResolvedSource: resolvedSource,
		Dependencies:   sources,
	}

	// Delete packages in lock with same name and distinct source This is a
	// corner case when source is updated but image SHA is not (i.e.
	// relocate same image to another registry)
	for _, lp := range lock.Packages {
		if self.Name == lp.Name && self.Type == lp.Type && self.Source != lp.Identifier() {
			m.log.Debug("Package with same name and type but different source exists in lock. Removing it.",
				"name", lp.Name,
				"type", ptr.Deref(lp.Type, "Unknown"),
				"old-source", lp.Identifier(),
				"new-source", self.Source,
			)

			if err := m.RemoveSelf(ctx, pr); err != nil {
				return found, installed, invalid, err
			}
			// refresh the lock to be in sync with the contents
			if err = m.client.Get(ctx, types.NamespacedName{Name: lockName}, lock); err != nil {
				return found, installed, invalid, err
			}

			break
		}
	}

	prExists := false

	// Check if package exists and update if needed.
	for i := range lock.Packages {
		if lock.Packages[i].Name != pr.GetName() {
			continue
		}

		// The package exists in the lock, and matches our desired
		// state.
		if LockPackagesEqual(&lock.Packages[i], &self) {
			prExists = true
			break
		}

		// The package exists in the lock but we need to update it, e.g.
		// because a new ImageConfig affects its resolved packages. Just
		// delete it and let the !prExists case below add the updated
		// version.
		lock.Packages = slices.Delete(lock.Packages, i, i+1)
		break
	}

	// If we don't exist in lock then we should add self.
	if !prExists {
		lock.Packages = append(lock.Packages, self)
		if err := m.client.Update(ctx, lock); err != nil {
			return found, installed, invalid, err
		}
		// Package may exist in the graph as a dependency, or may not exist at
		// all. We need to either convert it to a full node or add it.
		d.AddOrUpdateNodes(&self)

		// If any direct dependencies are missing we skip checking for
		// transitive ones.
		var missing []dag.Node

		for _, dep := range self.Dependencies {
			if d.NodeExists(dep.Identifier()) {
				installed++
				continue
			}

			missing = append(missing, &dep)
		}

		if installed != found {
			return found, installed, invalid, errors.Errorf(errFmtMissingDependencies, NDependenciesAndSomeMore(3, missing))
		}
	}

	// Build a dependency tree to detect missing dependencies.
	//
	// Background: When we initialized the DAG with d.Init(lock.Packages), some
	// dependencies referenced by lock packages might not have existed as actual
	// packages in the lock. The DAG automatically created placeholder nodes for
	// these missing dependencies and returned them as "implied" nodes.
	//
	// Now we need to check if any of these missing dependencies are actually
	// reachable from our current package through the dependency graph:
	//
	// 1. TraceNode builds a tree of all dependencies reachable from this package
	// 2. We check if any "implied" (missing) dependencies appear in this tree
	// 3. If they do, it means they're required but missing from the lock
	// 4. Counter-intuitively, finding an implied dependency in the tree means
	//    it's "missing" - because implied nodes represent dependencies that
	//    should exist but don't have actual package entries in the lock
	//
	// The "installed--" logic decrements the count because we initially assume
	// all dependencies in the tree are installed, but implied ones aren't really.
	tree, err := d.TraceNode(self.ResolvedSource)
	if err != nil {
		return found, installed, invalid, err
	}

	found = len(tree)
	installed = found
	var missing []dag.Node

	for _, imp := range implied {
		if _, ok := tree[imp.Identifier()]; ok {
			installed--

			missing = append(missing, imp)
		}
	}

	if len(missing) != 0 {
		return found, installed, invalid, errors.Errorf(errFmtMissingDependencies, NDependenciesAndSomeMore(3, missing))
	}

	// All of our dependencies and transitive dependencies must exist. Check
	// that neighbors have valid versions.
	var invalidDeps []string

	for _, dep := range self.Dependencies {
		n, err := d.GetNode(dep.Identifier())
		if err != nil {
			return found, installed, invalid, errors.New(errDependencyNotInGraph)
		}

		lp, ok := n.(*v1beta1.LockPackage)
		if !ok {
			return found, installed, invalid, errors.New(errDependencyNotLockPackage)
		}

		// Check if the constraint is a digest, if so, compare it directly.
		if d, err := conregv1.NewHash(dep.Constraints); err == nil {
			if lp.Version != d.String() {
				return found, installed, invalid, errors.Errorf("existing package %s@%s is incompatible with constraint %s", lp.Identifier(), lp.Version, strings.TrimSpace(dep.Constraints))
			}

			continue
		}

		c, err := semver.NewConstraint(dep.Constraints)
		if err != nil {
			return found, installed, invalid, err
		}

		v, err := semver.NewVersion(lp.Version)
		if err != nil {
			return found, installed, invalid, err
		}

		if !c.Check(v) {
			s := fmt.Sprintf("existing package %s@%s", lp.Identifier(), lp.Version)
			if dep.Constraints != "" {
				s = fmt.Sprintf("%s is incompatible with constraint %s", s, strings.TrimSpace(dep.Constraints))
			}

			invalidDeps = append(invalidDeps, s)
		}
	}

	invalid = len(invalidDeps)
	if invalid > 0 {
		return found, installed, invalid, errors.Errorf(errFmtIncompatibleDependency, strings.Join(invalidDeps, "; "))
	}

	return found, installed, invalid, nil
}

// RemoveSelf removes a package from the lock.
func (m *PackageDependencyManager) RemoveSelf(ctx context.Context, pr v1.PackageRevision) error {
	// Get the lock.
	lock := &v1beta1.Lock{}

	err := m.client.Get(ctx, types.NamespacedName{Name: lockName}, lock)
	if kerrors.IsNotFound(err) {
		// If lock does not exist then we don't need to remove self.
		return nil
	}

	if err != nil {
		return err
	}

	// Find self and remove. If we don't exist, its a no-op.
	for i, lp := range lock.Packages {
		if lp.Name == pr.GetName() {
			m.log.Debug("Removing package revision from lock", "name", lp.Name)

			lock.Packages = append(lock.Packages[:i], lock.Packages[i+1:]...)

			return m.client.Update(ctx, lock)
		}
	}

	return nil
}

// NDependenciesAndSomeMore returns the first n dependencies in detail, and a
// summary of how many more exist.
func NDependenciesAndSomeMore(n int, d []dag.Node) string {
	out := make([]string, len(d))
	for i := range d {
		if d[i].GetConstraints() == "" {
			out[i] = fmt.Sprintf("%q", d[i].Identifier())
			continue
		}

		out[i] = fmt.Sprintf("%q (%s)", d[i].Identifier(), d[i].GetConstraints())
	}

	return resource.StableNAndSomeMore(n, out)
}

// LockPackagesEqual compares two LockPackages for equality, focusing on fields
// that can change due to ImageConfig rewrites (ResolvedSource and dependency ResolvedPackage fields).
func LockPackagesEqual(a, b *v1beta1.LockPackage) bool {
	if a.ResolvedSource != b.ResolvedSource {
		return false
	}

	if len(a.Dependencies) != len(b.Dependencies) {
		return false
	}

	for i, depA := range a.Dependencies {
		depB := b.Dependencies[i]
		if depA.ResolvedPackage != depB.ResolvedPackage {
			return false
		}
	}

	return true
}
