/*
 * Copyright 2025 The Kubernetes Authors.
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
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"path"
	"strconv"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	drapb "k8s.io/kubelet/pkg/apis/dra/v1"
	registerapi "k8s.io/kubelet/pkg/apis/pluginregistration/v1"

	"github.com/RBLN-SW/k8s-dra-driver-npu/pkg/logging"
)

type healthcheck struct {
	grpc_health_v1.UnimplementedHealthServer

	server *grpc.Server
	wg     sync.WaitGroup

	regClient registerapi.RegistrationClient
	draClient drapb.DRAPluginClient
}

func startHealthcheck(ctx context.Context, config *Config) (*healthcheck, error) {
	port := config.flags.healthcheckPort
	if port < 0 {
		return nil, nil
	}

	logger := logging.FromContext(ctx)

	addr := net.JoinHostPort("", strconv.Itoa(port))
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("failed to listen for healthcheck service at %s: %w", addr, err)
	}

	regSockPath := (&url.URL{
		Scheme: "unix",
		// TODO: this needs to adapt when seamless upgrades
		// are enabled and the filename includes a uid.
		Path: path.Join(config.flags.kubeletRegistrarDirectoryPath, config.flags.driverName+"-reg.sock"),
	}).String()
	regConn, err := grpc.NewClient(
		regSockPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("connect to registration socket: %w", err)
	}

	draSockPath := (&url.URL{
		Scheme: "unix",
		Path:   path.Join(config.DriverPluginPath(), "dra.sock"),
	}).String()
	draConn, err := grpc.NewClient(
		draSockPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("connect to DRA socket: %w", err)
	}

	server := grpc.NewServer()
	healthcheck := &healthcheck{
		server:    server,
		regClient: registerapi.NewRegistrationClient(regConn),
		draClient: drapb.NewDRAPluginClient(draConn),
	}
	grpc_health_v1.RegisterHealthServer(server, healthcheck)

	// One record for the whole service rather than one per dialled socket: the
	// grpc.NewClient calls above are lazy, so their own records would only
	// have restated configuration the operator already set.
	logger.Info("Starting healthcheck service",
		"addr", lis.Addr().String(), "registrationSocket", regSockPath, "draSocket", draSockPath)

	healthcheck.wg.Add(1)
	go func() {
		defer healthcheck.wg.Done()
		// Serve returns nil once GracefulStop runs, so this only fires on a
		// real failure.
		if err := server.Serve(lis); err != nil {
			logger.Error("Failed to serve healthcheck service", "err", err, "addr", addr)
		}
	}()

	return healthcheck, nil
}

func (h *healthcheck) Stop() {
	if h.server != nil {
		slog.Debug("Stopping healthcheck service")
		h.server.GracefulStop()
	}
	h.wg.Wait()
}

// Check implements [grpc_health_v1.HealthServer].
func (h *healthcheck) Check(ctx context.Context, req *grpc_health_v1.HealthCheckRequest) (*grpc_health_v1.HealthCheckResponse, error) {
	knownServices := map[string]struct{}{"": {}, "liveness": {}}
	if _, known := knownServices[req.GetService()]; !known {
		return nil, status.Error(codes.NotFound, "unknown service")
	}

	status := &grpc_health_v1.HealthCheckResponse{
		Status: grpc_health_v1.HealthCheckResponse_NOT_SERVING,
	}
	logger := logging.FromContext(ctx)

	info, err := h.regClient.GetInfo(ctx, &registerapi.InfoRequest{})
	if err != nil {
		// Error, not warn: three of these in a row is what makes kubelet
		// restart the container, so it is the record an operator alerts on.
		logger.Error("Healthcheck failed calling GetInfo", "err", err)
		return status, nil
	}
	logger.Log(ctx, logging.LevelTrace, "Successfully invoked GetInfo", "info", info)

	_, err = h.draClient.NodePrepareResources(ctx, &drapb.NodePrepareResourcesRequest{})
	if err != nil {
		logger.Error("Healthcheck failed calling NodePrepareResources", "err", err)
		return status, nil
	}
	logger.Log(ctx, logging.LevelTrace, "Successfully invoked NodePrepareResources")

	status.Status = grpc_health_v1.HealthCheckResponse_SERVING
	return status, nil
}
