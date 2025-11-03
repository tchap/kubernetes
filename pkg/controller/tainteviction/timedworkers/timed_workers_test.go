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

package timedworkers

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"k8s.io/klog/v2/ktesting"
)

func TestExecute(t *testing.T) {
	testVal := int32(0)

	synctest.Test(t, func(t *testing.T) {
		var wg sync.WaitGroup
		defer wg.Wait()

		_, ctx := ktesting.NewTestContext(t)
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		var workWG sync.WaitGroup
		workWG.Add(5)
		queue := CreateWorkerQueue(func(ctx context.Context, fireAt time.Time, args *WorkArgs) error {
			atomic.AddInt32(&testVal, 1)
			workWG.Done()
			return nil
		})

		wg.Go(func() {
			queue.Run(ctx, 1)
		})

		now := time.Now()

		queue.AddWork(ctx, NewWorkArgs("1", "1"), now, now)
		queue.AddWork(ctx, NewWorkArgs("2", "2"), now, now)
		queue.AddWork(ctx, NewWorkArgs("3", "3"), now, now)
		queue.AddWork(ctx, NewWorkArgs("4", "4"), now, now)
		queue.AddWork(ctx, NewWorkArgs("5", "5"), now, now)

		workWG.Wait()
	})

	lastVal := atomic.LoadInt32(&testVal)
	if lastVal != 5 {
		t.Errorf("Expected testVal = 5, got %v", lastVal)
	}
}

func TestExecuteDelayed(t *testing.T) {
	testVal := int32(0)

	synctest.Test(t, func(t *testing.T) {
		var wg sync.WaitGroup
		defer wg.Wait()

		_, ctx := ktesting.NewTestContext(t)
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		var workWG sync.WaitGroup
		workWG.Add(5)
		queue := CreateWorkerQueue(func(ctx context.Context, fireAt time.Time, args *WorkArgs) error {
			atomic.AddInt32(&testVal, 1)
			workWG.Done()
			return nil
		})

		wg.Go(func() {
			queue.Run(ctx, 1)
		})

		now := time.Now()
		then := now.Add(10 * time.Second)

		queue.AddWork(ctx, NewWorkArgs("1", "1"), now, then)
		queue.AddWork(ctx, NewWorkArgs("2", "2"), now, then)
		queue.AddWork(ctx, NewWorkArgs("3", "3"), now, then)
		queue.AddWork(ctx, NewWorkArgs("4", "4"), now, then)
		queue.AddWork(ctx, NewWorkArgs("5", "5"), now, then)
		queue.AddWork(ctx, NewWorkArgs("1", "1"), now, then)
		queue.AddWork(ctx, NewWorkArgs("2", "2"), now, then)
		queue.AddWork(ctx, NewWorkArgs("3", "3"), now, then)
		queue.AddWork(ctx, NewWorkArgs("4", "4"), now, then)
		queue.AddWork(ctx, NewWorkArgs("5", "5"), now, then)

		time.Sleep(11 * time.Second)
		workWG.Wait()
	})

	lastVal := atomic.LoadInt32(&testVal)
	if lastVal != 5 {
		t.Errorf("Expected testVal = 5, got %v", lastVal)
	}
}

func TestCancel(t *testing.T) {
	testVal := int32(0)

	synctest.Test(t, func(t *testing.T) {
		var wg sync.WaitGroup
		defer wg.Wait()

		logger, ctx := ktesting.NewTestContext(t)
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		var workWG sync.WaitGroup
		workWG.Add(3)
		queue := CreateWorkerQueue(func(ctx context.Context, fireAt time.Time, args *WorkArgs) error {
			atomic.AddInt32(&testVal, 1)
			workWG.Done()
			return nil
		})

		wg.Go(func() {
			queue.Run(ctx, 1)
		})

		now := time.Now()
		then := now.Add(10 * time.Second)

		queue.AddWork(ctx, NewWorkArgs("1", "1"), now, then)
		queue.AddWork(ctx, NewWorkArgs("2", "2"), now, then)
		queue.AddWork(ctx, NewWorkArgs("3", "3"), now, then)
		queue.AddWork(ctx, NewWorkArgs("4", "4"), now, then)
		queue.AddWork(ctx, NewWorkArgs("5", "5"), now, then)
		queue.AddWork(ctx, NewWorkArgs("1", "1"), now, then)
		queue.AddWork(ctx, NewWorkArgs("2", "2"), now, then)
		queue.AddWork(ctx, NewWorkArgs("3", "3"), now, then)
		queue.AddWork(ctx, NewWorkArgs("4", "4"), now, then)
		queue.AddWork(ctx, NewWorkArgs("5", "5"), now, then)
		queue.CancelWork(logger, NewWorkArgs("2", "2").KeyFromWorkArgs())
		queue.CancelWork(logger, NewWorkArgs("4", "4").KeyFromWorkArgs())

		time.Sleep(11 * time.Second)
		workWG.Wait()
	})

	lastVal := atomic.LoadInt32(&testVal)
	if lastVal != 3 {
		t.Errorf("Expected testVal = 3, got %v", lastVal)
	}
}

func TestCancelAndRead(t *testing.T) {
	testVal := int32(0)

	synctest.Test(t, func(t *testing.T) {
		var wg sync.WaitGroup
		defer wg.Wait()

		logger, ctx := ktesting.NewTestContext(t)
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		var workWG sync.WaitGroup
		workWG.Add(4)
		queue := CreateWorkerQueue(func(ctx context.Context, fireAt time.Time, args *WorkArgs) error {
			atomic.AddInt32(&testVal, 1)
			workWG.Done()
			return nil
		})

		wg.Go(func() {
			queue.Run(ctx, 1)
		})

		now := time.Now()
		then := now.Add(10 * time.Second)

		queue.AddWork(ctx, NewWorkArgs("1", "1"), now, then)
		queue.AddWork(ctx, NewWorkArgs("2", "2"), now, then)
		queue.AddWork(ctx, NewWorkArgs("3", "3"), now, then)
		queue.AddWork(ctx, NewWorkArgs("4", "4"), now, then)
		queue.AddWork(ctx, NewWorkArgs("5", "5"), now, then)
		queue.AddWork(ctx, NewWorkArgs("1", "1"), now, then)
		queue.AddWork(ctx, NewWorkArgs("2", "2"), now, then)
		queue.AddWork(ctx, NewWorkArgs("3", "3"), now, then)
		queue.AddWork(ctx, NewWorkArgs("4", "4"), now, then)
		queue.AddWork(ctx, NewWorkArgs("5", "5"), now, then)
		queue.CancelWork(logger, NewWorkArgs("2", "2").KeyFromWorkArgs())
		queue.CancelWork(logger, NewWorkArgs("4", "4").KeyFromWorkArgs())
		queue.AddWork(ctx, NewWorkArgs("2", "2"), now, then)

		time.Sleep(11 * time.Second)
		workWG.Wait()
	})

	lastVal := atomic.LoadInt32(&testVal)
	if lastVal != 4 {
		t.Errorf("Expected testVal = 4, got %v", lastVal)
	}
}
