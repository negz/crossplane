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
	"context"
	"sort"

	"github.com/google/go-containerregistry/pkg/authn/k8schain"
	"github.com/google/go-containerregistry/pkg/name"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/durationpb"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/json"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/crossplane/crossplane-runtime/pkg/errors"
	"github.com/crossplane/crossplane-runtime/pkg/event"
	"github.com/crossplane/crossplane-runtime/pkg/meta"
	"github.com/crossplane/crossplane-runtime/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/pkg/resource"
	"github.com/crossplane/crossplane-runtime/pkg/resource/unstructured"
	"github.com/crossplane/crossplane-runtime/pkg/resource/unstructured/composed"
	"github.com/crossplane/crossplane-runtime/pkg/resource/unstructured/composite"

	iov1alpha1 "github.com/crossplane/crossplane/apis/apiextensions/fn/io/v1alpha1"
	fnv1alpha1 "github.com/crossplane/crossplane/apis/apiextensions/fn/proto/v1alpha1"
	v1 "github.com/crossplane/crossplane/apis/apiextensions/v1"
)

// Error strings.
const (
	errFetchXRConnectionDetails = "cannot fetch composite resource connection details"
	errGetExistingCDs           = "cannot get existing composed resources"
	errImgPullCfg               = "cannot get xfn image pull config"
	errBuildFunctionIOObserved  = "cannot build FunctionIO observed state"
	errBuildFunctionIODesired   = "cannot build initial FunctionIO desired state"
	errMarshalXR                = "cannot marshal composite resource"
	errMarshalCD                = "cannot marshal composed resource"
	errParseImage               = "cannot parse image reference"
	errGetServiceAccount        = "cannot get image pull secrets from service account"
	errGetImagePullSecret       = "cannot get image pull secret"
	errNewKeychain              = "cannot create a new in-cluster registry authentication keychain"
	errResolveKeychain          = "cannot resolve in-cluster registry authentication keychain"
	errAuthCfg                  = "cannot get in-cluster registry authentication credentials"
	errPatchAndTransform        = "cannot patch and transform"
	errRunFunctionPipeline      = "cannot run Composition Function pipeline"
	errDeleteUndesiredCDs       = "cannot delete undesired composed resources"
	errApplyXR                  = "cannot apply composite resource"
	errObserveCDs               = "cannot observe composed resources"
	errAnonymousCD              = "encountered composed resource without required \"" + AnnotationKeyCompositionResourceName + "\" annotation"
	errUnmarshalDesiredXR       = "cannot unmarshal desired composite resource from FunctionIO"
	errUnmarshalDesiredCD       = "cannot unmarshal desired composed resource from FunctionIO"
	errMarshalFnIO              = "cannot marshal input FunctionIO"
	errDialRunner               = "cannot dial container runner"
	errApplyRunFunctionOption   = "cannot apply run function option"
	errRunFnContainer           = "cannot run container"
	errCloseRunner              = "cannot close connection to container runner"
	errUnmarshalFnIO            = "cannot unmarshal output FunctionIO"
	errFatalResult              = "fatal function pipeline result"

	errFmtApplyCD                  = "cannot apply composed resource %q"
	errFmtFetchCDConnectionDetails = "cannot fetch connection details for composed resource %q (a %s named %s)"
	errFmtRenderXR                 = "cannot render composite resource from composed resource %q (a %s named %s)"
	errFmtRunFn                    = "cannot run function %q"

	errFmtUnsupportedFnType        = "unsupported function type %q"
	errFmtParseDesiredCD           = "cannot parse desired composed resource %q from FunctionIO"
	errFmtDeleteCD                 = "cannot delete composed resource %q (a %s named %s)"
	errFmtReadiness                = "cannot determine whether composed resource %q (a %s named %s) is ready"
	errFmtExtractConnectionDetails = "cannot extract connection details from composed resource %q (a %s named %s)"
)

// DefaultTarget is the default function runner target endpoint.
const DefaultTarget = "unix-abstract:crossplane/fn/default.sock"

// A PTFComposer (i.e. Patch, Transform, and Function Composer) supports
// composing resources using both Patch and Transform (P&T) logic and a pipeline
// of Composition Functions. Callers may mix P&T with Composition Functions or
// use only one or the other. It does not support anonymous, unnamed resource
// templates and will panic if it encounters one.
type PTFComposer struct {
	client    resource.ClientApplicator
	composite ptfComposite
	composed  ptfComposed
	container ContainerFunctionRunner
}

type ptfComposite struct {
	managed.ConnectionDetailsFetcher
	ComposedResourceObserver
	ComposedResourceDeleter
}

// A DryRunRenderer renders a resource by submitting it to the API server via a
// dry-run create.
type DryRunRenderer interface {
	DryRunRender(ctx context.Context, cd resource.Object) error
}

type ptfComposed struct {
	DryRunRenderer
	ReadinessChecker
	ConnectionDetailsExtractor
}

// An ObservedResource is an existing, observed composed resource.
type ObservedResource struct {
	Resource          resource.Composed
	ConnectionDetails managed.ConnectionDetails
}

// A DesiredResource is a desired composed resource. It may or may not exist.
type DesiredResource struct {
	Resource        resource.Composed
	ReadinessChecks []ReadinessCheck
	ExtractConfigs  []ConnectionDetailExtractConfig
}

// ObservedResources are existing, observed composed resources.
type ObservedResources map[ResourceName]ObservedResource

// DesiredResources are desired composed resources. They may or may not exist.
type DesiredResources map[ResourceName]DesiredResource

// ComposedResourceTemplates are the P&T templates for composed resources.
type ComposedResourceTemplates map[ResourceName]v1.ComposedTemplate

// A ComposedResourceObserver observes existing composed resources.
type ComposedResourceObserver interface {
	ObserveComposedResources(ctx context.Context, xr resource.Composite) (ObservedResources, error)
}

// A ComposedResourceObserverFn observes existing composed resources.
type ComposedResourceObserverFn func(ctx context.Context, xr resource.Composite) (ObservedResources, error)

// ObserveComposedResources observes existing composed resources.
func (fn ComposedResourceObserverFn) ObserveComposedResources(ctx context.Context, xr resource.Composite) (ObservedResources, error) {
	return fn(ctx, xr)
}

// A ComposedResourceDeleter deletes existing composed resources.
type ComposedResourceDeleter interface {
	DeleteComposedResources(ctx context.Context, owner metav1.Object, cds ObservedResources) error
}

// A ComposedResourceDeleterFn deletes composed resources (and their state).
type ComposedResourceDeleterFn func(ctx context.Context, owner metav1.Object, cds ObservedResources) error

// DeleteComposedResources deletes composed resources (and their state).
func (fn ComposedResourceDeleterFn) DeleteComposedResources(ctx context.Context, owner metav1.Object, cds ObservedResources) error {
	return fn(ctx, owner, cds)
}

// A PTFComposerOption is used to configure a PTFComposer.
type PTFComposerOption func(*PTFComposer)

// WithCompositeConnectionDetailsFetcher configures how the PTFComposer should
// get the composite resource's connection details.
func WithCompositeConnectionDetailsFetcher(f managed.ConnectionDetailsFetcher) PTFComposerOption {
	return func(p *PTFComposer) {
		p.composite.ConnectionDetailsFetcher = f
	}
}

// WithComposedResourceObserver configures how the PTFComposer should get existing
// composed resources.
func WithComposedResourceObserver(g ComposedResourceObserver) PTFComposerOption {
	return func(p *PTFComposer) {
		p.composite.ComposedResourceObserver = g
	}
}

// WithComposedResourceDeleter configures how the PTFComposer should delete
// undesired composed resources.
func WithComposedResourceDeleter(d ComposedResourceDeleter) PTFComposerOption {
	return func(p *PTFComposer) {
		p.composite.ComposedResourceDeleter = d
	}
}

// WithDryRunRenderer configures how the PTFComposer should dry-run render
// composed resources - i.e. by submitting them to the API server to generate a
// name for them.
func WithDryRunRenderer(r DryRunRenderer) PTFComposerOption {
	return func(p *PTFComposer) {
		p.composed.DryRunRenderer = r
	}
}

// WithReadinessChecker configures how the PTFComposer checks composed resource
// readiness.
func WithReadinessChecker(c ReadinessChecker) PTFComposerOption {
	return func(p *PTFComposer) {
		p.composed.ReadinessChecker = c
	}
}

// WithConnectionDetailsExtractor configures how a PTComposer extracts XR
// connection details from a composed resource.
func WithConnectionDetailsExtractor(c ConnectionDetailsExtractor) PTFComposerOption {
	return func(p *PTFComposer) {
		p.composed.ConnectionDetailsExtractor = c
	}
}

// WithContainerFunctionRunner configures how the PTFComposer should run
// containerized Composition Functions.
func WithContainerFunctionRunner(r ContainerFunctionRunner) PTFComposerOption {
	return func(p *PTFComposer) {
		p.container = r
	}
}

// NewPTFComposer returns a new Composer that supports composing resources using
// both Patch and Transform (P&T) logic and a pipeline of Composition Functions.
func NewPTFComposer(kube client.Client, o ...PTFComposerOption) *PTFComposer {
	// TODO(negz): Can we avoid double-wrapping if the supplied client is
	// already wrapped? Or just do away with unstructured.NewClient completely?
	kube = unstructured.NewClient(kube)

	f := NewSecretConnectionDetailsFetcher(kube)

	c := &PTFComposer{
		client: resource.ClientApplicator{Client: kube, Applicator: resource.NewAPIPatchingApplicator(kube)},

		composite: ptfComposite{
			ConnectionDetailsFetcher: f,
			ComposedResourceObserver: NewExistingComposedResourceObserver(kube, f),
			ComposedResourceDeleter:  NewUndesiredComposedResourceDeleter(kube),
		},

		composed: ptfComposed{
			DryRunRenderer:             NewAPIDryRunRenderer(kube),
			ReadinessChecker:           ReadinessCheckerFn(IsReady),
			ConnectionDetailsExtractor: ConnectionDetailsExtractorFn(ExtractConnectionDetails),
		},

		container: ContainerFunctionRunnerFn(RunFunction),
	}

	for _, fn := range o {
		fn(c)
	}

	return c
}

// Compose resources using both either the Patch & Transform style resources
// array, the functions array, or both.
func (c *PTFComposer) Compose(ctx context.Context, xr resource.Composite, req CompositionRequest) (CompositionResult, error) { //nolint:gocyclo // We probably don't want any further abstraction for the sake of reduced complexity.
	xrConnDetails, err := c.composite.FetchConnection(ctx, xr)
	if err != nil {
		return CompositionResult{}, errors.Wrap(err, errFetchXRConnectionDetails)
	}

	observed, err := c.composite.ObserveComposedResources(ctx, xr)
	if err != nil {
		return CompositionResult{}, errors.Wrap(err, errGetExistingCDs)
	}

	// Inline PatchSets before composing resources.
	cts, err := ComposedTemplates(req.Revision.Spec.PatchSets, req.Revision.Spec.Resources)
	if err != nil {
		return CompositionResult{}, errors.Wrap(err, errInline)
	}

	// We build a map of resource name to composed resource template so we can
	// later lookup the template (if any) for a desired resource after the
	// Composition Function pipeline has run.
	templates := ComposedResourceTemplates{}
	for _, t := range cts {
		// It's safe to assume *t.Name will never be nil - we disable the
		// PTFComposer if any composed resource template is not named.
		templates[ResourceName(*t.Name)] = t
	}

	// If we have an environment, run all environment patches before composing
	// resources.
	if req.Environment != nil && req.Revision.Spec.Environment != nil {
		for i, p := range req.Revision.Spec.Environment.Patches {
			if err := ApplyEnvironmentPatch(p, xr, req.Environment); err != nil {
				return CompositionResult{}, errors.Wrapf(err, errFmtPatchEnvironment, i)
			}
		}
	}

	events := []event.Event{}
	desired := DesiredResources{}

	// TODO(negz): We probably need to iterate over this in order to maintain
	// the behaviour of the PTComposer, but once Functions come into play
	// everything is randomized. We build a FunctionIO with random resource
	// order. Don't do that?

	// Note that iterating over composed templates in the order they're
	// specified is intentional.
	for i := range cts {
		ct := cts[i]
		name := ResourceName(*ct.Name)

		cd := composed.New()

		if err := RenderJSON(cd, ct.Base.Raw); err != nil {
			// TODO(negz): Make this error string a constant.
			return CompositionResult{}, errors.Wrap(err, "cannot render composed resource from template base")
		}

		// Failures to patch aren't terminal - we just emit a warning event and
		// move on. This is because patches often fail because other patches
		// need to happen first in order for them to succeed. If we returned an
		// error when a patch failed we might never reach the patch that would
		// unblock it.

		if err := RenderFromEnvironmentPatches(cd, req.Environment, ct.Patches); err != nil {
			events = append(events, event.Warning(reasonCompose, errors.Wrapf(err, errFmtResourceName, name)))
			continue
		}

		if err := RenderFromCompositePatches(cd, xr, ct.Patches); err != nil {
			events = append(events, event.Warning(reasonCompose, errors.Wrapf(err, errFmtResourceName, name)))
			continue
		}

		if err := RenderToCompositePatches(xr, cd, ct.Patches); err != nil {
			events = append(events, event.Warning(reasonCompose, errors.Wrapf(err, errFmtResourceName, name)))
			continue
		}

		desired[name] = DesiredResource{
			Resource:        cd,
			ReadinessChecks: ReadinessChecksFromComposedTemplate(&ct),
			ExtractConfigs:  ExtractConfigsFromComposedTemplate(&ct),
		}
	}

	// TODO(negz): Should we try to build the FunctionIO with a stable order? I
	// could imagine being able to cache Function responses for identical
	// FunctionIOs in future. Presumably observed resources will change a lot
	// though - e.g. resourceVersion etc.

	// Build observed state to be passed to our Composition Function pipeline.
	o, err := FunctionIOObserved(xr, xrConnDetails, observed)
	if err != nil {
		return CompositionResult{}, errors.Wrap(err, errBuildFunctionIOObserved)
	}

	// Build the initial desired state to be passed to our Composition Function
	// pipeline. It's expected that each function in the pipeline will mutate
	// this state. It includes any desired state accumulated by the P&T logic.
	d, err := FunctionIODesired(xr, xrConnDetails, desired)
	if err != nil {
		return CompositionResult{}, errors.Wrap(err, errBuildFunctionIODesired)
	}

	// Run Composition Functions, updating the composition state accordingly.
	// Note that this will replace state.Composite with a new object that was
	// unmarshalled from the function pipeline's desired state.
	r := make([]iov1alpha1.Result, 0)
	for _, fn := range req.Revision.Spec.Functions {
		switch fn.Type {
		case v1.FunctionTypeContainer:
			fnio, err := c.container.RunFunction(ctx, &iov1alpha1.FunctionIO{Config: fn.Config, Observed: o, Desired: d, Results: r}, fn.Container)
			if err != nil {
				return CompositionResult{}, errors.Wrapf(err, errFmtRunFn, fn.Name)
			}
			// We require each function to pass through any results and desired
			// state from previous functions in the pipeline that they're
			// unconcerned with, as well as their own results and desired state.
			// We pass all functions the same observed state, since it should
			// represent the state before the function pipeline started.
			d = fnio.Desired
			r = fnio.Results
		default:
			return CompositionResult{}, errors.Wrapf(errors.Errorf(errFmtUnsupportedFnType, fn.Type), errFmtRunFn, fn.Name)
		}
	}

	// Results of fatal severity stop the Composition process. Normal or warning
	// results are accumulated to be emitted as events by the Reconciler.
	for _, rs := range r {
		switch rs.Severity {
		case iov1alpha1.SeverityFatal:
			return CompositionResult{}, errors.Wrap(errors.New(rs.Message), errFatalResult)
		case iov1alpha1.SeverityWarning:
			events = append(events, event.Warning(reasonCompose, errors.New(rs.Message)))
		case iov1alpha1.SeverityNormal:
			events = append(events, event.Normal(reasonCompose, rs.Message))
		}
	}

	// TODO(negz): Raise an issue to capture the difference between the
	// documented and actual bootstrap behaviour.

	// TODO(negz): Are we bootstrapping the desired resource state? If we don't,
	// then a nop function is going to return this as empty. Perhaps we should
	// assume an empty desired block means no function wanted to do anything?
	// e.g. Validation-only functions just returned warnings/errors.
	xr = composite.New()
	if err := RenderJSON(xr, d.Composite.Resource.Raw); err != nil {
		return CompositionResult{}, errors.Wrap(err, errUnmarshalDesiredXR)
	}

	for i := range d.Resources {
		dr := d.Resources[i]

		cd := composed.New()
		if err := RenderJSON(cd, dr.Resource.Raw); err != nil {
			return CompositionResult{}, errors.Wrap(err, errUnmarshalDesiredCD)
		}

		desired[ResourceName(dr.Name)] = DesiredResource{
			Resource:        cd,
			ReadinessChecks: append(desired[ResourceName(dr.Name)].ReadinessChecks, ReadinessChecksFromDesiredResource(&dr)...),
			ExtractConfigs:  append(desired[ResourceName(dr.Name)].ExtractConfigs, ExtractConfigsFromDesiredResource(&dr)...),
		}
	}

	for name, cd := range desired {
		if err := RenderComposedResourceMetadata(cd.Resource, xr, name); err != nil {
			// TODO(negz): Make this error a constant.
			return CompositionResult{}, errors.Wrap(err, "cannot render composed resource metadata")
		}

		if err := c.composed.DryRunRender(ctx, cd.Resource); err != nil {
			return CompositionResult{}, errors.Wrap(err, "cannot dry-run render composed resource")
		}
	}

	for _, cd := range d.Composite.ConnectionDetails {
		xrConnDetails[cd.Name] = []byte(cd.Value)
	}

	// Garbage collect any observed resources that aren't part of our final
	// desired state. We must do this before we update the XR's resource
	// references to ensure that we don't forget and leak them if a delete
	// fails.
	del := ObservedResources{}
	for name, cd := range observed {
		if _, ok := desired[name]; !ok {
			del[name] = cd
		}
	}
	if err := c.composite.DeleteComposedResources(ctx, xr, del); err != nil {
		return CompositionResult{}, errors.Wrap(err, errDeleteUndesiredCDs)
	}

	// Record references to all desired composed resources.
	UpdateResourceRefs(xr, desired)

	// The supplied options ensure we merge rather than replace arrays and
	// objects for which a merge configuration has been specified.
	//
	// Note that at this point state.Composite should be a new object - not the
	// xr that was passed to this Compose method. If this call to Apply changes
	// the XR in the API server (i.e. if it's not a no-op) the xr object that
	// was passed to this method will have a stale meta.resourceVersion. This
	// Subsequent attempts to update that object will therefore fail. This
	// should be okay; the caller should keep trying until this is a no-op.
	ao := mergeOptions(filterPatches(allPatches(cts), patchTypesToXR()...))
	if err := c.client.Apply(ctx, xr, ao...); err != nil {
		return CompositionResult{}, errors.Wrap(err, errApplyXR)
	}

	// TODO(negz): Does it matter that we're applying these in random order?
	// It's hard to guarantee stable order as we go through the FunctionIO
	// pipeline - it would require all functions to guarantee it. We could at
	// least sort by name?

	for name, cd := range desired {
		ao := []resource.ApplyOption{resource.MustBeControllableBy(xr.GetUID())}

		// If this desired resource is associated with a P&T composed template,
		// use its merge options. These determine whether objects should be
		// merged or replaced, and whether arrays should be appended or
		// replaced, when patching from an XR to a composed resource.
		//
		// TODO(negz): What about desired resources that aren't associated with
		// a P&T template? Hopefully in future this goes away entirely, and is
		// replaced with server-side-apply.
		// https://github.com/crossplane/crossplane/issues/4047
		if t, ok := templates[name]; ok {
			ao = append(ao, mergeOptions(filterPatches(t.Patches, patchTypesFromXR()...))...)
		}

		if err := c.client.Apply(ctx, cd.Resource, ao...); err != nil {
			return CompositionResult{}, errors.Wrapf(err, errFmtApplyCD, name)
		}
	}

	out := make([]ComposedResource, 0, len(desired))
	for name, cd := range desired {
		if _, ok := observed[name]; !ok {
			// There's no point trying to extract connection details from or
			// check the readiness of a resource that doesn't exist yet.
			continue
		}

		connDetails, err := c.composed.ExtractConnection(cd.Resource, observed[name].ConnectionDetails, cd.ExtractConfigs...)
		if err != nil {
			return CompositionResult{}, errors.Wrapf(err, errFmtExtractConnectionDetails, name, cd.Resource.GetObjectKind().GroupVersionKind().Kind, cd.Resource.GetName())
		}
		for key, val := range connDetails {
			xrConnDetails[key] = val
		}

		ready, err := c.composed.IsReady(ctx, cd.Resource, cd.ReadinessChecks...)
		if err != nil {
			return CompositionResult{}, errors.Wrapf(err, errFmtReadiness, name, cd.Resource.GetObjectKind().GroupVersionKind().Kind, cd.Resource.GetName())
		}

		out = append(out, ComposedResource{ResourceName: name, Ready: ready})
	}

	return CompositionResult{ConnectionDetails: xrConnDetails, Composed: out, Events: events}, nil
}

func allPatches(ct []v1.ComposedTemplate) []v1.Patch {
	out := make([]v1.Patch, 0, len(ct))
	for _, t := range ct {
		out = append(out, t.Patches...)
	}
	return out
}

// An ExistingComposedResourceObserver uses an XR's resource references to load
// any existing composed resources from the API server. It also loads their
// connection details.
type ExistingComposedResourceObserver struct {
	resource client.Reader
	details  managed.ConnectionDetailsFetcher
}

// NewExistingComposedResourceObserver returns a ComposedResourceGetter that
// fetches an XR's existing composed resources.
func NewExistingComposedResourceObserver(c client.Reader, f managed.ConnectionDetailsFetcher) *ExistingComposedResourceObserver {
	return &ExistingComposedResourceObserver{resource: c, details: f}
}

// ObserveComposedResources begins building composed resource state by
// fetching any existing composed resources referenced by the supplied composite
// resource, as well as their connection details.
func (g *ExistingComposedResourceObserver) ObserveComposedResources(ctx context.Context, xr resource.Composite) (ObservedResources, error) {
	ors := ObservedResources{}

	for _, ref := range xr.GetResourceReferences() {
		// The PTComposer writes references to resources that it didn't actually
		// render or create. It has to create these placeholder refs because it
		// supports anonymous (unnamed) resource templates; it needs to be able
		// associate entries a Composition's spec.resources array with entries
		// in an XR's spec.resourceRefs array by their index. These references
		// won't have a name - we won't be able to get them because they don't
		// reference a resource that actually exists.
		if ref.Name == "" {
			continue
		}

		r := composed.New(composed.FromReference(ref))
		nn := types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}
		err := g.resource.Get(ctx, nn, r)
		if kerrors.IsNotFound(err) {
			// We believe we created this resource, but it doesn't exist.
			continue
		}
		if err != nil {
			return nil, errors.Wrap(err, errGetComposed)
		}

		if c := metav1.GetControllerOf(r); c != nil && c.UID != xr.GetUID() {
			// If we don't control this resource we just pretend it doesn't
			// exist. We might try to render and re-create it later, but that
			// should fail because we check the controller ref there too.
			continue
		}

		name := GetCompositionResourceName(r)
		if name == "" {
			return nil, errors.New(errAnonymousCD)
		}

		conn, err := g.details.FetchConnection(ctx, r)
		if err != nil {
			return nil, errors.Wrapf(err, errFmtFetchCDConnectionDetails, name, r.GetKind(), r.GetName())
		}

		ors[name] = ObservedResource{
			Resource:          r,
			ConnectionDetails: conn,
		}

	}

	return ors, nil
}

// FunctionIOObserved builds observed state for a FunctionIO from the XR and any
// existing composed resources. This reflects the observed state of the world
// before any Composition (P&T or function-based) has taken place.
func FunctionIOObserved(xr resource.Composite, xc managed.ConnectionDetails, ors ObservedResources) (iov1alpha1.Observed, error) {
	raw, err := json.Marshal(xr)
	if err != nil {
		return iov1alpha1.Observed{}, errors.Wrap(err, errMarshalXR)
	}

	rs := runtime.RawExtension{Raw: raw}
	econn := make([]iov1alpha1.ExplicitConnectionDetail, 0, len(xc))
	for n, v := range xc {
		econn = append(econn, iov1alpha1.ExplicitConnectionDetail{Name: n, Value: string(v)})
	}

	oxr := iov1alpha1.ObservedComposite{Resource: rs, ConnectionDetails: econn}

	ocds := make([]iov1alpha1.ObservedResource, 0, len(ors))
	for name, or := range ors {

		raw, err := json.Marshal(or.Resource)
		if err != nil {
			return iov1alpha1.Observed{}, errors.Wrap(err, errMarshalCD)
		}

		rs := runtime.RawExtension{Raw: raw}

		ecds := make([]iov1alpha1.ExplicitConnectionDetail, 0, len(or.ConnectionDetails))
		for n, v := range or.ConnectionDetails {
			ecds = append(ecds, iov1alpha1.ExplicitConnectionDetail{Name: n, Value: string(v)})
		}

		ocds = append(ocds, iov1alpha1.ObservedResource{
			Name:              string(name),
			Resource:          rs,
			ConnectionDetails: ecds,
		})
	}

	return iov1alpha1.Observed{Composite: oxr, Resources: ocds}, nil
}

// FunctionIODesired builds the initial desired state for a FunctionIO from the XR
// and any existing or impending composed resources. This reflects the observed
// state of the world plus the initial desired state as built up by any P&T
// Composition that has taken place.
func FunctionIODesired(xr resource.Composite, xc managed.ConnectionDetails, drs DesiredResources) (iov1alpha1.Desired, error) {
	raw, err := json.Marshal(xr)
	if err != nil {
		return iov1alpha1.Desired{}, errors.Wrap(err, errMarshalXR)
	}

	rs := runtime.RawExtension{Raw: raw}
	econn := make([]iov1alpha1.ExplicitConnectionDetail, 0, len(xc))
	for n, v := range xc {
		econn = append(econn, iov1alpha1.ExplicitConnectionDetail{Name: n, Value: string(v)})
	}

	dxr := iov1alpha1.DesiredComposite{Resource: rs, ConnectionDetails: econn}

	dcds := make([]iov1alpha1.DesiredResource, 0, len(drs))
	for name, dr := range drs {
		raw, err := json.Marshal(dr.Resource)
		if err != nil {
			return iov1alpha1.Desired{}, errors.Wrap(err, errMarshalCD)
		}
		dcds = append(dcds, iov1alpha1.DesiredResource{
			Name:     string(name),
			Resource: runtime.RawExtension{Raw: raw},

			// TODO(negz): Should we include any connection details and
			// readiness checks from the P&T templates here? Doing so would
			// allow the composition function pipeline to alter them - i.e. to
			// remove details/checks. Currently the two are additive - we take
			// all the connection detail extraction configs and readiness checks
			// from the P&T process then append any from the function process.
		})
	}

	return iov1alpha1.Desired{Composite: dxr, Resources: dcds}, nil
}

// A ContainerFunctionRunnerOption modifies the behavior of a
// ContainerFunctionRunner by mutating the supplied RunFunctionRequest.
type ContainerFunctionRunnerOption func(ctx context.Context, fn *v1.ContainerFunction, r *fnv1alpha1.RunFunctionRequest) error

// WithKubernetesAuthentication configures a ContainerFunctionRunner to use
// "Kubernetes" authentication to pull images from a private OCI registry.
// Kubernetes authentication emulates how the Kubelet pulls private images.
// Specifically, it:
//
// 1. Loads credentials from the Docker config file, if any.
// 2. Loads credentials from the supplied service account's image pull secrets.
// 3. Loads credentials from the function's image pull secrets.
// 4. Loads credentials using the GKE, EKS, or AKS credentials helper.
func WithKubernetesAuthentication(c client.Reader, namespace, serviceAccount, registry string) ContainerFunctionRunnerOption {
	return func(ctx context.Context, fn *v1.ContainerFunction, r *fnv1alpha1.RunFunctionRequest) error {

		sa := &corev1.ServiceAccount{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: serviceAccount}, sa); err != nil {
			return errors.Wrap(err, errGetServiceAccount)
		}

		refs := make([]corev1.LocalObjectReference, 0, len(fn.ImagePullSecrets)+len(sa.ImagePullSecrets))
		refs = append(refs, sa.ImagePullSecrets...)
		refs = append(refs, fn.ImagePullSecrets...)

		ips := make([]corev1.Secret, len(refs))
		for i := range refs {
			if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: refs[i].Name}, &ips[i]); err != nil {
				return errors.Wrap(err, errGetImagePullSecret)
			}
		}

		// We use NewFromPullSecrets rather than NewInCluster so that we can use
		// our own client.Reader rather than the default in-cluster client. This
		// makes the option easier to test, ensures we re-use any existing
		// client caches, and allows us to run out-of-cluster.
		keychain, err := k8schain.NewFromPullSecrets(ctx, ips)
		if err != nil {
			return errors.Wrap(err, errNewKeychain)
		}

		ref, err := name.ParseReference(fn.Image, name.WithDefaultRegistry(registry))
		if err != nil {
			return errors.Wrap(err, errParseImage)
		}
		auth, err := keychain.Resolve(ref.Context())
		if err != nil {
			return errors.Wrap(err, errResolveKeychain)
		}
		a, err := auth.Authorization()
		if err != nil {
			return errors.Wrap(err, errAuthCfg)
		}

		if r.ImagePullConfig == nil {
			r.ImagePullConfig = &fnv1alpha1.ImagePullConfig{}
		}

		r.ImagePullConfig.Auth = &fnv1alpha1.ImagePullAuth{
			Username:      a.Username,
			Password:      a.Password,
			Auth:          a.Auth,
			IdentityToken: a.IdentityToken,
			RegistryToken: a.RegistryToken,
		}

		return nil
	}
}

// A ContainerFunctionRunner runs a containerized Composition Function.
type ContainerFunctionRunner interface {
	RunFunction(ctx context.Context, fnio *iov1alpha1.FunctionIO, fn *v1.ContainerFunction, o ...ContainerFunctionRunnerOption) (*iov1alpha1.FunctionIO, error)
}

// RunWithOptions returns a ContainerFunctionRunner that always runs using the
// supplied options.
type RunWithOptions struct {
	Runner  ContainerFunctionRunner
	Options []ContainerFunctionRunnerOption
}

// RunFunction runs a containerized Composition Function.
func (r RunWithOptions) RunFunction(ctx context.Context, fnio *iov1alpha1.FunctionIO, fn *v1.ContainerFunction, o ...ContainerFunctionRunnerOption) (*iov1alpha1.FunctionIO, error) {
	return r.Runner.RunFunction(ctx, fnio, fn, append(r.Options, o...)...)
}

// A ContainerFunctionRunnerFn runs a containerized Composition Function.
type ContainerFunctionRunnerFn func(ctx context.Context, fnio *iov1alpha1.FunctionIO, fn *v1.ContainerFunction, o ...ContainerFunctionRunnerOption) (*iov1alpha1.FunctionIO, error)

// RunFunction runs a containerized Composition Function.
func (fn ContainerFunctionRunnerFn) RunFunction(ctx context.Context, fnio *iov1alpha1.FunctionIO, fnc *v1.ContainerFunction, o ...ContainerFunctionRunnerOption) (*iov1alpha1.FunctionIO, error) {
	return fn(ctx, fnio, fnc, o...)
}

// RunFunction calls an external container function runner via gRPC.
func RunFunction(ctx context.Context, fnio *iov1alpha1.FunctionIO, fn *v1.ContainerFunction, o ...ContainerFunctionRunnerOption) (*iov1alpha1.FunctionIO, error) {
	in, err := yaml.Marshal(fnio)
	if err != nil {
		return nil, errors.Wrap(err, errMarshalFnIO)
	}

	target := DefaultTarget
	if fn.Runner != nil && fn.Runner.Endpoint != nil {
		target = *fn.Runner.Endpoint
	}

	conn, err := grpc.DialContext(ctx, target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, errors.Wrap(err, errDialRunner)
	}
	// Remember to close the connection, we are not deferring it to be able to properly handle errors,
	// without having to use a named return.

	req := &fnv1alpha1.RunFunctionRequest{
		Image:             fn.Image,
		Input:             in,
		ImagePullConfig:   ImagePullConfig(fn),
		RunFunctionConfig: RunFunctionConfig(fn),
	}

	for _, opt := range o {
		if err := opt(ctx, fn, req); err != nil {
			_ = conn.Close()
			return nil, errors.Wrap(err, errApplyRunFunctionOption)
		}
	}

	rsp, err := fnv1alpha1.NewContainerizedFunctionRunnerServiceClient(conn).RunFunction(ctx, req)
	if err != nil {
		// TODO(negz): Parse any gRPC status codes.
		_ = conn.Close()
		return nil, errors.Wrap(err, errRunFnContainer)
	}

	if err := conn.Close(); err != nil {
		return nil, errors.Wrap(err, errCloseRunner)
	}

	// TODO(negz): Sanity check this FunctionIO to ensure the function returned
	// a valid response. Does it contain at least a desired Composite resource?
	out := &iov1alpha1.FunctionIO{}
	return out, errors.Wrap(yaml.Unmarshal(rsp.Output, out), errUnmarshalFnIO)
}

// ImagePullConfig builds an ImagePullConfig for a FunctionIO.
func ImagePullConfig(fn *v1.ContainerFunction) *fnv1alpha1.ImagePullConfig {
	cfg := &fnv1alpha1.ImagePullConfig{}

	if fn.ImagePullPolicy != nil {
		switch *fn.ImagePullPolicy {
		case corev1.PullAlways:
			cfg.PullPolicy = fnv1alpha1.ImagePullPolicy_IMAGE_PULL_POLICY_ALWAYS
		case corev1.PullNever:
			cfg.PullPolicy = fnv1alpha1.ImagePullPolicy_IMAGE_PULL_POLICY_NEVER
		case corev1.PullIfNotPresent:
			fallthrough
		default:
			cfg.PullPolicy = fnv1alpha1.ImagePullPolicy_IMAGE_PULL_POLICY_IF_NOT_PRESENT
		}
	}
	return cfg
}

// RunFunctionConfig builds a RunFunctionConfig for a FunctionIO.
func RunFunctionConfig(fn *v1.ContainerFunction) *fnv1alpha1.RunFunctionConfig {
	out := &fnv1alpha1.RunFunctionConfig{}
	if fn.Timeout != nil {
		out.Timeout = durationpb.New(fn.Timeout.Duration)
	}
	if fn.Resources != nil {
		out.Resources = &fnv1alpha1.ResourceConfig{}
		if fn.Resources.Limits != nil {
			out.Resources.Limits = &fnv1alpha1.ResourceLimits{}
			if fn.Resources.Limits.CPU != nil {
				out.Resources.Limits.Cpu = fn.Resources.Limits.CPU.String()
			}
			if fn.Resources.Limits.Memory != nil {
				out.Resources.Limits.Memory = fn.Resources.Limits.Memory.String()
			}
		}
	}
	if fn.Network != nil {
		out.Network = &fnv1alpha1.NetworkConfig{}
		if fn.Network.Policy != nil {
			switch *fn.Network.Policy {
			case v1.ContainerFunctionNetworkPolicyIsolated:
				out.Network.Policy = fnv1alpha1.NetworkPolicy_NETWORK_POLICY_ISOLATED
			case v1.ContainerFunctionNetworkPolicyRunner:
				out.Network.Policy = fnv1alpha1.NetworkPolicy_NETWORK_POLICY_RUNNER
			}
		}
	}
	return out
}

// An UndesiredComposedResourceDeleter deletes undesired composed resources from
// the API server.
type UndesiredComposedResourceDeleter struct {
	client client.Writer
}

// NewUndesiredComposedResourceDeleter returns a ComposedResourceDeleter that
// deletes undesired composed resources from both the API server and Composition
// state.
func NewUndesiredComposedResourceDeleter(c client.Writer) *UndesiredComposedResourceDeleter {
	return &UndesiredComposedResourceDeleter{client: c}
}

// DeleteComposedResources deletes any composed resource that didn't come out the other
// end of the Composition Function pipeline (i.e. that wasn't in the final
// desired state after running the pipeline). Composed resources are deleted
// from both the supposed composition state and from the API server.
func (d *UndesiredComposedResourceDeleter) DeleteComposedResources(ctx context.Context, owner metav1.Object, ors ObservedResources) error {
	for name, cd := range ors {
		// No need to garbage collect resources that don't exist.
		if !meta.WasCreated(cd.Resource) {
			continue
		}

		// We want to garbage collect this resource, but we don't control it.
		if c := metav1.GetControllerOf(cd.Resource); c == nil || c.UID != owner.GetUID() {
			continue
		}

		if err := d.client.Delete(ctx, cd.Resource); resource.IgnoreNotFound(err) != nil {
			return errors.Wrapf(err, errFmtDeleteCD, name, cd.Resource.GetObjectKind().GroupVersionKind().Kind, cd.Resource.GetName())
		}
	}

	return nil
}

// UpdateResourceRefs updates the supplied state to ensure the XR references all
// composed resources that exist or are pending creation.
func UpdateResourceRefs(xr resource.ComposedResourcesReferencer, drs DesiredResources) {
	refs := make([]corev1.ObjectReference, 0, len(drs))
	for _, dr := range drs {
		ref := meta.ReferenceTo(dr.Resource, dr.Resource.GetObjectKind().GroupVersionKind())
		refs = append(refs, *ref)
	}

	// We want to ensure our refs are stable.
	sort.Slice(refs, func(i, j int) bool {
		ri, rj := refs[i], refs[j]
		return ri.APIVersion+ri.Kind+ri.Name < rj.APIVersion+rj.Kind+rj.Name
	})

	xr.SetResourceReferences(refs)
}
