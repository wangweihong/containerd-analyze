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

package config

import (
	"github.com/BurntSushi/toml"
	"github.com/containerd/containerd/errdefs"
	"github.com/pkg/errors"
)

// Config provides containerd configuration data for the server
type Config struct {
	// Root is the path to a directory where containerd will store persistent data
	Root string `toml:"root"` // 默认为 /var/lib/containerd
	// State is the path to a directory where containerd will store transient data
	State string `toml:"state"` // 默认为 /run/containerd
	// GRPC configuration settings
	GRPC GRPCConfig `toml:"grpc"`
	// Debug and profiling settings
	Debug Debug `toml:"debug"` //包含调试服务器的配置
	// Metrics and monitoring settings
	Metrics MetricsConfig `toml:"metrics"` //包含指标服务器的配置
	// DisabledPlugins are IDs of plugins to disable. Disabled plugins won't be
	// initialized and started.
	DisabledPlugins []string `toml:"disabled_plugins"`
	// Plugins provides plugin specific configuration for the initialization of a plugin
	Plugins map[string]toml.Primitive `toml:"plugins"` // 插件配置，每个插件都有默认的配置，这里会更改配置的特定项
	// OOMScore adjust the containerd's oom score
	OOMScore int `toml:"oom_score"`
	// Cgroup specifies cgroup information for the containerd daemon process
	Cgroup CgroupConfig `toml:"cgroup"`
	// ProxyPlugins configures plugins which are communicated to over GRPC
	ProxyPlugins map[string]ProxyPlugin `toml:"proxy_plugins"`

	md toml.MetaData //通过加载toml配置文件的得到。
}

// GRPCConfig provides GRPC configuration for the socket
type GRPCConfig struct {
	Address        string `toml:"address"` //默认 /run/containerd/containerd.sock
	UID            int    `toml:"uid"`
	GID            int    `toml:"gid"`
	MaxRecvMsgSize int    `toml:"max_recv_message_size"` //默认 16MB
	MaxSendMsgSize int    `toml:"max_send_message_size"` //默认 16MB
}

// Debug provides debug configuration
type Debug struct {
	Address string `toml:"address"`
	UID     int    `toml:"uid"`
	GID     int    `toml:"gid"`
	Level   string `toml:"level"`
}

// MetricsConfig provides metrics configuration
type MetricsConfig struct {
	Address       string `toml:"address"`
	GRPCHistogram bool   `toml:"grpc_histogram"`
}

// CgroupConfig provides cgroup configuration
type CgroupConfig struct {
	Path string `toml:"path"`
}

// ProxyPlugin provides a proxy plugin configuration
type ProxyPlugin struct {
	Type    string `toml:"type"`
	Address string `toml:"address"`
}

// Decode unmarshals a plugin specific configuration by plugin id
func (c *Config) Decode(id string, v interface{}) (interface{}, error) {
	data, ok := c.Plugins[id]
	if !ok {
		return v, nil
	}
	if err := c.md.PrimitiveDecode(data, v); err != nil {
		return nil, err
	}
	return v, nil
}

// LoadConfig loads the containerd server config from the provided path
func LoadConfig(path string, v *Config) error {
	if v == nil {
		return errors.Wrapf(errdefs.ErrInvalidArgument, "argument v must not be nil")
	}
	md, err := toml.DecodeFile(path, v)
	if err != nil {
		return err
	}
	v.md = md
	return nil
}
