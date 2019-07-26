// +build linux

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

package linux

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"time"

	eventstypes "github.com/containerd/containerd/api/events"
	"github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/containers"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/events/exchange"
	"github.com/containerd/containerd/identifiers"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/metadata"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/platforms"
	"github.com/containerd/containerd/plugin"
	"github.com/containerd/containerd/runtime"
	"github.com/containerd/containerd/runtime/linux/runctypes"
	"github.com/containerd/containerd/runtime/v1/linux/proc"
	shim "github.com/containerd/containerd/runtime/v1/shim/v1"
	runc "github.com/containerd/go-runc"
	"github.com/containerd/typeurl"
	ptypes "github.com/gogo/protobuf/types"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	bolt "go.etcd.io/bbolt"
	"golang.org/x/sys/unix"
)

var (
	pluginID = fmt.Sprintf("%s.%s", plugin.RuntimePlugin, "linux")
	empty    = &ptypes.Empty{}
)

const (
	configFilename = "config.json"
	defaultRuntime = "runc"
	defaultShim    = "containerd-shim"
)

func init() {
	plugin.Register(&plugin.Registration{
		Type:   plugin.RuntimePlugin, //插件类型
		ID:     "linux",
		InitFn: New, // 插件初始
		Requires: []plugin.Type{
			plugin.MetadataPlugin, //io.containerd.metadata.v1
		},
		Config: &Config{
			Shim:    defaultShim,    //container-shim
			Runtime: defaultRuntime, //runc
		},
	})
}

var _ = (runtime.PlatformRuntime)(&Runtime{})

// Config options for the runtime
// 运行插件自定义的配置
type Config struct {
	// Shim is a path or name of binary implementing the Shim GRPC API
	Shim string `toml:"shim"`
	// Runtime is a path or name of an OCI runtime used by the shim
	Runtime string `toml:"runtime"`
	// RuntimeRoot is the path that shall be used by the OCI runtime for its data
	RuntimeRoot string `toml:"runtime_root"`
	// NoShim calls runc directly from within the pkg
	NoShim bool `toml:"no_shim"`
	// Debug enable debug on the shim
	ShimDebug bool `toml:"shim_debug"`
}

// New returns a configured runtime
// plugin.InitContext是在containerd初始化时, 初始化的上下文，包括插件的根目录/运行目录等。
func New(ic *plugin.InitContext) (interface{}, error) {
	ic.Meta.Platforms = []ocispec.Platform{platforms.DefaultSpec()}

	// /var/lib/containerd/io.containerd.runtime.v1.linux
	if err := os.MkdirAll(ic.Root, 0711); err != nil {
		return nil, err
	}

	// /run/containerd/io.containerd.runtime.v1.linux
	if err := os.MkdirAll(ic.State, 0711); err != nil {
		return nil, err
	}
	//获取io.containerd.metadata.v1类型插件
	m, err := ic.Get(plugin.MetadataPlugin)
	if err != nil {
		return nil, err
	}
	cfg := ic.Config.(*Config)
	r := &Runtime{
		root:    ic.Root,
		state:   ic.State,
		tasks:   runtime.NewTaskList(), //现在这个表是空的，加载任务后在填充
		db:      m.(*metadata.DB),
		address: ic.Address, // /run/containerd/container.sock
		events:  ic.Events,
		config:  cfg, //插件的配置
	}
	tasks, err := r.restoreTasks(ic.Context)
	if err != nil {
		return nil, err
	}
	for _, t := range tasks {
		//填充任务到任务列表中
		if err := r.tasks.AddWithNamespace(t.namespace, t); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// Runtime for a linux based system
type Runtime struct {
	root    string // 插件中定义的root .一般而言记录/var/lib/containerd/io.containerd.runtime.v1.linux/
	state   string // 插件中定义的state. /run/containerd/io.containerd.runtime.v1.linux/
	address string // containerd的socket

	tasks  *runtime.TaskList //任务列表，在New()加载插件state目录下各命名空间的任务后填充。
	db     *metadata.DB      // io.containerd.metadata.v1插件
	events *exchange.Exchange

	config *Config
}

// ID of the runtime
func (r *Runtime) ID() string {
	return pluginID
}

// Create a new task
func (r *Runtime) Create(ctx context.Context, id string, opts runtime.CreateOpts) (_ runtime.Task, err error) {
	//从context中提取命名空间
	namespace, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, err
	}

	if err := identifiers.Validate(id); err != nil {
		return nil, errors.Wrapf(err, "invalid task id")
	}

	ropts, err := r.getRuncOptions(ctx, id)
	if err != nil {
		return nil, err
	}

	bundle, err := newBundle(id,
		filepath.Join(r.state, namespace),
		filepath.Join(r.root, namespace),
		opts.Spec.Value)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			bundle.Delete()
		}
	}()

	shimopt := ShimLocal(r.config, r.events)
	if !r.config.NoShim {
		var cgroup string
		if opts.TaskOptions != nil {
			v, err := typeurl.UnmarshalAny(opts.TaskOptions)
			if err != nil {
				return nil, err
			}
			cgroup = v.(*runctypes.CreateOptions).ShimCgroup
		}
		exitHandler := func() {
			log.G(ctx).WithField("id", id).Info("shim reaped")
			t, err := r.tasks.Get(ctx, id)
			if err != nil {
				// Task was never started or was already successfully deleted
				return
			}
			lc := t.(*Task)

			log.G(ctx).WithFields(logrus.Fields{
				"id":        id,
				"namespace": namespace,
			}).Warn("cleaning up after killed shim")
			if err = r.cleanupAfterDeadShim(context.Background(), bundle, namespace, id, lc.pid); err != nil {
				log.G(ctx).WithError(err).WithFields(logrus.Fields{
					"id":        id,
					"namespace": namespace,
				}).Warn("failed to clean up after killed shim")
			}
		}
		shimopt = ShimRemote(r.config, r.address, cgroup, exitHandler)
	}

	// container-shim客户端
	s, err := bundle.NewShimClient(ctx, namespace, shimopt, ropts)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			if kerr := s.KillShim(ctx); kerr != nil {
				log.G(ctx).WithError(err).Error("failed to kill shim")
			}
		}
	}()

	rt := r.config.Runtime
	if ropts != nil && ropts.Runtime != "" {
		rt = ropts.Runtime
	}
	sopts := &shim.CreateTaskRequest{
		ID:         id,
		Bundle:     bundle.path,
		Runtime:    rt,
		Stdin:      opts.IO.Stdin,
		Stdout:     opts.IO.Stdout,
		Stderr:     opts.IO.Stderr,
		Terminal:   opts.IO.Terminal,
		Checkpoint: opts.Checkpoint,
		Options:    opts.TaskOptions,
	}
	for _, m := range opts.Rootfs {
		sopts.Rootfs = append(sopts.Rootfs, &types.Mount{
			Type:    m.Type,
			Source:  m.Source,
			Options: m.Options,
		})
	}

	//调用contaienr-shim创建容器？
	cr, err := s.Create(ctx, sopts)
	if err != nil {
		return nil, errdefs.FromGRPC(err)
	}
	t, err := newTask(id, namespace, int(cr.Pid), s, r.events, r.tasks, bundle)
	if err != nil {
		return nil, err
	}
	if err := r.tasks.Add(ctx, t); err != nil {
		return nil, err
	}
	r.events.Publish(ctx, runtime.TaskCreateEventTopic, &eventstypes.TaskCreate{
		ContainerID: sopts.ID,
		Bundle:      sopts.Bundle,
		Rootfs:      sopts.Rootfs,
		IO: &eventstypes.TaskIO{
			Stdin:    sopts.Stdin,
			Stdout:   sopts.Stdout,
			Stderr:   sopts.Stderr,
			Terminal: sopts.Terminal,
		},
		Checkpoint: sopts.Checkpoint,
		Pid:        uint32(t.pid),
	})

	return t, nil
}

// Tasks returns all tasks known to the runtime
func (r *Runtime) Tasks(ctx context.Context, all bool) ([]runtime.Task, error) {
	return r.tasks.GetAll(ctx, all)
}

func (r *Runtime) restoreTasks(ctx context.Context) ([]*Task, error) {

	// 读取 插件/run/containerd/io.containerd.runtime.v1.linux/目录
	dir, err := ioutil.ReadDir(r.state)
	if err != nil {
		return nil, err
	}
	var o []*Task
	//state目录的子目录为命名空间
	for _, namespace := range dir {
		if !namespace.IsDir() {
			continue
		}
		name := namespace.Name() //命名空间名, moby
		log.G(ctx).WithField("namespace", name).Debug("loading tasks in namespace")
		tasks, err := r.loadTasks(ctx, name)
		if err != nil {
			return nil, err
		}
		o = append(o, tasks...)
	}
	return o, nil
}

// Get a specific task by task id
func (r *Runtime) Get(ctx context.Context, id string) (runtime.Task, error) {
	return r.tasks.Get(ctx, id)
}

// Add a runtime task
func (r *Runtime) Add(ctx context.Context, task runtime.Task) error {
	return r.tasks.Add(ctx, task)
}

// Delete a runtime task
func (r *Runtime) Delete(ctx context.Context, id string) {
	r.tasks.Delete(ctx, id)
}

// 加载指定命名空间的任务
func (r *Runtime) loadTasks(ctx context.Context, ns string) ([]*Task, error) {
	// /run/containerd/io.containerd.runtime.v1.linux/moby
	dir, err := ioutil.ReadDir(filepath.Join(r.state, ns))
	if err != nil {
		return nil, err
	}
	var o []*Task
	//加载该目录下的容器
	for _, path := range dir {
		if !path.IsDir() {
			continue
		}
		id := path.Name()
		bundle := loadBundle(
			id, // 任务ID
			filepath.Join(r.state, ns, id), // docker容器 /run/containerd/io.containerd.runtime.v1.linux/moby/<容器ID>
			filepath.Join(r.root, ns, id),  // docker容器  /run/containerd/io.containerd.runtime.v1.linux/moby/<容器ID>
		)
		ctx = namespaces.WithNamespace(ctx, ns)                                  // 将命名空间信息存在context中
		pid, _ := runc.ReadPidFile(filepath.Join(bundle.path, proc.InitPidFile)) // 读取init.pid文件获取容器运行pid

		// 建立containerd-shim客户端
		// 该客户端关闭时会 做清理，清理什么？
		s, err := bundle.NewShimClient(ctx, ns, ShimConnect(r.config, func() {
			err := r.cleanupAfterDeadShim(ctx, bundle, ns, id, pid)
			if err != nil {
				log.G(ctx).WithError(err).WithField("bundle", bundle.path).
					Error("cleaning up after dead shim")
			}
		}), nil)
		if err != nil {
			log.G(ctx).WithError(err).WithFields(logrus.Fields{
				"id":        id,
				"namespace": ns,
			}).Error("connecting to shim")
			err := r.cleanupAfterDeadShim(ctx, bundle, ns, id, pid)
			if err != nil {
				log.G(ctx).WithError(err).WithField("bundle", bundle.path).
					Error("cleaning up after dead shim")
			}
			continue
		}

		//构建新任务对象
		t, err := newTask(id, ns, pid, s, r.events, r.tasks, bundle)
		if err != nil {
			log.G(ctx).WithError(err).Error("loading task type")
			continue
		}
		o = append(o, t)
	}
	return o, nil
}

func (r *Runtime) cleanupAfterDeadShim(ctx context.Context, bundle *bundle, ns, id string, pid int) error {
	ctx = namespaces.WithNamespace(ctx, ns) //保存namespace到context中
	if err := r.terminate(ctx, bundle, ns, id); err != nil {
		if r.config.ShimDebug {
			return errors.Wrap(err, "failed to terminate task, leaving bundle for debugging")
		}
		log.G(ctx).WithError(err).Warn("failed to terminate task")
	}

	// Notify Client
	exitedAt := time.Now().UTC()
	//推送容器退出事件给containerd?
	r.events.Publish(ctx, runtime.TaskExitEventTopic, &eventstypes.TaskExit{
		ContainerID: id,
		ID:          id,
		Pid:         uint32(pid),
		ExitStatus:  128 + uint32(unix.SIGKILL),
		ExitedAt:    exitedAt,
	})

	//将任务移除出队列
	r.tasks.Delete(ctx, id)
	if err := bundle.Delete(); err != nil {
		log.G(ctx).WithError(err).Error("delete bundle")
	}

	r.events.Publish(ctx, runtime.TaskDeleteEventTopic, &eventstypes.TaskDelete{
		ContainerID: id,
		Pid:         uint32(pid),
		ExitStatus:  128 + uint32(unix.SIGKILL),
		ExitedAt:    exitedAt,
	})

	return nil
}

func (r *Runtime) terminate(ctx context.Context, bundle *bundle, ns, id string) error {
	rt, err := r.getRuntime(ctx, ns, id)
	if err != nil {
		return err
	}
	if err := rt.Delete(ctx, id, &runc.DeleteOpts{
		Force: true,
	}); err != nil {
		log.G(ctx).WithError(err).Warnf("delete runtime state %s", id)
	}
	if err := mount.Unmount(filepath.Join(bundle.path, "rootfs"), 0); err != nil {
		log.G(ctx).WithError(err).WithFields(logrus.Fields{
			"path": bundle.path,
			"id":   id,
		}).Warnf("unmount task rootfs")
	}
	return nil
}

func (r *Runtime) getRuntime(ctx context.Context, ns, id string) (*runc.Runc, error) {
	ropts, err := r.getRuncOptions(ctx, id)
	if err != nil {
		return nil, err
	}

	var (
		cmd  = r.config.Runtime
		root = proc.RuncRoot
	)
	if ropts != nil {
		if ropts.Runtime != "" {
			cmd = ropts.Runtime
		}
		if ropts.RuntimeRoot != "" {
			root = ropts.RuntimeRoot
		}
	}

	return &runc.Runc{
		Command:      cmd,
		LogFormat:    runc.JSON,
		PdeathSignal: unix.SIGKILL,
		Root:         filepath.Join(root, ns),
		Debug:        r.config.ShimDebug,
	}, nil
}

func (r *Runtime) getRuncOptions(ctx context.Context, id string) (*runctypes.RuncOptions, error) {
	var container containers.Container

	if err := r.db.View(func(tx *bolt.Tx) error {
		store := metadata.NewContainerStore(tx)
		var err error
		container, err = store.Get(ctx, id)
		return err
	}); err != nil {
		return nil, err
	}

	if container.Runtime.Options != nil {
		v, err := typeurl.UnmarshalAny(container.Runtime.Options)
		if err != nil {
			return nil, err
		}
		ropts, ok := v.(*runctypes.RuncOptions)
		if !ok {
			return nil, errors.New("invalid runtime options format")
		}

		return ropts, nil
	}
	return &runctypes.RuncOptions{}, nil
}
