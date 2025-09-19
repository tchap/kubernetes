/*
Copyright 2025 The Kubernetes Authors.

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

package controller

import (
	"context"

	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/component-base/featuregate"
	"k8s.io/klog/v2"
)

// This file contains types and functions for wrapping controller implementations from downstream packages.
// Every controller wrapper implements the Controller interface,
// which is then associated with a ControllerDescriptor, which holds additional static metadata
// needed so that the manager can manage Controllers properly.

// Controller defines the base interface that all controller wrappers must implement.
type Controller interface {
	// Name returns the controller's canonical name.
	Name() string

	// Run runs the controller loop.
	// When there is anything to be done, it blocks until the context is cancelled.
	// Run must ensure all goroutines are terminated before returning.
	Run(context.Context)
}

// ControllerConstructor is a constructor for a controller.
// A nil Controller returned means that the associated controller is disabled.
type ControllerConstructor func(ctx context.Context, controllerContext app.ControllerContext, controllerName string) (Controller, error)

type ControllerDescriptor struct {
	name                      string
	constructor               ControllerConstructor
	requiredFeatureGates      []featuregate.Feature
	aliases                   []string
	isDisabledByDefault       bool
	isCloudProviderController bool
	requiresSpecialHandling   bool
}

func (r *ControllerDescriptor) Name() string {
	return r.name
}

func (r *ControllerDescriptor) GetControllerConstructor() ControllerConstructor {
	return r.constructor
}

func (r *ControllerDescriptor) GetRequiredFeatureGates() []featuregate.Feature {
	return append([]featuregate.Feature(nil), r.requiredFeatureGates...)
}

// GetAliases returns aliases to ensure backwards compatibility and should never be removed!
// Only addition of new aliases is allowed, and only when a canonical name is changed (please see CHANGE POLICY of controller names)
func (r *ControllerDescriptor) GetAliases() []string {
	return append([]string(nil), r.aliases...)
}

func (r *ControllerDescriptor) IsDisabledByDefault() bool {
	return r.isDisabledByDefault
}

func (r *ControllerDescriptor) IsCloudProviderController() bool {
	return r.isCloudProviderController
}

// RequiresSpecialHandling should return true only in a special non-generic controllers like ServiceAccountTokenController
func (r *ControllerDescriptor) RequiresSpecialHandling() bool {
	return r.requiresSpecialHandling
}

// BuildController creates a controller based on the given descriptor.
// The associated controller's constructor is called at the end, so the same contract applies for the return values here.
func (r *ControllerDescriptor) BuildController(ctx context.Context, controllerCtx app.ControllerContext) (Controller, error) {
	logger := klog.FromContext(ctx)
	controllerName := r.Name()

	for _, featureGate := range r.GetRequiredFeatureGates() {
		if !utilfeature.DefaultFeatureGate.Enabled(featureGate) {
			logger.Info("Controller is disabled by a feature gate",
				"controller", controllerName,
				"requiredFeatureGates", r.GetRequiredFeatureGates())
			return nil, nil
		}
	}

	if r.IsCloudProviderController() {
		logger.Info("Skipping a cloud provider controller", "controller", controllerName)
		return nil, nil
	}

	ctx = klog.NewContext(ctx, klog.LoggerWithName(logger, controllerName))
	return r.GetControllerConstructor()(ctx, controllerCtx, controllerName)
}
