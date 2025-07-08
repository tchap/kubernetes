/*
Copyright 2024 The Kubernetes Authors.

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

package app

import (
	"context"
	"fmt"
	"golang.org/x/sync/errgroup"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/metadata"
	"k8s.io/kubernetes/cmd/kube-controller-manager/names"
	"k8s.io/kubernetes/pkg/features"

	utilfeature "k8s.io/apiserver/pkg/util/feature"
	clientgofeaturegate "k8s.io/client-go/features"
	svm "k8s.io/kubernetes/pkg/controller/storageversionmigrator"
)

func newStorageVersionMigratorControllerDescriptor() *ControllerDescriptor {
	return &ControllerDescriptor{
		name:     names.StorageVersionMigratorController,
		aliases:  []string{"svm"},
		initFunc: newSVMController,
	}
}

func newSVMController(ctx context.Context, controllerContext ControllerContext, controllerName string) (Controller, error) {
	if !utilfeature.DefaultFeatureGate.Enabled(features.StorageVersionMigrator) ||
		!clientgofeaturegate.FeatureGates().Enabled(clientgofeaturegate.InformerResourceVersion) {
		return nil, nil
	}

	if !controllerContext.ComponentConfig.GarbageCollectorController.EnableGarbageCollector {
		return nil, fmt.Errorf("storage version migrator requires garbage collector")
	}

	// svm controller can make a lot of requests during migration, keep it fast
	config, err := controllerContext.ClientBuilder.Config(controllerName)
	if err != nil {
		return nil, fmt.Errorf("failed to create a client config for %s: %v", controllerName, err)
	}

	config.QPS *= 20
	config.Burst *= 100

	client, err := controllerContext.ClientBuilder.Client(controllerName)
	if err != nil {
		return nil, fmt.Errorf("failed to create a client for %s: %v", controllerName, err)
	}

	informer := controllerContext.InformerFactory.Storagemigration().V1alpha1().StorageVersionMigrations()

	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return nil, err
	}

	metaClient, err := metadata.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create the metadata client for %s: %w", controllerName, err)
	}

	svmc := svm.NewSVMController(
		ctx,
		client,
		dynamicClient,
		informer,
		controllerName,
		controllerContext.RESTMapper,
		controllerContext.GraphBuilder,
	)
	return newNamedRunnableFunc(func(ctx context.Context) error {
		eg, ctx := errgroup.WithContext(ctx)
		eg.Go(func() error {
			svm.NewResourceVersionController(
				ctx,
				client,
				discoveryClient,
				metaClient,
				informer,
				controllerContext.RESTMapper,
			).Run(ctx)
			return nil
		})

		eg.Go(func() error {
			svmc.Run(ctx)
			return nil
		})
		return eg.Wait()
	}, controllerName), nil
}
