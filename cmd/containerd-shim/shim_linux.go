/*
   Copyright The containerd Authors.

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

package main

import (
	"os"
	"os/signal"

	"github.com/containerd/containerd/runtime/v1/shim"
	runc "github.com/containerd/go-runc"
	"github.com/containerd/ttrpc"
	"github.com/opencontainers/runc/libcontainer/system"
	"golang.org/x/sys/unix"
)

// setupSignals creates a new signal handler for all signals and sets the shim as a
// sub-reaper so that the container processes are reparented
func setupSignals() (chan os.Signal, error) {
	signals := make(chan os.Signal, 32)
	signal.Notify(signals, unix.SIGTERM, unix.SIGINT, unix.SIGCHLD, unix.SIGPIPE)
	// make sure runc is setup to use the monitor
	// for waiting on processes
	runc.Monitor = shim.Default //替换runc默认的进程的监控器
	// set the shim as the subreaper for all orphaned processes created by the container
	//  prctl: If arg2 is nonzero, set the "child subreaper" attribute of the calling process; if arg2 is zero, unset the attribute.
	if err := system.SetSubreaper(1); err != nil {
		return nil, err
	}
	return signals, nil
}

func newServer() (*ttrpc.Server, error) {
	return ttrpc.NewServer(ttrpc.WithServerHandshaker(ttrpc.UnixSocketRequireSameUser()))
}
