package main

import (
	"context"
	"errors"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health/grpc_health_v1"
	drapb "k8s.io/kubelet/pkg/apis/dra/v1"
	registerapi "k8s.io/kubelet/pkg/apis/pluginregistration/v1"
)

type healthyRegistrationClient struct{ registerapi.RegistrationClient }

func (healthyRegistrationClient) GetInfo(context.Context, *registerapi.InfoRequest, ...grpc.CallOption) (*registerapi.PluginInfo, error) {
	return &registerapi.PluginInfo{}, nil
}

type healthyDRAClient struct{ drapb.DRAPluginClient }

func (healthyDRAClient) NodePrepareResources(context.Context, *drapb.NodePrepareResourcesRequest, ...grpc.CallOption) (*drapb.NodePrepareResourcesResponse, error) {
	return &drapb.NodePrepareResourcesResponse{}, nil
}

func TestReadinessRequiresKubeletAcknowledgement(t *testing.T) {
	d := &driver{}
	h := &healthcheck{registered: d.registered.Load, regClient: healthyRegistrationClient{}, draClient: healthyDRAClient{}}
	check := func(service string, want grpc_health_v1.HealthCheckResponse_ServingStatus) {
		t.Helper()
		got, err := h.Check(context.Background(), &grpc_health_v1.HealthCheckRequest{Service: service})
		if err != nil || got.Status != want {
			t.Fatalf("%s=%v err=%v, want %v", service, got, err, want)
		}
	}
	check("liveness", grpc_health_v1.HealthCheckResponse_SERVING)
	check("readiness", grpc_health_v1.HealthCheckResponse_NOT_SERVING)
	ack := func(registered bool, handlerErr error) {
		_, _ = d.observeRegistration(context.Background(), &registerapi.RegistrationStatus{PluginRegistered: registered}, nil, func(context.Context, any) (any, error) { return &registerapi.RegistrationStatusResponse{}, handlerErr })
	}
	ack(true, nil)
	check("readiness", grpc_health_v1.HealthCheckResponse_SERVING)
	ack(false, errors.New("registration rejected"))
	check("readiness", grpc_health_v1.HealthCheckResponse_NOT_SERVING)
	ack(true, errors.New("ack failed"))
	check("readiness", grpc_health_v1.HealthCheckResponse_NOT_SERVING)
}

func TestRegistrationReadinessIsSafeAcrossConcurrentRPCs(t *testing.T) {
	d := &driver{}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_, _ = d.observeRegistration(context.Background(), &registerapi.RegistrationStatus{PluginRegistered: j%2 == 0}, nil, func(context.Context, any) (any, error) { return nil, nil })
				_ = d.registered.Load()
			}
		}()
	}
	wg.Wait()
}
