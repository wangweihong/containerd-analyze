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

package command

import (
	gocontext "context"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/services/server"
	srvconfig "github.com/containerd/containerd/services/server/config"
	"github.com/containerd/containerd/sys"
	"github.com/containerd/containerd/version"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	"google.golang.org/grpc/grpclog"
)

const usage = `
                    __        _                     __
  _________  ____  / /_____ _(_)___  ___  _________/ /
 / ___/ __ \/ __ \/ __/ __ ` + "`" + `/ / __ \/ _ \/ ___/ __  /
/ /__/ /_/ / / / / /_/ /_/ / / / / /  __/ /  / /_/ /
\___/\____/_/ /_/\__/\__,_/_/_/ /_/\___/_/   \__,_/

high performance container runtime
`

func init() {
	logrus.SetFormatter(&logrus.TextFormatter{
		TimestampFormat: log.RFC3339NanoFixed,
		FullTimestamp:   true,
	})

	// Discard grpc logs so that they don't mess with our stdio
	grpclog.SetLoggerV2(grpclog.NewLoggerV2(ioutil.Discard, ioutil.Discard, ioutil.Discard))

	cli.VersionPrinter = func(c *cli.Context) {
		fmt.Println(c.App.Name, version.Package, c.App.Version, version.Revision)
	}
}

// App returns a *cli.App instance.
func App() *cli.App {
	app := cli.NewApp()
	app.Name = "containerd"
	app.Version = version.Version
	app.Usage = usage
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:  "config,c",
			Usage: "path to the configuration file",
			Value: defaultConfigPath,
		},
		cli.StringFlag{
			Name:  "log-level,l",
			Usage: "set the logging level [trace, debug, info, warn, error, fatal, panic]",
		},
		cli.StringFlag{
			Name:  "address,a",
			Usage: "address for containerd's GRPC server",
		},
		cli.StringFlag{
			Name:  "root",
			Usage: "containerd root directory",
		},
		cli.StringFlag{
			Name:  "state",
			Usage: "containerd state directory",
		},
	}
	app.Commands = []cli.Command{
		configCommand,
		publishCommand,
		ociHook,
	}
	app.Action = func(context *cli.Context) error {
		var (
			start   = time.Now()
			signals = make(chan os.Signal, 2048)
			serverC = make(chan *server.Server, 1)
			ctx     = gocontext.Background()
			config  = defaultConfig() //默认配置
		)

		//启动一个线程来监听系统信号
		done := handleSignals(ctx, signals, serverC)
		// start the signal handler as soon as we can to make sure that
		// we don't miss any signals during boot
		signal.Notify(signals, handledSignals...) //

		//根据参数，加载配置文件。 注意这里并没有覆盖默认配置。
		if err := srvconfig.LoadConfig(context.GlobalString("config"), config); err != nil && !os.IsNotExist(err) {
			return err
		}
		// apply flags to the config
		//根据标志覆盖配置
		if err := applyFlags(context, config); err != nil {
			return err
		}
		// cleanup temp mounts
		//在根目录下设置临时挂载
		if err := mount.SetTempMountLocation(filepath.Join(config.Root, "tmpmounts")); err != nil {
			return errors.Wrap(err, "creating temp mount location")
		}
		// unmount all temp mounts on boot for the server
		warnings, err := mount.CleanupTempMounts(0)
		if err != nil {
			log.G(ctx).WithError(err).Error("unmounting temp mounts")
		}
		for _, w := range warnings {
			log.G(ctx).WithError(w).Warn("cleanup temp mount")
		}
		address := config.GRPC.Address
		if address == "" {
			return errors.New("grpc address cannot be empty")
		}
		log.G(ctx).WithFields(logrus.Fields{
			"version":  version.Version,
			"revision": version.Revision,
		}).Info("starting containerd")

		//根据配置启动一个contaienrd服务器
		server, err := server.New(ctx, config)
		if err != nil {
			return err
		}
		serverC <- server
		if config.Debug.Address != "" {
			var l net.Listener
			if filepath.IsAbs(config.Debug.Address) {
				if l, err = sys.GetLocalListener(config.Debug.Address, config.Debug.UID, config.Debug.GID); err != nil {
					return errors.Wrapf(err, "failed to get listener for debug endpoint")
				}
			} else {
				if l, err = net.Listen("tcp", config.Debug.Address); err != nil {
					return errors.Wrapf(err, "failed to get listener for debug endpoint")
				}
			}
			serve(ctx, l, server.ServeDebug) //启动一个调试服务器
		}

		//如果指定了，将会启动一个metrics服务器
		if config.Metrics.Address != "" {
			l, err := net.Listen("tcp", config.Metrics.Address)
			if err != nil {
				return errors.Wrapf(err, "failed to get listener for metrics endpoint")
			}
			serve(ctx, l, server.ServeMetrics)
		}

		//创建unix socket文件，设置socket的uid和gid
		l, err := sys.GetLocalListener(address, config.GRPC.UID, config.GRPC.GID)
		if err != nil {
			return errors.Wrapf(err, "failed to get listener for main endpoint")
		}
		serve(ctx, l, server.ServeGRPC)

		log.G(ctx).Infof("containerd successfully booted in %fs", time.Since(start).Seconds())
		<-done
		return nil
	}
	return app
}

func serve(ctx gocontext.Context, l net.Listener, serveFunc func(net.Listener) error) {
	path := l.Addr().String()
	log.G(ctx).WithField("address", path).Info("serving...")
	go func() {
		defer l.Close()
		if err := serveFunc(l); err != nil {
			log.G(ctx).WithError(err).WithField("address", path).Fatal("serve failure")
		}
	}()
}

func applyFlags(context *cli.Context, config *srvconfig.Config) error {
	// the order for config vs flag values is that flags will always override
	// the config values if they are set
	if err := setLevel(context, config); err != nil {
		return err
	}
	for _, v := range []struct {
		name string
		d    *string
	}{
		{
			name: "root",
			d:    &config.Root,
		},
		{
			name: "state",
			d:    &config.State,
		},
		{
			name: "address",
			d:    &config.GRPC.Address,
		},
	} {
		//containerd的标志覆盖默认的
		if s := context.GlobalString(v.name); s != "" {
			*v.d = s
		}
	}
	return nil
}

func setLevel(context *cli.Context, config *srvconfig.Config) error {
	l := context.GlobalString("log-level")
	if l == "" {
		l = config.Debug.Level
	}
	if l != "" {
		lvl, err := log.ParseLevel(l)
		if err != nil {
			return err
		}
		logrus.SetLevel(lvl)
	}
	return nil
}
