/*
 * Copyright 2023 The Kubernetes Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/urfave/cli/v2"

	"github.com/RBLN-SW/k8s-dra-driver-npu/pkg/consts"
	"github.com/RBLN-SW/k8s-dra-driver-npu/pkg/flags"
	"github.com/RBLN-SW/k8s-dra-driver-npu/pkg/logging"
	coreclientset "k8s.io/client-go/kubernetes"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
)

const (
	DriverPluginCheckpointFile = "checkpoint.json"
)

// version is stamped at build time with -ldflags "-X main.version=...". It is
// reported in the startup record because "which build is running" is the first
// thing an operator needs when correlating a bug report with a log stream.
var version = "devel"

type Flags struct {
	kubeClientConfig flags.KubeClientConfig

	nodeName                      string
	cdiRoot                       string
	kubeletRegistrarDirectoryPath string
	kubeletPluginsDirectoryPath   string
	healthcheckPort               int
	driverName                    string
}

type Config struct {
	flags         *Flags
	coreclient    coreclientset.Interface
	cancelMainCtx func(error)
}

func (c Config) DriverPluginPath() string {
	return filepath.Join(c.flags.kubeletPluginsDirectoryPath, c.flags.driverName)
}

func main() {
	// The contract logger comes first so everything below emits through it.
	level, format := logging.SetupFromEnv()
	// Route klog (kubeletplugin helper, client-go, utilruntime) through slog and
	// raise klog's own V(n) gate to match; also enables contextual logging so
	// ctx-derived loggers share the handler.
	logging.BridgeKlog(level)
	// glog (rblnlib-go rsdgroup/rblnsmi) defaults to files under /tmp; send it
	// to stderr so RSD-group failures reach the container log stream. Ignore
	// the error defensively in case a future rblnlib-go drops glog.
	_ = flag.Set("logtostderr", "true")

	slog.Info("Starting npu-kubelet-plugin",
		"version", version, "logLevel", level, "logFormat", format)
	if err := newApp().Run(os.Args); err != nil {
		slog.Error("Command failed", "err", err)
		os.Exit(1)
	}
}

func newApp() *cli.App {
	flags := &Flags{}
	cliFlags := []cli.Flag{
		&cli.StringFlag{
			Name:        "node-name",
			Usage:       "The name of the node to be worked on.",
			Required:    true,
			Destination: &flags.nodeName,
			EnvVars:     []string{"NODE_NAME"},
		},
		&cli.StringFlag{
			Name:        "cdi-root",
			Usage:       "Absolute path to the directory where CDI files will be generated.",
			Value:       "/etc/cdi",
			Destination: &flags.cdiRoot,
			EnvVars:     []string{"CDI_ROOT"},
		},
		&cli.StringFlag{
			Name:        "kubelet-registrar-directory-path",
			Usage:       "Absolute path to the directory where kubelet stores plugin registrations.",
			Value:       kubeletplugin.KubeletRegistryDir,
			Destination: &flags.kubeletRegistrarDirectoryPath,
			EnvVars:     []string{"KUBELET_REGISTRAR_DIRECTORY_PATH"},
		},
		&cli.StringFlag{
			Name:        "kubelet-plugins-directory-path",
			Usage:       "Absolute path to the directory where kubelet stores plugin data.",
			Value:       kubeletplugin.KubeletPluginsDir,
			Destination: &flags.kubeletPluginsDirectoryPath,
			EnvVars:     []string{"KUBELET_PLUGINS_DIRECTORY_PATH"},
		},
		&cli.IntFlag{
			Name:        "healthcheck-port",
			Usage:       "Port to start a gRPC healthcheck service. When positive, a literal port number. When zero, a random port is allocated. When negative, the healthcheck service is disabled.",
			Value:       -1,
			Destination: &flags.healthcheckPort,
			EnvVars:     []string{"HEALTHCHECK_PORT"},
		},
		&cli.StringFlag{
			Name:        "driver-name",
			Usage:       "Name of the DRA driver.",
			Destination: &flags.driverName,
			EnvVars:     []string{"DRIVER_NAME"},
		},
	}
	cliFlags = append(cliFlags, flags.kubeClientConfig.Flags()...)

	app := &cli.App{
		Name:            "npu-kubelet-plugin",
		Usage:           "npu-kubelet-plugin implements a DRA driver plugin.",
		ArgsUsage:       " ",
		HideHelpCommand: true,
		Flags:           cliFlags,
		Before: func(c *cli.Context) error {
			if c.Args().Len() > 0 {
				return fmt.Errorf("arguments not supported: %v", c.Args().Slice())
			}
			return nil
		},
		Action: func(c *cli.Context) error {
			ctx := c.Context
			clientSets, err := flags.kubeClientConfig.NewClientSets()
			if err != nil {
				return fmt.Errorf("create client: %w", err)
			}

			if flags.driverName == "" {
				flags.driverName = consts.DriverName
			}

			config := &Config{
				flags:      flags,
				coreclient: clientSets.Core,
			}

			return RunPlugin(ctx, config)
		},
	}

	return app
}

func RunPlugin(ctx context.Context, config *Config) error {
	err := os.MkdirAll(config.DriverPluginPath(), 0750)
	if err != nil {
		return err
	}

	info, err := os.Stat(config.flags.cdiRoot)
	switch {
	case err != nil && os.IsNotExist(err):
		err := os.MkdirAll(config.flags.cdiRoot, 0750)
		if err != nil {
			return err
		}
	case err != nil:
		return err
	case !info.IsDir():
		return fmt.Errorf("path for CDI file generation is not a directory: %q", config.flags.cdiRoot)
	}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	defer stop()
	ctx, cancel := context.WithCancelCause(ctx)
	config.cancelMainCtx = cancel

	driver, err := NewDriver(ctx, config)
	if err != nil {
		return err
	}

	<-ctx.Done()
	stop()
	if err := context.Cause(ctx); err != nil && !errors.Is(err, context.Canceled) {
		// The error itself was already logged once by HandleError via
		// utilruntime; record only the shutdown reason here. Warn, not info:
		// this is the branch operators alert on.
		slog.Warn("Shutting down after fatal background error", "err", err)
	} else {
		slog.Info("Shutting down on signal")
	}

	err = driver.Shutdown()
	if err != nil {
		slog.Error("Failed to shut down driver cleanly", "err", err)
	}

	return nil
}
