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
	"archive/tar"
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	securejoin "github.com/cyphar/filepath-securejoin"
	"golang.org/x/sys/unix"

	"github.com/crossplane/crossplane-runtime/pkg/errors"
)

// NOTE(negz): Initially we used afero to allow mocking the filesystem, but we
// need to do a lot of things afero doesn't support on its mock filesystems
// (symlinks, mounts etc) which made using it more awkward than it was worth.

// OCI whiteouts.
// See https://github.com/opencontainers/image-spec/blob/v1.0/layer.md#whiteouts
const (
	ociWhiteoutPrefix     = ".wh."
	ociWhiteoutMetaPrefix = ociWhiteoutPrefix + ociWhiteoutPrefix
	ociWhiteoutOpaqueDir  = ociWhiteoutMetaPrefix + ".opq"
)

// A HeaderHandler handles a single file (header) within a tarball.
type HeaderHandler interface {
	// Handle the supplied tarball header by applying it to the supplied path,
	// e.g. creating a file, directory, etc. The supplied io.Reader is expected
	// to be a tarball advanced to the supplied header, i.e. via tr.Next().
	Handle(h *tar.Header, tr io.Reader, path string) error
}

// A HeaderHandlerFn is a function that acts as a HeaderHandler.
type HeaderHandlerFn func(h *tar.Header, tr io.Reader, path string) error

// Handle the supplied tarball header.
func (fn HeaderHandlerFn) Handle(h *tar.Header, tr io.Reader, path string) error {
	return fn(h, tr, path)
}

// A StackingLayerExtractor extracts an OCI layer by 'stacking' it atop the
// supplied root directory.
type StackingLayerExtractor struct {
	h HeaderHandler
}

// Apply calls the StackingLayerExtractor's HeaderHandler for each file in the
// supplied layer tarball, adjusting their path to be rooted under the supplied
// root directory. That is, /foo would be extracted to /bar as /bar/foo.
func (e *StackingLayerExtractor) Apply(ctx context.Context, tb io.Reader, root string) error {
	tr := tar.NewReader(tb)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return errors.Wrap(err, errAdvanceTarball)
		}

		// SecureJoin joins hdr.Name to root, ensuring the resulting path does
		// not escape root either syntactically (via "..") or via symlinks in
		// the path. For example:
		//
		// * Joining "/a" and "../etc/passwd" results in "/a/etc/passwd".
		// * Joining "/a" and "evil/passwd" where "/a/evil" exists and is a
		//   symlink to "/etc" results in "/a/etc/passwd".
		//
		// https://codeql.github.com/codeql-query-help/go/go-unsafe-unzip-symlink/
		path, err := securejoin.SecureJoin(root, hdr.Name)
		if err != nil {
			return errors.Wrap(err, errEvalSymlinks)
		}

		if err := e.h.Handle(hdr, tr, path); err != nil {
			return errors.Wrapf(err, "could not handle tar header for %q", hdr.Name)
		}
	}

	// TODO(negz): Handle MAC times for directories. This needs to be done last,
	// since mutating a directory's contents will update its MAC times.

	return nil
}

// A WhiteoutHandler handles OCI whiteouts by deleting the corresponding files.
// It passes anything that is not a whiteout to an underlying HeaderHandler. It
// avoids deleting any file created by the underling HeaderHandler.
type WhiteoutHandler struct {
	wrapped HeaderHandler
	handled map[string]bool
}

// NewWhiteoutHandler returns a HeaderHandler that handles OCI whiteouts by
// deleting the corresponding files.
func NewWhiteoutHandler(hh HeaderHandler) *WhiteoutHandler {
	return &WhiteoutHandler{wrapped: hh, handled: make(map[string]bool)}
}

// Handle the supplied tar header.
func (w *WhiteoutHandler) Handle(h *tar.Header, tr io.Reader, path string) error {
	// If this isn't a whiteout file, extract it.
	if !strings.HasPrefix(filepath.Base(path), ociWhiteoutPrefix) {
		w.handled[path] = true
		return w.wrapped.Handle(h, tr, path)
	}

	// We must only whiteout files from previous layers; i.e. not files that
	// we've extracted from this layer. We're operating on a merged overlayfs,
	// so we can't rely on the filesystem to distinguish what files are from a
	// previous layer. Instead we track which files we've extracted from this
	// layer and avoid whiting-out any file we've extracted. It's possible we'll
	// see a whiteout out-of-order; i.e. we'll whiteout /foo, then later extract
	// /foo from the same layer. This should be fine; we'll first delete it,
	// recreate it, resulting in the desired file in our overlayfs upper dir.
	// https://github.com/opencontainers/image-spec/blob/v1.0/layer.md#whiteouts

	base := filepath.Base(path)
	dir := filepath.Dir(path)

	// Handle explicit whiteout files. These files resolve to an explicit path
	// that should be deleted from the current layer.
	if base != ociWhiteoutOpaqueDir {
		whiteout := filepath.Join(dir, base[len(ociWhiteoutPrefix):])

		if w.handled[whiteout] {
			return nil
		}

		return errors.Wrapf(os.RemoveAll(whiteout), "cannot whiteout file %q", whiteout)
	}

	// Handle an opaque directory. These files indicate that all siblings in
	// their directory should be deleted from the current layer.
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) {
			// Either this path is under a directory we already deleted or we've
			// been asked to whiteout a directory that doesn't exist.
			return nil
		}
		if err != nil {
			return err
		}

		// Don't delete the directory we're whiting out, or a file we've
		// extracted from this layer.
		if path == dir || w.handled[path] {
			return nil
		}

		return os.RemoveAll(path)
	})

	return errors.Wrapf(err, "cannot whiteout dir %q", dir)
}

// TODO(negz): Make this an ExtractHandler struct, with pluggable handlers for
// each type - or perhaps a map of types to handlers?

// Extract is a HeaderHandler that creates a file at the supplied path per the
// supplied tar header. It is not aware of OCI whiteout files, and will only
// create files that can be created inside an unprivileged user namespace.
func Extract(h *tar.Header, tr io.Reader, path string) error {
	mode := h.FileInfo().Mode()

	// ExtractDir should correct these permissions.
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return errors.Wrap(err, errMkdir)
	}

	switch h.Typeflag {
	case tar.TypeDir:
		if err := ExtractDir(h, tr, path); err != nil {
			return errors.Wrap(err, "cannot extract directory")
		}
	// TODO(negz): Don't extract hard links as symlinks. Creating an actual hard
	// link would require us to securely join the path of the 'root' directory
	// we're untarring into with h.Linkname, but we don't currently plumb the
	// root directory down to this level.
	case tar.TypeSymlink, tar.TypeLink:
		if err := ExtractSymlink(h, tr, path); err != nil {
			return errors.Wrap(err, "cannot extract symlink")
		}
	case tar.TypeReg:
		if err := ExtractFile(h, tr, path); err != nil {
			return errors.Wrap(err, "cannot extract file")
		}
	case tar.TypeFifo:
		if err := ExtractFIFO(h, tr, path); err != nil {
			return errors.Wrap(err, "cannot extract fifo")
		}
	default:
		// Better to return an error than to write a partial layer. Note that
		// tar.TypeBlock and tar.TypeChar in particular are unsupported because
		// they can't be created without CAP_MKNOD in the 'root' user namespace
		// per https://man7.org/linux/man-pages/man7/user_namespaces.7.html
		return errors.Errorf(errFmtUnsupportedMode, h.Name, mode)
	}

	// We expect to have CAP_CHOWN (inside a user namespace) when running
	// this code, but if that namespace was created by a user without
	// CAP_SETUID and CAP_SETGID only one UID and GID (root) will exist and
	// we'll get syscall.EINVAL if we try to chown to any other. We ignore
	// this error and attempt to run the function regardless; functions that
	// run 'as root' (in their namespace) should work fine.

	// TODO(negz): Return this error if it isn't syscall.EINVAL? Currently
	// doing so would require taking a dependency on the syscall package per
	// https://groups.google.com/g/golang-nuts/c/BpWN9N-hw3s.
	_ = os.Lchown(path, h.Uid, h.Gid)

	// TODO(negz): Handle MAC times.

	return nil
}

// ExtractDir is a HeaderHandler that creates a directory at the supplied path
// per the supplied tar header.
func ExtractDir(h *tar.Header, _ io.Reader, path string) error {
	mode := h.FileInfo().Mode()
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return errors.Wrap(os.MkdirAll(path, mode.Perm()), errMkdir)
	}
	if err != nil {
		return errors.Wrapf(err, "cannot determine whether path %q already exists", path)
	}
	if !fi.IsDir() {
		return errors.Errorf("path %q exists but is not a directory", path)
	}
	return errors.Wrap(os.Chmod(path, mode.Perm()), errChmod)
}

// ExtractSymlink is a HeaderHandler that creates a symlink at the supplied path
// per the supplied tar header.
func ExtractSymlink(h *tar.Header, _ io.Reader, path string) error {
	// We don't sanitize h.LinkName (the symlink's target). It will be sanitized
	// by SecureJoin above to prevent malicious writes during the untar process,
	// and will be evaluated relative to root during function execution.
	return errors.Wrap(os.Symlink(h.Linkname, path), errSymlink)
}

// ExtractFIFO is a HeaderHandler that creates a FIFO at the supplied path per
// the supplied tar header.
func ExtractFIFO(h *tar.Header, _ io.Reader, path string) error {
	// We won't have CAP_MKNOD in a user namespace created by a user who doesn't
	// have CAP_MKNOD in the initial/root user namespace, but we don't need it
	// to use mknod to create a FIFO.
	// https://man7.org/linux/man-pages/man2/mknod.2.html
	mode := uint32(h.Mode&0777) | unix.S_IFIFO
	dev := unix.Mkdev(uint32(h.Devmajor), uint32(h.Devminor))
	return errors.Wrap(unix.Mknod(path, mode, int(dev)), "cannot create FIFO")
}

// ExtractFile is a HeaderHandler that creates a regular file at the supplied
// path per the supplied tar header.
func ExtractFile(h *tar.Header, tr io.Reader, path string) error {
	mode := h.FileInfo().Mode()

	//nolint:gosec // The root of this path is user supplied input.
	dst, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return errors.Wrap(err, errOpenFile)
	}

	n, err := copyChunks(dst, tr, 1024*1024) // Copy in 1MB chunks.
	if err != nil {
		_ = dst.Close()
		return errors.Wrap(err, errCopyFile)
	}
	if err := dst.Close(); err != nil {
		return errors.Wrap(err, errCloseFile)
	}
	if n != h.Size {
		return errors.Errorf(errFmtSize, n, path, h.Size)
	}
	return nil
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
