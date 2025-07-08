/*
Copyright 2016 The Kubernetes Authors.

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

// Package app implements a server that runs a set of active
// components.  This includes replication controllers, service endpoints and
// nodes.
package app

import (
	"context"
	"fmt"

	"k8s.io/kubernetes/cmd/kube-controller-manager/names"
	endpointslicecontroller "k8s.io/kubernetes/pkg/controller/endpointslice"
	endpointslicemirroringcontroller "k8s.io/kubernetes/pkg/controller/endpointslicemirroring"
)

func newEndpointSliceControllerDescriptor() *ControllerDescriptor {
	return &ControllerDescriptor{
		name:     names.EndpointSliceController,
		aliases:  []string{"endpointslice"},
		initFunc: newEndpointSliceController,
	}
}

func newEndpointSliceController(ctx context.Context, controllerContext ControllerContext, controllerName string) (Controller, error) {
	client, err := controllerContext.ClientBuilder.Client("endpointslice-controller")
	if err != nil {
		return nil, fmt.Errorf("failed to create a client: %w", err)
	}

	esc := endpointslicecontroller.NewController(
		ctx,
		controllerContext.InformerFactory.Core().V1().Pods(),
		controllerContext.InformerFactory.Core().V1().Services(),
		controllerContext.InformerFactory.Core().V1().Nodes(),
		controllerContext.InformerFactory.Discovery().V1().EndpointSlices(),
		controllerContext.ComponentConfig.EndpointSliceController.MaxEndpointsPerSlice,
		client,
		controllerContext.ComponentConfig.EndpointSliceController.EndpointUpdatesBatchPeriod.Duration,
	)
	return newNamedRunnableFunc(func(ctx context.Context) error {
		esc.Run(ctx, int(controllerContext.ComponentConfig.EndpointSliceController.ConcurrentServiceEndpointSyncs))
		return nil
	}, controllerName), nil
}

func newEndpointSliceMirroringControllerDescriptor() *ControllerDescriptor {
	return &ControllerDescriptor{
		name:     names.EndpointSliceMirroringController,
		aliases:  []string{"endpointslicemirroring"},
		initFunc: newEndpointSliceMirroringController,
	}
}

func newEndpointSliceMirroringController(ctx context.Context, controllerContext ControllerContext, controllerName string) (Controller, error) {
	client, err := controllerContext.ClientBuilder.Client("endpointslicemirroring-controller")
	if err != nil {
		return nil, fmt.Errorf("failed to create a client: %w", err)
	}

	esmc := endpointslicemirroringcontroller.NewController(
		ctx,
		controllerContext.InformerFactory.Core().V1().Endpoints(),
		controllerContext.InformerFactory.Discovery().V1().EndpointSlices(),
		controllerContext.InformerFactory.Core().V1().Services(),
		controllerContext.ComponentConfig.EndpointSliceMirroringController.MirroringMaxEndpointsPerSubset,
		client,
		controllerContext.ComponentConfig.EndpointSliceMirroringController.MirroringEndpointUpdatesBatchPeriod.Duration,
	)
	return newNamedRunnableFunc(func(ctx context.Context) error {
		esmc.Run(ctx, int(controllerContext.ComponentConfig.EndpointSliceMirroringController.MirroringConcurrentServiceEndpointSyncs))
		return nil
	}, controllerName), nil
}
