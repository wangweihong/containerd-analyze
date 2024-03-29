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

package server

import (
	"context"
	"expvar"
	"io"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	csapi "github.com/containerd/containerd/api/services/content/v1"
	ssapi "github.com/containerd/containerd/api/services/snapshots/v1"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/content/local"
	csproxy "github.com/containerd/containerd/content/proxy"
	"github.com/containerd/containerd/defaults"
	"github.com/containerd/containerd/events/exchange"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/metadata"
	"github.com/containerd/containerd/pkg/dialer"
	"github.com/containerd/containerd/plugin"
	srvconfig "github.com/containerd/containerd/services/server/config"
	"github.com/containerd/containerd/snapshots"
	ssproxy "github.com/containerd/containerd/snapshots/proxy"
	metrics "github.com/docker/go-metrics"
	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/pkg/errors"
	bolt "go.etcd.io/bbolt"
	"google.golang.org/grpc"
)

// New creates and initializes a new containerd server
func New(ctx context.Context, config *srvconfig.Config) (*Server, error) {
	switch {
	case config.Root == "":
		return nil, errors.New("root must be specified")
	case config.State == "":
		return nil, errors.New("state must be specified")
	case config.Root == config.State:
		return nil, errors.New("root and state must be different paths")
	}

	if err := os.MkdirAll(config.Root, 0711); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(config.State, 0711); err != nil {
		return nil, err
	}
	//设置进程的OOM_score以及加入cgroup
	if err := apply(ctx, config); err != nil {
		return nil, err
	}

	//加载所有的插件
	//注意：1.部分插件通过配置引入
	//2. 部分插件在LoadPlugins中显示引入
	//3. 部分服务在containerd启动时通过init()以插件的形式进行注册。
	plugins, err := LoadPlugins(ctx, config)
	if err != nil {
		return nil, err
	}

	serverOpts := []grpc.ServerOption{
		grpc.UnaryInterceptor(grpc_prometheus.UnaryServerInterceptor),
		grpc.StreamInterceptor(grpc_prometheus.StreamServerInterceptor),
	}
	if config.GRPC.MaxRecvMsgSize > 0 {
		serverOpts = append(serverOpts, grpc.MaxRecvMsgSize(config.GRPC.MaxRecvMsgSize))
	}
	if config.GRPC.MaxSendMsgSize > 0 {
		serverOpts = append(serverOpts, grpc.MaxSendMsgSize(config.GRPC.MaxSendMsgSize))
	}

	//创建一个grpc服务器
	rpc := grpc.NewServer(serverOpts...)
	var (
		services []plugin.Service
		s        = &Server{
			rpc:    rpc,
			events: exchange.NewExchange(),
			config: config,
		}
		initialized = plugin.NewPluginSet()
	)

	//遍历所有插件
	for _, p := range plugins {
		id := p.URI() // v1/runtime插件 io.containerd.runtime.v1.linux
		// contained启动时打印的日子 msg="loading plugin "io.containerd.runtime.v1.linux"..." type=io.containerd.runtime.v1
		log.G(ctx).WithField("type", p.Type).Infof("loading plugin %q...", id)

		//插件初始化用的上下文,
		initContext := plugin.NewContext(
			ctx,
			p,            // 插件注册程序
			initialized,  // 已初始化插件表。这个是一个指针，所有组件初始化成功都会添加到该表中。
			config.Root,  // 默认 /var/lib/containerd
			config.State, // 默认 /run/containerd
		)
		initContext.Events = s.events
		initContext.Address = config.GRPC.Address //默认时 /run/containerd/containerd.sock

		// load the plugin specific configuration if it is provided
		if p.Config != nil {
			// 如果containerd的配置中提供了特定插件的配置选项， 则
			// 覆盖插件默认的配置选项。
			pluginConfig, err := config.Decode(p.ID, p.Config)
			if err != nil {
				return nil, err
			}
			initContext.Config = pluginConfig
		}

		// 调用插件自定义的初始化函数
		result := p.Init(initContext)
		if err := initialized.Add(result); err != nil {
			return nil, errors.Wrapf(err, "could not add plugin result to plugin set")
		}

		instance, err := result.Instance() //插件执行初始化函数后生成的插件实例
		if err != nil {
			if plugin.IsSkipPlugin(err) {
				log.G(ctx).WithField("type", p.Type).Infof("skip loading plugin %q...", id)
			} else {
				log.G(ctx).WithError(err).Warnf("failed to load plugin %s", id)
			}
			continue
		}
		// check for grpc services that should be registered with the server
		// 如果插件实例实现了注册函数，待会也会将注册到grpc服务。
		if service, ok := instance.(plugin.Service); ok {
			services = append(services, service)
		}
		s.plugins = append(s.plugins, result)
	}
	// register services after all plugins have been initialized
	// 每个服务都将自己的api注册到grpc server中。
	for _, service := range services {
		if err := service.Register(rpc); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Server is the containerd main daemon
type Server struct {
	rpc     *grpc.Server
	events  *exchange.Exchange
	config  *srvconfig.Config
	plugins []*plugin.Plugin
}

// ServeGRPC provides the containerd grpc APIs on the provided listener
func (s *Server) ServeGRPC(l net.Listener) error {
	// grpc柱状图
	if s.config.Metrics.GRPCHistogram {
		// enable grpc time histograms to measure rpc latencies
		grpc_prometheus.EnableHandlingTimeHistogram() // 启动promethus 对grpc的采集
	}
	// before we start serving the grpc API register the grpc_prometheus metrics
	// handler.  This needs to be the last service registered so that it can collect
	// metrics for every other service
	grpc_prometheus.Register(s.rpc) //？p
	return trapClosedConnErr(s.rpc.Serve(l))
}

// ServeMetrics provides a prometheus endpoint for exposing metrics
func (s *Server) ServeMetrics(l net.Listener) error {
	m := http.NewServeMux()
	m.Handle("/v1/metrics", metrics.Handler())
	return trapClosedConnErr(http.Serve(l, m))
}

// ServeDebug provides a debug endpoint
func (s *Server) ServeDebug(l net.Listener) error {
	// don't use the default http server mux to make sure nothing gets registered
	// that we don't want to expose via containerd
	m := http.NewServeMux()
	m.Handle("/debug/vars", expvar.Handler())
	m.Handle("/debug/pprof/", http.HandlerFunc(pprof.Index))
	m.Handle("/debug/pprof/cmdline", http.HandlerFunc(pprof.Cmdline))
	m.Handle("/debug/pprof/profile", http.HandlerFunc(pprof.Profile))
	m.Handle("/debug/pprof/symbol", http.HandlerFunc(pprof.Symbol))
	m.Handle("/debug/pprof/trace", http.HandlerFunc(pprof.Trace))
	return trapClosedConnErr(http.Serve(l, m))
}

// Stop the containerd server canceling any open connections
func (s *Server) Stop() {
	s.rpc.Stop()
	for i := len(s.plugins) - 1; i >= 0; i-- {
		p := s.plugins[i]
		instance, err := p.Instance()
		if err != nil {
			log.L.WithError(err).WithField("id", p.Registration.ID).
				Errorf("could not get plugin instance")
			continue
		}
		closer, ok := instance.(io.Closer)
		if !ok {
			continue
		}
		if err := closer.Close(); err != nil {
			log.L.WithError(err).WithField("id", p.Registration.ID).
				Errorf("failed to close plugin")
		}
	}
}

// LoadPlugins loads all plugins into containerd and generates an ordered graph
// of all plugins.
// 加载所有的插件，移除配置中明确禁止的插件，返回插件的注册信息
func LoadPlugins(ctx context.Context, config *srvconfig.Config) ([]*plugin.Registration, error) {
	// load all plugins into containerd
	if err := plugin.Load(filepath.Join(config.Root, "plugins")); err != nil {
		return nil, err
	}
	// load additional plugins that don't automatically register themselves
	plugin.Register(&plugin.Registration{
		Type: plugin.ContentPlugin,
		ID:   "content",
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			ic.Meta.Exports["root"] = ic.Root
			return local.NewStore(ic.Root)
		},
	})
	plugin.Register(&plugin.Registration{
		Type: plugin.MetadataPlugin,
		ID:   "bolt",
		Requires: []plugin.Type{
			plugin.ContentPlugin,
			plugin.SnapshotPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			if err := os.MkdirAll(ic.Root, 0711); err != nil {
				return nil, err
			}
			cs, err := ic.Get(plugin.ContentPlugin)
			if err != nil {
				return nil, err
			}

			snapshottersRaw, err := ic.GetByType(plugin.SnapshotPlugin)
			if err != nil {
				return nil, err
			}

			snapshotters := make(map[string]snapshots.Snapshotter)
			for name, sn := range snapshottersRaw {
				sn, err := sn.Instance()
				if err != nil {
					log.G(ic.Context).WithError(err).
						Warnf("could not use snapshotter %v in metadata plugin", name)
					continue
				}
				snapshotters[name] = sn.(snapshots.Snapshotter)
			}

			path := filepath.Join(ic.Root, "meta.db")
			ic.Meta.Exports["path"] = path

			db, err := bolt.Open(path, 0644, nil)
			if err != nil {
				return nil, err
			}
			mdb := metadata.NewDB(db, cs.(content.Store), snapshotters)
			if err := mdb.Init(ic.Context); err != nil {
				return nil, err
			}
			return mdb, nil
		},
	})

	clients := &proxyClients{}
	for name, pp := range config.ProxyPlugins {
		var (
			t plugin.Type
			f func(*grpc.ClientConn) interface{}

			address = pp.Address
		)

		switch pp.Type {
		case string(plugin.SnapshotPlugin), "snapshot":
			t = plugin.SnapshotPlugin
			ssname := name
			f = func(conn *grpc.ClientConn) interface{} {
				return ssproxy.NewSnapshotter(ssapi.NewSnapshotsClient(conn), ssname)
			}

		case string(plugin.ContentPlugin), "content":
			t = plugin.ContentPlugin
			f = func(conn *grpc.ClientConn) interface{} {
				return csproxy.NewContentStore(csapi.NewContentClient(conn))
			}
		default:
			log.G(ctx).WithField("type", pp.Type).Warn("unknown proxy plugin type")
		}

		plugin.Register(&plugin.Registration{
			Type: t,
			ID:   name,
			InitFn: func(ic *plugin.InitContext) (interface{}, error) {
				ic.Meta.Exports["address"] = address
				conn, err := clients.getClient(address)
				if err != nil {
					return nil, err
				}
				return f(conn), nil
			},
		})

	}

	// return the ordered graph for plugins
	return plugin.Graph(config.DisabledPlugins), nil
}

type proxyClients struct {
	m       sync.Mutex
	clients map[string]*grpc.ClientConn
}

func (pc *proxyClients) getClient(address string) (*grpc.ClientConn, error) {
	pc.m.Lock()
	defer pc.m.Unlock()
	if pc.clients == nil {
		pc.clients = map[string]*grpc.ClientConn{}
	} else if c, ok := pc.clients[address]; ok {
		return c, nil
	}

	gopts := []grpc.DialOption{
		grpc.WithInsecure(),
		grpc.WithBackoffMaxDelay(3 * time.Second),
		grpc.WithDialer(dialer.Dialer),

		// TODO(stevvooe): We may need to allow configuration of this on the client.
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(defaults.DefaultMaxRecvMsgSize)),
		grpc.WithDefaultCallOptions(grpc.MaxCallSendMsgSize(defaults.DefaultMaxSendMsgSize)),
	}

	conn, err := grpc.Dial(dialer.DialAddress(address), gopts...)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to dial %q", address)
	}

	pc.clients[address] = conn

	return conn, nil
}

func trapClosedConnErr(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "use of closed network connection") {
		return nil
	}
	return err
}
