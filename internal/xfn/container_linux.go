//go:build linux

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
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"syscall"

	"sigs.k8s.io/yaml"

	"github.com/crossplane/crossplane-runtime/pkg/errors"

	"github.com/crossplane/crossplane/apis/apiextensions/fn/v1alpha1"
	fnv1alpha1 "github.com/crossplane/crossplane/apis/apiextensions/fn/v1alpha1"
)

// The subcommand of xfn to invoke - i.e. "xfn spark <source> <bundle>"
const spark = "spark"

// Error strings.
const (
	errInvalidInput  = "invalid function input"
	errInvalidOutput = "invalid function output"
	errBadReference  = "OCI tag is not a valid reference"
	errHeadImg       = "cannot fetch OCI image descriptor"
	errExecFn        = "cannot execute function"
	errFetchFn       = "cannot fetch function from registry"
	errLookupFn      = "cannot lookup function in store"
	errWriteFn       = "cannot write function to store"
	errDeleteBundle  = "cannot delete OCI bundle"
	errChownFd       = "cannot chown file descriptor"

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

// An ContainerRunner runs an XRM function packaged as an OCI image by
// extracting it and running it as a 'rootless' container.
type ContainerRunner struct {
	image   string
	rootUID int
	rootGID int
	setuid  bool // Specifically, CAP_SETUID and CAP_SETGID.
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

// NewContainerRunner returns a new Runner that runs functions as rootless
// containers.
func NewContainerRunner(image string, o ...ContainerRunnerOption) *ContainerRunner {
	r := &ContainerRunner{image: image}
	for _, fn := range o {
		fn(r)
	}
	return r
}

// Run a function as a rootless OCI container. Functions that return non-zero,
// or that cannot be executed in the first place (e.g. because they cannot be
// fetched from the registry) will return an error.
func (r *ContainerRunner) Run(ctx context.Context, in *fnv1alpha1.FunctionIO) (*fnv1alpha1.FunctionIO, error) { //nolint:gocyclo
	// NOTE(negz): This is currently only a touch over our complexity goal.

	// Parse the input early, before we potentially pull and write the image.
	y, err := yaml.Marshal(in)
	if err != nil {
		return nil, errors.Wrap(err, errInvalidInput)
	}

	/*
		We want to create an overlayfs with the cached rootfs as the lower layer
		and the bundle's rootfs as the upper layer, if possible. Kernel 5.11 and
		later supports using overlayfs inside a user (and mount) namespace. The
		best way to run code in a user namespace in Go is to execute a separate
		binary; the unix.Unshare syscall affects only one OS thread, and the Go
		scheduler might move the goroutine to another.

		Therefore we execute a small shim - xfn spark - in a new user and mount
		namespace. spark sets up the overlayfs if the Kernel supports it.
		Otherwise it falls back to making a copy of the cached rootfs. spark
		then executes an OCI runtime which creates another layer of namespaces
		in order to actually execute the function.

		We don't need to cleanup the mounts spark creates. They will be removed
		automatically along with their mount namespace when spark exits.
	*/
	cmd := exec.CommandContext(ctx, os.Args[0], spark, r.image) //nolint:gosec // We're intentionally executing with variable input.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: r.rootUID, Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: r.rootGID, Size: 1}},
	}

	// When we have CAP_SETUID and CAP_SETGID (i.e. typically when root), we can
	// map a range of UIDs (0 to 65,336) inside the user namespace to a range in
	// its parent. We can also drop privileges (in the parent user namespace) by
	// running spark as root in the user namespace.
	if r.setuid {
		cmd.SysProcAttr.UidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: r.rootUID, Size: UserNamespaceUIDs}}
		cmd.SysProcAttr.GidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: r.rootGID, Size: UserNamespaceGIDs}}
		cmd.SysProcAttr.GidMappingsEnableSetgroups = true

		// UID and GID 0 here are relative to the new user namespace - i.e. they
		// correspond to HostID in the parent. We're able to do this because
		// Go's exec.Command will:
		//
		// 1. Call clone(2) to create a child process in a new user namespace.
		// 2. In the child process, wait for /proc/self/uid_map to be written.
		// 3. In the parent process, write the child's /proc/$pid/uid_map.
		// 4. In the child process, call setuid(2) and setgid(2) per Credential.
		// 5. In the child process, call execve(2) to execute spark.
		//
		// Per user_namespaces(7) the child process created by clone(2) starts
		// out with a complete set of capabilities in the new user namespace
		// until the call to execve(2) causes them to be recalculated. This
		// includes the CAP_SETUID and CAP_SETGID necessary to become UID 0 in
		// the child user namespace, effectively dropping privileges to UID
		// 100000 in the parent user namespace.
		//
		// https://github.com/golang/go/blob/1b03568/src/syscall/exec_linux.go#L446
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: 0, Gid: 0}
	}

	stdio, err := StdioPipes(cmd, r.rootUID, r.rootGID)
	if err != nil {
		return nil, errors.Wrap(err, errCreateStdioPipes)
	}

	if err := cmd.Start(); err != nil {
		return nil, errors.Wrap(err, errStartFunction)
	}

	if _, err := stdio.Stdin.Write(y); err != nil {
		return nil, errors.Wrap(err, errWriteFunctionIO)
	}

	// Closing the write end of the stdio pipe will cause the read end to return
	// EOF. This is necessary to avoid a function blocking forever while reading
	// from stdin.
	if err := stdio.Stdin.Close(); err != nil {
		return nil, errors.Wrap(err, errCloseStdin)
	}

	// We must read all of stdout and stderr before calling cmd.Wait, which
	// closes the underlying pipes.
	stdout, err := io.ReadAll(stdio.Stdout)
	if err != nil {
		return nil, errors.Wrap(err, errReadStdout)
	}

	stderr, err := io.ReadAll(stdio.Stderr)
	if err != nil {
		return nil, errors.Wrap(err, errReadStdout)
	}

	if err := cmd.Wait(); err != nil {
		err = errors.Wrap(err, errExecFn)
		if len(stderr) != 0 {
			// TODO(negz): Handle stderr being too long.
			return nil, errors.Errorf("%w: %s", err, bytes.TrimSuffix(stderr, []byte("\n")))
		}
		return nil, err
	}

	out := &v1alpha1.FunctionIO{}
	return out, errors.Wrap(yaml.Unmarshal(stdout, out), errInvalidOutput)
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
