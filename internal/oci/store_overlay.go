/*
Copyright 2022 The Crossplane Authors.

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

package oci

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	ociv1 "github.com/google/go-containerregistry/pkg/v1"
	runtime "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"

	"github.com/crossplane/crossplane-runtime/pkg/errors"
)

// Error strings.
const (
	errAdvanceTarball   = "cannot advance to next entry in tarball"
	errCloseFsTarball   = "cannot close function filesystem tarball"
	errFetchFnOCIConfig = "cannot fetch function OCI config"
	errUntarFn          = "cannot unarchive function tarball"
	errMkdir            = "cannot make directory"
	errSymlink          = "cannot create symlink"
	errEvalSymlinks     = "cannot evaluate symlinks"
	errOpenFile         = "cannot open file"
	errCopyFile         = "cannot copy file"
	errCloseFile        = "cannot close file"
	errCreateFile       = "cannot create file"
	errWriteFile        = "cannot write file"
	errMakeTmpDir       = "cannot make temporary directory"
	errParseImageConfig = "cannot parse OCI image config"
	errNewRuntimeConfig = "cannot create new OCI runtime config"
	errRemoveBundle     = "cannot remove OCI bundle from store"
	errChown            = "cannot chown path"
	errChmod            = "cannot chmod path"
	errFindImage        = "cannot find OCI image in cache"
	errDirExists        = "cannot determine whether dir exists"
	errConfigRoot       = "OCI config file must specify root.path relative to the root of the bundle"
	errSetupRootFS      = "cannot setup OCI bundle rootfs"
	errTeardownRootFS   = "cannot tear down OCI bundle rootfs"

	errFmtSize            = "wrote %d bytes to %q; expected %d"
	errFmtUnsupportedMode = "tarball contained file %q with unknown file type: %q"
	errFmtRenameTmpDir    = "cannot move temporary directory %q to %q"
	errFmtRunExists       = "bundle for run ID %q already exists"
)

// OverlayFS directories.
const (
	overlayDirLower  = "lower" // Only used when there are no parent layers.
	overlayDirUpper  = "upper"
	overlayDirMerged = "merged" // Only used when generating diff layers.
	overlayDirWork   = "work"
)

// TODO(negz): Consider caching the result of SupportsOverlay (e.g. by writing a
// .cachetype file) to avoid invoking it every time we run a function.

func SupportsOverlay() bool {
	// NewOverlayBundle will call MkdirAll on this path, which will succeed
	// despite it already existing. We use MkdirTemp to generate a random name.
	tmp, _ := os.MkdirTemp(os.TempDir(), "xfn-supports-overlay-")
	_ = os.MkdirAll(filepath.Join(tmp, overlayDirLower), 0700)

	b, err := NewOverlayBundle(tmp, &runtime.Spec{}, []string{filepath.Join(tmp, overlayDirLower)})
	if err != nil {
		return false
	}
	if err := b.Cleanup(); err != nil {
		return false
	}
	return true
}

type OverlayStore struct {
	root string

	e *LayerExtractor // TODO(negz): Make this an interface.
}

func NewOverlayStore(root string) (*OverlayStore, error) {
	for _, p := range []string{
		filepath.Join(root, layers),
		filepath.Join(root, images),
		filepath.Join(root, containers),
	} {
		if err := os.MkdirAll(p, 0700); err != nil {
			return nil, errors.Wrap(err, errMkdir)
		}
	}

	s := &OverlayStore{
		root: root,
		e:    &LayerExtractor{h: NewWhiteoutHandler(HeaderHandlerFn(Extract))},
	}
	return s, nil
}

func (s *OverlayStore) BundleContainer(ctx context.Context, i ociv1.Image, id string) (Bundle, error) {
	if err := s.cacheImage(ctx, i); err != nil {
		return nil, errors.Wrap(err, "cannot cache OCI image")
	}

	d, err := i.Digest()
	if err != nil {
		return nil, errors.Wrap(err, "cannot get image digest")
	}
	// The image's manifest should already be populated; this won't pull.
	m, err := i.Manifest()
	if err != nil {
		return nil, errors.Wrap(err, "cannot get OCI image manifest")
	}

	lowerPaths := make([]string, len(m.Layers))
	for i := range m.Layers {
		lowerPaths[i] = s.layerPath(m.Layers[i].Digest.Hex)
	}

	// Create an OCI runtime config file from our cached OCI image config file.
	// We do this every time we run the function because in future it's likely
	// that we'll want to derive the OCI runtime config file from both the OCI
	// image config file and user supplied input (i.e. from the functions array
	// of a Composition). Using the i.Config() or i.RawConfig() methods would
	// cause the config file to be pulled from a remote registry; we use the
	// config file we cached to disk instead.
	cfg, err := NewRuntimeConfigFromFile(filepath.Join(s.imagePath(d.Hex), config))
	if err != nil {
		return nil, errors.Wrap(err, "cannot create OCI runtime config")
	}

	b, err := NewOverlayBundle(s.containerPath(id), cfg, lowerPaths)
	return b, errors.Wrap(err, "cannot create bundle for container")
}

type OverlayBundle struct {
	path    string
	tmpfs   TmpFSMount
	overlay OverlayMount
}

func NewOverlayBundle(dir string, cfg *runtime.Spec, parentLayerPaths []string) (OverlayBundle, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return OverlayBundle{}, errors.Wrap(err, "cannot create bundle dir")
	}

	if err := os.Mkdir(filepath.Join(dir, tmpfs), 0700); err != nil {
		_ = os.RemoveAll(dir)
		return OverlayBundle{}, errors.Wrap(err, errMkdir)
	}

	tm := TmpFSMount{Mountpoint: filepath.Join(dir, tmpfs)}
	if err := tm.Mount(); err != nil {
		_ = os.RemoveAll(dir)
		return OverlayBundle{}, errors.Wrap(err, "cannot mount workdir tmpfs")
	}

	for _, p := range []string{
		filepath.Join(dir, tmpfs, overlayDirUpper),
		filepath.Join(dir, tmpfs, overlayDirWork),
		filepath.Join(dir, rootfs),
	} {
		if err := os.Mkdir(p, 0700); err != nil {
			_ = os.RemoveAll(dir)
			return OverlayBundle{}, errors.Wrapf(err, "cannot create %s dir", p)
		}
	}

	om := OverlayMount{
		Lower:      parentLayerPaths,
		Upper:      filepath.Join(dir, tmpfs, overlayDirUpper),
		Work:       filepath.Join(dir, tmpfs, overlayDirWork),
		Mountpoint: filepath.Join(dir, rootfs),
	}
	if err := om.Mount(); err != nil {
		_ = os.RemoveAll(dir)
		return OverlayBundle{}, errors.Wrap(err, "cannot mount workdir overlayfs")
	}

	if err := WriteRuntimeConfig(filepath.Join(dir, config), cfg); err != nil {
		_ = os.RemoveAll(dir)
		return OverlayBundle{}, errors.Wrap(err, "cannot write OCI runtime config")
	}

	return OverlayBundle{tmpfs: tm, overlay: om, path: dir}, nil
}

func (b OverlayBundle) Path() string { return b.path }

func (b OverlayBundle) Cleanup() error {
	if err := b.overlay.Unmount(); err != nil {
		return errors.Wrap(err, "cannot unmount bundle overlayfs")
	}
	if err := b.tmpfs.Unmount(); err != nil {
		return errors.Wrap(err, "cannot unmount bundle tmpfs")
	}
	return errors.Wrap(os.RemoveAll(b.path), "cannot remove bundle")
}

type TmpFSMount struct {
	Mountpoint string
}

func (m TmpFSMount) Mount() error {
	var flags uintptr
	return errors.Wrap(unix.Mount("tmpfs", m.Mountpoint, "tmpfs", flags, ""), "cannot mount tmpfs")
}

func (m TmpFSMount) Unmount() error {
	var flags int
	return errors.Wrap(unix.Unmount(m.Mountpoint, flags), "cannot unmount tmpfs")
}

type OverlayMount struct {
	Mountpoint string
	Lower      []string
	Upper      string
	Work       string
}

func (m OverlayMount) Mount() error {
	var flags uintptr
	data := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", strings.Join(m.Lower, ":"), m.Upper, m.Work)
	return errors.Wrap(unix.Mount("overlay", m.Mountpoint, "overlay", flags, data), "cannot mount overlayfs")
}

func (m OverlayMount) Unmount() error {
	var flags int
	return errors.Wrap(unix.Unmount(m.Mountpoint, flags), "cannot unmount overlayfs")
}

func (s *OverlayStore) containerPath(id string) string {
	return filepath.Join(s.root, containers, id)
}

func (s *OverlayStore) cacheImage(ctx context.Context, i ociv1.Image) error { //nolint:gocyclo // Only a touch over (12) at the time of writing.
	d, err := i.Digest()
	if err != nil {
		return errors.Wrap(err, "cannot get image digest")
	}

	// Check whether all of the image's layers appear to exist in the cache. If
	// the image is in the cache there's a good chance its layers are, but it's
	// cheap to double-check.
	layers, err := i.Layers()
	if err != nil {
		return errors.Wrap(err, "cannot get image layers")
	}

	for i := range layers {
		if err := s.cacheLayer(ctx, layers[i], layers[:i]...); err != nil {
			return errors.Wrap(err, "cannot cache image layer")
		}
	}

	exists, err := s.imageExists(d.Hex)
	if err != nil {
		return errors.Wrap(err, "cannot determine whether image exists in cache")
	}
	if exists {
		// Image is already in the cache; nothing to do.
		return nil
	}

	w, err := NewImageWorkdir(os.TempDir(), d.Hex)
	if err != nil {
		return errors.Wrap(err, "cannot create image work dir")
	}
	defer w.Cleanup() //nolint:errcheck // Not much we can do if this fails.

	m, err := i.RawManifest()
	if err != nil {
		return errors.Wrap(err, "cannot get image manifest")
	}

	if err := os.WriteFile(filepath.Join(w.Path, manifest), m, 0600); err != nil {
		return errors.Wrap(err, "cannot write image manifest")
	}

	c, err := i.RawConfigFile()
	if err != nil {
		return errors.Wrap(err, "cannot get image config")
	}

	if err := os.WriteFile(filepath.Join(w.Path, config), c, 0600); err != nil {
		return errors.Wrap(err, "cannot write image config")
	}

	return errors.Wrap(os.Rename(w.Path, s.imagePath(d.Hex)), "cannot move image into place")
}

func (s *OverlayStore) imagePath(digest string) string {
	return filepath.Join(s.root, images, digest)
}

func (s *OverlayStore) imageExists(digest string) (bool, error) {
	path := s.imagePath(digest)
	fi, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, errors.Wrapf(err, "cannot stat path %q", path)
	}
	if !fi.IsDir() {
		return false, errors.Errorf("path %q exists but is not a directory", path)
	}
	// TODO(negz): Check whether this image has a config.json and manifest.json
	// at the expected paths?
	return true, nil
}

func (s *OverlayStore) cacheLayer(ctx context.Context, l ociv1.Layer, parents ...ociv1.Layer) error {
	d, err := l.Digest()
	if err != nil {
		return errors.Wrap(err, "cannot get layer digest")
	}

	exists, err := s.layerExists(d.Hex)
	if err != nil {
		return errors.Wrap(err, "cannot determine whether layer exists in cache")
	}
	if exists {
		// No need to cache this layer; we already did.
		return nil
	}

	// This call to Uncompressed is what actually pulls the layer.
	tarball, err := l.Uncompressed()
	if err != nil {
		return errors.Wrap(err, "cannot fetch and extract layer")
	}

	parentPaths := make([]string, len(parents))
	for i := range parents {
		d, err := parents[i].Digest()
		if err != nil {
			return errors.Wrap(err, "cannot get parent layer digest")
		}
		parentPaths[i] = s.layerPath(d.Hex)
	}

	lw, err := NewLayerWorkdir(os.TempDir(), d.Hex, parentPaths)
	if err != nil {
		return errors.Wrap(err, "cannot create layer work dir")
	}

	if err := s.e.Apply(ctx, tarball, lw.ApplyPath()); err != nil {
		_ = lw.Cleanup()
		return errors.Wrap(err, "cannot untar layer")
	}

	if err := os.Rename(lw.ResultPath(), s.layerPath(d.Hex)); err != nil {
		_ = lw.Cleanup()
		return errors.Wrap(err, "cannot move layer into place")
	}

	return errors.Wrap(lw.Cleanup(), "cannot cleanup layer work dir")
}

// layerPath returns the path at which the cache would store an overlayfs layer
// for the OCI layer with the supplied (compressed) digest.
func (s *OverlayStore) layerPath(digest string) string {
	return filepath.Join(s.root, layers, digest)
}

func (s *OverlayStore) layerExists(digest string) (bool, error) {
	path := s.layerPath(digest)
	fi, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, errors.Wrapf(err, "cannot stat path %q", path)
	}
	if !fi.IsDir() {
		return false, errors.Errorf("path %q exists but is not a directory", path)
	}
	// TODO(negz): Is there anything else we can do to validate that this is
	// really a layer? Currently it contains an extracted tarball at its root.
	return true, nil
}

type LayerWorkdir struct {
	OverlayMount

	path string
}

func NewLayerWorkdir(dir, digest string, parentLayerPaths []string) (LayerWorkdir, error) {
	tmp, err := os.MkdirTemp(dir, fmt.Sprintf("xfn-wrk-%s-%s-", layers, digest))
	if err != nil {
		return LayerWorkdir{}, errors.Wrap(err, "cannot create temp dir")
	}

	for _, d := range []string{overlayDirMerged, overlayDirUpper, overlayDirLower, overlayDirWork} {
		if err := os.Mkdir(filepath.Join(tmp, d), 0700); err != nil {
			_ = os.RemoveAll(tmp)
			return LayerWorkdir{}, errors.Wrapf(err, "cannot create %s dir", d)
		}
	}

	w := LayerWorkdir{
		OverlayMount: OverlayMount{
			Lower:      []string{filepath.Join(tmp, overlayDirLower)},
			Upper:      filepath.Join(tmp, overlayDirUpper),
			Work:       filepath.Join(tmp, overlayDirWork),
			Mountpoint: filepath.Join(tmp, overlayDirMerged),
		},
		path: tmp,
	}

	if len(parentLayerPaths) != 0 {
		w.Lower = parentLayerPaths
	}

	if err := w.Mount(); err != nil {
		_ = os.RemoveAll(tmp)
		return LayerWorkdir{}, errors.Wrap(err, "cannot mount workdir overlayfs")
	}

	return w, nil
}

func (d LayerWorkdir) ApplyPath() string {
	return filepath.Join(d.path, overlayDirMerged)
}

func (d LayerWorkdir) ResultPath() string {
	return filepath.Join(d.path, overlayDirUpper)
}

func (d LayerWorkdir) Cleanup() error {
	if err := d.Unmount(); err != nil {
		return errors.Wrap(err, "cannot unmount workdir overlayfs")
	}
	return errors.Wrap(os.RemoveAll(d.path), "cannot remove workdir")
}
