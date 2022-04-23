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
	"io"
	"os/exec"
	"syscall"

	"github.com/crossplane/crossplane-runtime/pkg/errors"
)

// Error strings.
const (
	errCreateCacheDir = "cannot create cache directory"
	errChownCacheDir  = "cannot chown cache directory"
	errInvalidInput   = "invalid function input"
	errInvalidOutput  = "invalid function output"
	errBadReference   = "OCI tag is not a valid reference"
	errHeadImg        = "cannot fetch OCI image descriptor"
	errExecFn         = "cannot execute function"
	errFetchFn        = "cannot fetch function from registry"
	errLookupFn       = "cannot lookup function in store"
	errWriteFn        = "cannot write function to store"
	errDeleteBundle   = "cannot delete OCI bundle"
	errChownFd        = "cannot chown file descriptor"

	errCreateStdioPipes = "cannot create stdio pipes"
	errCreateStdinPipe  = "cannot create stdin pipe"
	errCreateStdoutPipe = "cannot create stdout pipe"
	errCreateStderrPipe = "cannot create stderr pipe"
	errStartFunction    = "cannot start function container"
	errWriteFunctionIO  = "cannot write FunctionIO to container stdin"
	errCloseStdin       = "cannot close stdin pipe"
	errReadStdout       = "cannot read from stdout pipe"
	errReadStderr       = "cannot read from stderr pipe"
)

const defaultCacheDir = "/xfn"

// An ContainerRunner runs an XRM function packaged as an OCI image by
// extracting it and running it as a 'rootless' container.
type ContainerRunner struct {
	image   string
	rootUID int
	rootGID int
	setuid  bool // Specifically, CAP_SETUID and CAP_SETGID.
	cache   string
}

// A ContainerRunnerOption configures a new ContainerRunner.
type ContainerRunnerOption func(*ContainerRunner)

// MapToRoot configures what UID and GID should map to root (UID/GID 0) in the
// user namespace in which the function will be run.
func MapToRoot(uid, gid int) ContainerRunnerOption {
	return func(r *ContainerRunner) {
		r.rootUID = uid
		r.rootGID = gid
	}
}

// SetUID indicates that the container runner should attempt operations that
// require CAP_SETUID and CAP_SETGID, for example creating a user namespace that
// maps arbitrary UIDs and GIDs to the parent namespace.
func SetUID(s bool) ContainerRunnerOption {
	return func(r *ContainerRunner) {
		r.setuid = s
	}
}

// WithCacheDir specifies the directory used for caching function images and
// containers.
func WithCacheDir(d string) ContainerRunnerOption {
	return func(r *ContainerRunner) {
		r.cache = d
	}
}

// NewContainerRunner returns a new Runner that runs functions as rootless
// containers.
func NewContainerRunner(image string, o ...ContainerRunnerOption) *ContainerRunner {
	r := &ContainerRunner{image: image, cache: defaultCacheDir}
	for _, fn := range o {
		fn(r)
	}

	return r
}

// StdioPipes creates and returns pipes that will be connected to the supplied
// command's stdio when it starts. It calls fchown(2) to ensure all pipes are
// owned by the supplied user and group ID; this ensures that the command can
// read and write its stdio even when xfn is running as root (in the parent
// namespace) and the command is not.
func StdioPipes(cmd *exec.Cmd, uid, gid int) (*Stdio, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, errors.Wrap(err, errCreateStdinPipe)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, errors.Wrap(err, errCreateStdoutPipe)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, errors.Wrap(err, errCreateStderrPipe)
	}

	// StdinPipe and friends above return "our end" of the pipe - i.e. stdin is
	// the io.WriteCloser we can use to write to the command's stdin. They also
	// setup the "command's end" of the pipe - i.e. cmd.Stdin is the io.Reader
	// the command can use to read its stdin. In all cases these pipes _should_
	// be *os.Files.
	for _, s := range []any{stdin, stdout, stderr, cmd.Stdin, cmd.Stdout, cmd.Stderr} {
		f, ok := s.(interface{ Fd() uintptr })
		if !ok {
			return nil, errors.Errorf("stdio pipe (type: %T) missing required Fd() method", f)
		}
		if err := syscall.Fchown(int(f.Fd()), uid, gid); err != nil {
			return nil, errors.Wrap(err, errChownFd)
		}
	}

	return &Stdio{Stdin: stdin, Stdout: stdout, Stderr: stderr}, nil
}

// Stdio can be used to read and write a command's standard I/O.
type Stdio struct {
	Stdin  io.WriteCloser
	Stdout io.ReadCloser
	Stderr io.ReadCloser
}
