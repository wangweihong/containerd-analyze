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

package plugin

import (
	"fmt"
	"sync"

	"github.com/pkg/errors"
	"google.golang.org/grpc"
)

var (
	// ErrNoType is returned when no type is specified
	ErrNoType = errors.New("plugin: no type")
	// ErrNoPluginID is returned when no id is specified
	ErrNoPluginID = errors.New("plugin: no id")

	// ErrSkipPlugin is used when a plugin is not initialized and should not be loaded,
	// this allows the plugin loader differentiate between a plugin which is configured
	// not to load and one that fails to load.
	ErrSkipPlugin = errors.New("skip plugin")

	// ErrInvalidRequires will be thrown if the requirements for a plugin are
	// defined in an invalid manner.
	ErrInvalidRequires = errors.New("invalid requires")
)

// IsSkipPlugin returns true if the error is skipping the plugin
func IsSkipPlugin(err error) bool {
	return errors.Cause(err) == ErrSkipPlugin
}

// Type is the type of the plugin
type Type string

func (t Type) String() string { return string(t) }

// containerd默认的插件
const (
	// InternalPlugin implements an internal plugin to containerd
	InternalPlugin Type = "io.containerd.internal.v1"
	// RuntimePlugin implements a runtime
	RuntimePlugin Type = "io.containerd.runtime.v1"
	// RuntimePluginV2 implements a runtime v2
	RuntimePluginV2 Type = "io.containerd.runtime.v2"
	// ServicePlugin implements a internal service
	ServicePlugin Type = "io.containerd.service.v1" //为什么 service/contaienrs中同时注册了 ServicePlugin以及GRPC Plugin?
	// GRPCPlugin implements a grpc service
	GRPCPlugin Type = "io.containerd.grpc.v1" //对外的服务？
	// SnapshotPlugin implements a snapshotter
	SnapshotPlugin Type = "io.containerd.snapshotter.v1"
	// TaskMonitorPlugin implements a task monitor
	TaskMonitorPlugin Type = "io.containerd.monitor.v1"
	// DiffPlugin implements a differ
	DiffPlugin Type = "io.containerd.differ.v1"
	// MetadataPlugin implements a metadata store
	MetadataPlugin Type = "io.containerd.metadata.v1"
	// ContentPlugin implements a content store
	ContentPlugin Type = "io.containerd.content.v1"
	// GCPlugin implements garbage collection policy
	GCPlugin Type = "io.containerd.gc.v1"
)

// Registration contains information for registering a plugin
type Registration struct {
	// Type of the plugin
	Type Type //插件类型如：io.containerd.runtime.v1(插件类型为运行时)
	// ID of the plugin
	ID string //插件ID  io.containerd.runtime.v1这类型插件可能有多个不同的插件， 如linux的，如 windows. 这些就是特定插件的ID
	// Config specific to the plugin
	Config interface{} // 插件自定义的配置, 每个插件都有各自不同的配置。 containerd配置文件可以替换插件的配置
	// Requires is a list of plugins that the registered plugin requires to be available
	Requires []Type

	// InitFn is called when initializing a plugin. The registration and
	// context are passed in. The init function may modify the registration to
	// add exports, capabilities and platform support declarations.
	InitFn func(*InitContext) (interface{}, error) //各个插件自定义的初始化函数
}

// Init the registered plugin
func (r *Registration) Init(ic *InitContext) *Plugin {
	p, err := r.InitFn(ic)
	return &Plugin{
		Registration: r,
		Config:       ic.Config,
		Meta:         ic.Meta,
		instance:     p,
		err:          err,
	}
}

// URI returns the full plugin URI
//  v1/runtime插件 类型io.containerd.runtime.v1 ID为linux
func (r *Registration) URI() string {
	return fmt.Sprintf("%s.%s", r.Type, r.ID)
}

// Service allows GRPC services to be registered with the underlying server
//就是将这些插件实现的服务添加到grpc server中
type Service interface {
	Register(*grpc.Server) error
}

//记录所有插件的注册？
var register = struct {
	sync.RWMutex
	r []*Registration
}{}

// Load loads all plugins at the provided path into containerd
func Load(path string) (err error) {
	defer func() {
		if v := recover(); v != nil {
			rerr, ok := v.(error)
			if !ok {
				rerr = fmt.Errorf("%s", v)
			}
			err = rerr
		}
	}()
	return loadPlugins(path)
}

// Register allows plugins to register
func Register(r *Registration) {
	register.Lock()
	defer register.Unlock()

	//注册必须有ID和类型
	if r.Type == "" {
		panic(ErrNoType)
	}
	if r.ID == "" {
		panic(ErrNoPluginID)
	}

	var last bool
	// 依赖的插件？
	for _, requires := range r.Requires {
		if requires == "*" {
			last = true
		}
	}
	if last && len(r.Requires) != 1 {
		panic(ErrInvalidRequires)
	}

	register.r = append(register.r, r)
}

// Graph0 returns an ordered list of registered plugins for initialization.
// Plugins in disableList specified by id will be disabled.
func Graph(disableList []string) (ordered []*Registration) {
	register.RLock()
	defer register.RUnlock()
	for _, d := range disableList {
		for i, r := range register.r {
			if r.ID == d {
				register.r = append(register.r[:i], register.r[i+1:]...)
				break
			}
		}
	}

	added := map[*Registration]bool{}
	for _, r := range register.r {

		children(r.ID, r.Requires, added, &ordered)
		if !added[r] {
			ordered = append(ordered, r)
			added[r] = true
		}
	}
	return ordered
}

func children(id string, types []Type, added map[*Registration]bool, ordered *[]*Registration) {
	for _, t := range types {
		for _, r := range register.r {
			if r.ID != id && (t == "*" || r.Type == t) {
				children(r.ID, r.Requires, added, ordered)
				if !added[r] {
					*ordered = append(*ordered, r)
					added[r] = true
				}
			}
		}
	}
}
