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

package xfn

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/google/go-containerregistry/pkg/name"
	ociv1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/spf13/afero"
	"sigs.k8s.io/yaml"

	"github.com/crossplane/crossplane-runtime/pkg/errors"

	"github.com/crossplane/crossplane/apis/apiextensions/fn/v1alpha1"
	fnv1alpha1 "github.com/crossplane/crossplane/apis/apiextensions/fn/v1alpha1"
	"github.com/crossplane/crossplane/internal/xpkg"
)

const (
	errInvalidInput     = "invalid function input"
	errInvalidOutput    = "invalid function output"
	errBadReference     = "OCI tag is not a valid reference"
	errHeadImg          = "cannot fetch OCI image descriptor"
	errExecFn           = "cannot execute function"
	errNoCmd            = "function OCI image must specify entrypoint and/or cmd"
	errAdvanceTarball   = "cannot advance to next entry in tarball"
	errCloseFsTarball   = "cannot close function filesystem tarball"
	errFetchFnOCIConfig = "cannot fetch function OCI config"
	errMakeFnTmpDir     = "cannot make temporary directory to unarchive function tarball"
	errUntarFn          = "cannot unarchive function tarball"

	// TODO(negz): Make these errFmt, with the image string.
	errFetchFn            = "cannot fetch function from registry"
	errLookupFn           = "cannot lookup function in store"
	errWriteFn            = "cannot write function to store"
	errFmtMkdir           = "cannot make directory %q"
	errFmtOpenFile        = "cannot open file %q"
	errFmtCopyFile        = "cannot copy file %q"
	errFmtCloseFile       = "cannot close file %q"
	errFmtCreateFile      = "cannot create file %q"
	errFmtWriteFile       = "cannot write file %q"
	errFmtReadFile        = "cannot read file %q"
	errFmtSize            = "wrote %d bytes to %q; expected %d"
	errFmtInvalidPath     = "tarball contains invalid file path %q"
	errFmtFsExists        = "cannot determine whether filesystem %q exists"
	errFmtFsNotFound      = "filesystem %q not found"
	errFmtUnsupportedMode = "tarball contained file %q with unknown file type: %q"
	errFmtRenameFnTmpDir  = "cannot move temporary function filesystem %q to %q"
)

const configFileSuffix = ".json"

// An OCIRunner runs an XRM function packaged as an OCI image by extracting it
// and running it in a chroot.
type OCIRunner struct {
	image string

	// TODO(negz): Break fetch-ey bits out of xpkg.
	defaultRegistry string
	registry        xpkg.Fetcher
	store           *Store
}

// A OCIRunnerOption configures a new OCIRunner.
type OCIRunnerOption func(*OCIRunner)

// NewOCIRunner returns a new Runner that runs functions packaged as OCI images.
func NewOCIRunner(image string, o ...OCIRunnerOption) *OCIRunner {
	r := &OCIRunner{
		image:           image,
		defaultRegistry: name.DefaultRegistry,
		registry:        xpkg.NewNopFetcher(),
		store:           NewStore("/xfn/store"),
	}
	for _, fn := range o {
		fn(r)
	}
	return r
}

// Run a function packaged as an OCI image. Functions are not run as containers,
// but rather by unarchiving them and executing their entrypoint and/or cmd in a
// chroot with their supplied environment variables set. This allows them to be
// run from inside an existing, unprivileged container. Functions that write to
// stderr, return non-zero, or that cannot be executed in the first place (e.g.
// because they cannot be fetched from the registry) will return an error.
func (r *OCIRunner) Run(ctx context.Context, in *fnv1alpha1.ResourceList) (*fnv1alpha1.ResourceList, error) {
	// Parse the input early, before we potentially pull and write the image.
	y, err := yaml.Marshal(in)
	if err != nil {
		return nil, errors.Wrap(err, errInvalidInput)
	}
	stdin := bytes.NewReader(y)

	ref, err := name.ParseReference(r.image, name.WithDefaultRegistry(r.defaultRegistry))
	if err != nil {
		return nil, errors.Wrap(err, errBadReference)
	}

	d, err := r.registry.Head(ctx, ref)
	if err != nil {
		return nil, errors.Wrap(err, errHeadImg)
	}

	cfg, root, err := r.store.Lookup(ctx, d.Digest.Hex)
	if IsNotFound(err) {
		// If the function isn't found in the store we fetch it, write it, and
		// try to look it up again.
		img, ferr := r.registry.Fetch(ctx, ref)
		if ferr != nil {
			return nil, errors.Wrap(ferr, errFetchFn)
		}

		if err := r.store.Write(ctx, d.Digest.Hex, img); err != nil {
			return nil, errors.Wrap(err, errWriteFn)
		}

		// Note we're setting the outer err that satisfied IsNotFound.
		cfg, root, err = r.store.Lookup(ctx, d.Digest.Hex)
	}
	if err != nil {
		return nil, errors.Wrap(err, errLookupFn)
	}

	// Per https://github.com/opencontainers/image-spec/blob/v1.0/config.md the
	// EntryPoint is "A list of arguments to use as the command to execute when
	// the container starts", whereas the Cmd is "arguments to the entrypoint of
	// the container. If an Entrypoint value is not specified, then the first
	// entry of the Cmd array SHOULD be interpreted as the executable to run."
	argv := make([]string, 0, len(cfg.Config.Entrypoint)+len(cfg.Config.Cmd))
	argv = append(argv, cfg.Config.Entrypoint...)
	argv = append(argv, cfg.Config.Cmd...)
	if len(argv) == 0 {
		return nil, errors.New(errNoCmd)
	}

	//nolint:gosec // Taking user-supplied input here is intentional.
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Chroot: root}
	cmd.Dir = cfg.Config.WorkingDir
	cmd.Env = cfg.Config.Env
	cmd.Stdin = stdin

	stdout, err := cmd.Output()
	if err != nil {
		// TODO(negz): Don't swallow stderr if this is an *exec.ExitError?
		return nil, errors.Wrap(err, errExecFn)
	}

	out := &v1alpha1.ResourceList{}
	return out, errors.Wrap(yaml.Unmarshal(stdout, out), errInvalidOutput)
}

type errNotFound struct{ error }

// IsNotFound indicates a config file and/or filesystem were not found in the
// store.
func IsNotFound(err error) bool {
	return errors.As(err, &errNotFound{})
}

// We'll get an error that we want to wrap.
// We want to decorate that error to 'be' something else.

// A Store of extracted OCI images - config files and flattened filesystems.
type Store struct {
	root string
	fs   afero.Afero
}

// A StoreOption configures a new Store.
type StoreOption func(*Store)

// WithFS configures the filesystem a store should use.
func WithFS(fs afero.Afero) StoreOption {
	return func(s *Store) {
		s.fs = fs
	}
}

// NewStore returns a new store ready for use. The store is backed by the OS
// filesystem unless the WithFS option is supplied.
func NewStore(root string, o ...StoreOption) *Store {
	s := &Store{root: root, fs: afero.Afero{Fs: afero.NewOsFs()}}
	for _, fn := range o {
		fn(s)
	}
	return s
}

// Lookup the config file and flattened filesystem of the supplied ID in the
// store. Returns an error that satisfies IsNotFound if either are not found.
func (s *Store) Lookup(ctx context.Context, id string) (*ociv1.ConfigFile, string, error) {
	cfgPath := filepath.Join(s.root, id+configFileSuffix)

	f, err := s.fs.Open(cfgPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, "", errNotFound{errors.Wrapf(err, errFmtOpenFile, cfgPath)}
	}
	if err != nil {
		return nil, "", errors.Wrapf(err, errFmtOpenFile, cfgPath)
	}

	cfg, err := ociv1.ParseConfigFile(f)
	if err != nil {
		_ = f.Close()
		return nil, "", errors.Wrapf(err, errFmtReadFile, cfgPath)
	}

	if err := f.Close(); err != nil {
		return nil, "", errors.Wrapf(err, errFmtCloseFile, cfgPath)
	}

	fsPath := filepath.Join(s.root, id)
	exists, err := s.fs.DirExists(fsPath)
	if err != nil {
		return nil, "", errors.Wrapf(err, errFmtFsExists, fsPath)
	}
	if !exists {
		return nil, "", errNotFound{errors.Wrapf(err, errFmtFsNotFound, fsPath)}
	}

	return cfg, fsPath, nil
}

// Write the config file and flattened filesystem of the supplied OCI image to
// the store. Write simulates a transaction in that it will attempt to unwind a
// partial write if an error occurs.
func (s *Store) Write(ctx context.Context, id string, img ociv1.Image) error {
	// We unarchive to a temporary directory first, then move it to its
	// 'real' path only if the unarchive worked. This lets us simulate an
	// 'atomic' unarchive that we can unwind if it fails.
	tmp, err := s.fs.TempDir(s.root, id)
	if err != nil {
		return errors.Wrap(err, errMakeFnTmpDir)
	}

	// RemoveAll doesn't return an error if the supplied directory doesn't
	// exist, so this should be a no-op if we successfully called Rename to
	// move our tmp directory to its 'real' path.
	defer s.fs.RemoveAll(tmp) //nolint:errcheck // There's not much we can do if this fails.

	flattened := mutate.Extract(img)
	if err := untar(ctx, tar.NewReader(flattened), s.fs, tmp); err != nil {
		_ = flattened.Close()
		return errors.Wrap(err, errUntarFn)
	}
	if err := flattened.Close(); err != nil {
		return errors.Wrap(err, errCloseFsTarball)
	}

	cfg, err := img.ConfigFile()
	if err != nil {
		return errors.Wrap(err, errFetchFnOCIConfig)
	}

	cfgPath := filepath.Join(s.root, id+configFileSuffix)
	f, err := s.fs.Create(cfgPath)
	if err != nil {
		return errors.Wrapf(err, errFmtCreateFile, cfgPath)
	}

	if err := json.NewEncoder(f).Encode(cfg); err != nil {
		_ = f.Close()
		return errors.Wrapf(err, errFmtWriteFile, cfgPath)
	}

	if err := f.Close(); err != nil {
		return errors.Wrapf(err, errFmtCloseFile, cfgPath)
	}

	// We successfully wrote our 'filesystem' and config file. Time to move
	// our temporary working directory to the 'real' location.
	fsPath := filepath.Join(s.root, id)
	return errors.Wrapf(s.fs.Rename(tmp, fsPath), errFmtRenameFnTmpDir, tmp, fsPath)
}

// untar an uncompressed tarball to dir in the supplied filesystem.
// Adapted from https://github.com/golang/build/blob/5aee8e/internal/untar/untar.go
func untar(ctx context.Context, tb io.Reader, fs afero.Fs, dir string) error { //nolint:gocyclo
	// NOTE(negz): This function is a little over our gocyclo target. I can't
	// see an immediate way to simplify/break it up that would be equally easy
	// to read.

	tr := tar.NewReader(tb)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return errors.Wrap(err, errAdvanceTarball)
		}
		if !validPath(hdr.Name) {
			return errors.Errorf(errFmtInvalidPath, hdr.Name)
		}

		path := filepath.Join(dir, filepath.Clean(filepath.FromSlash(hdr.Name)))
		mode := hdr.FileInfo().Mode()

		switch {
		case mode.IsDir():
			if err := fs.MkdirAll(path, 0755); err != nil {
				return errors.Wrapf(err, errFmtMkdir, path)
			}
		case mode.IsRegular():
			d := filepath.Dir(path)
			if err := fs.MkdirAll(d, 0755); err != nil {
				return errors.Wrapf(err, errFmtMkdir, d)
			}

			dst, err := fs.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, mode.Perm())
			if err != nil {
				return errors.Wrapf(err, errFmtOpenFile, path)
			}
			n, err := copyChunks(dst, tr, 1024*1024) // Copy in 1MB chunks.
			if err != nil {
				_ = dst.Close()
				return errors.Wrapf(err, errFmtCopyFile, path)
			}
			if err := dst.Close(); err != nil {
				return errors.Wrapf(err, errFmtCloseFile, path)
			}
			if n != hdr.Size {
				return errors.Errorf(errFmtSize, n, path, hdr.Size)
			}
		default:
			return errors.Errorf(errFmtUnsupportedMode, hdr.Name, mode)
		}
	}
}

func validPath(p string) bool {
	if p == "" || strings.Contains(p, `\`) || strings.Contains(p, "../") {
		return false
	}
	return true
}

// copyChunks pleases gosec per https://github.com/securego/gosec/pull/433.
// Like Copy it reads from src until EOF, it does not treat an EOF from Read as
// an error to be reported.
//
// NOTE(negz): This rule confused me at first because io.Copy appears to use a
// buffer, but in fact it bypasses it if src/dst is an io.WriterTo/ReaderFrom.
func copyChunks(dst io.Writer, src io.Reader, chunkSize int64) (int64, error) {
	var written int64
	for {
		w, err := io.CopyN(dst, src, chunkSize)
		written += w
		if errors.Is(err, io.EOF) {
			return written, nil
		}
		if err != nil {
			return written, err
		}
	}
}
