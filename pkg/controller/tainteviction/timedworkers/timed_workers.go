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
	"fmt"
	"sync"
	"time"

	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

type WorkItem[T fmt.Stringer] struct {
	Args      T
	CreatedAt time.Time
	FireAt    time.Time
}

func (item WorkItem[T]) Progressing() bool {
	return !item.CreatedAt.Before(item.FireAt)
}

// TimedWorkerQueue keeps a set workers that are waiting for execution.
type TimedWorkerQueue[T fmt.Stringer] struct {
	sync.Mutex
	workFunc func(ctx context.Context, fireAt time.Time, args T) error

	// args maps args key to the associated args.
	items map[string]WorkItem[T]
	queue workqueue.TypedDelayingInterface[string]
}

// CreateWorkerQueue creates a new TimedWorkerQueue for workers that will execute
// given function `f`.
func CreateWorkerQueue[T fmt.Stringer](f func(ctx context.Context, fireAt time.Time, args T) error) *TimedWorkerQueue[T] {
	return &TimedWorkerQueue[T]{
		items:    make(map[string]WorkItem[T]),
		workFunc: f,
		queue:    workqueue.NewTypedDelayingQueue[string](),
	}
}

func (q *TimedWorkerQueue[T]) Run(ctx context.Context, numWorkers int) {
	var wg sync.WaitGroup
	for i := 0; i < numWorkers; i++ {
		wg.Go(func() {
			q.worker(ctx)
		})
	}

	<-ctx.Done()
	q.queue.ShutDown()
	wg.Wait()
}

func (q *TimedWorkerQueue[T]) worker(ctx context.Context) {
	for q.processNextItem(ctx) {
	}
}

func (q *TimedWorkerQueue[T]) processNextItem(ctx context.Context) bool {
	key, quit := q.queue.Get()
	if quit {
		return false
	}
	defer q.queue.Done(key)

	q.Lock()
	item, ok := q.items[key]
	q.Unlock()
	if !ok {
		return true
	}

	logger := klog.FromContext(ctx)
	logger.V(4).Info("Firing worker", "item", key, "fireTime", item.FireAt)
	defer logger.V(4).Info("TimedWorker finished, removing", "item", key)

	if err := q.workFunc(ctx, item.FireAt, item.Args); err != nil {
		klog.FromContext(ctx).Error(err, "TimedWorker failed", "item", key)
	}

	q.Lock()
	q.workDoneUnsafe(key)
	q.Unlock()
	return true
}

// AddWork adds a work to the WorkerQueue which will be executed not earlier than `fireAt`.
// If replace is false, an existing work item will not get replaced, otherwise it
// gets canceled and the new one is added instead.
func (q *TimedWorkerQueue[T]) AddWork(ctx context.Context, args T, createdAt time.Time, fireAt time.Time) {
	key := args.String()
	logger := klog.FromContext(ctx)

	q.Lock()
	defer q.Unlock()
	if _, exists := q.items[key]; exists {
		logger.V(4).Info("Trying to add already existing work, skipping", "item", key, "createTime", createdAt, "fireTime", fireAt)
		return
	}
	logger.V(4).Info("Adding TimedWorkerQueue item and to be fired at firedTime", "item", key, "createTime", createdAt, "fireTime", fireAt)
	q.addWorkUnsafe(key, args, createdAt, fireAt)
}

// UpdateWork adds or replaces a work item such that it will be executed not earlier than `fireAt`.
// This is a cheap no-op when the old and new fireAt are the same.
func (q *TimedWorkerQueue[T]) UpdateWork(ctx context.Context, args T, createdAt time.Time, fireAt time.Time) {
	key := args.String()
	logger := klog.FromContext(ctx)

	q.Lock()
	defer q.Unlock()
	if item, exists := q.items[key]; exists {
		if item.Progressing() {
			logger.V(4).Info("Keeping existing work, already in progress", "item", key)
			return
		}
		if item.FireAt.Compare(fireAt) == 0 {
			logger.V(4).Info("Keeping existing work, same time", "item", key, "createTime", item.CreatedAt, "fireTime", item.FireAt)
			return
		}
		logger.V(4).Info("Replacing existing work", "item", key, "createTime", item.CreatedAt, "fireTime", item.FireAt)
		q.workDoneUnsafe(key)
	}
	logger.V(4).Info("Adding TimedWorkerQueue item and to be fired at firedTime", "item", key, "createTime", createdAt, "fireTime", fireAt)
	q.addWorkUnsafe(key, args, createdAt, fireAt)
}

// CancelWork removes scheduled function execution from the queue. Returns true if work was cancelled.
// The key must be the same as the one returned by WorkArgs.KeyFromWorkArgs, i.e.
// the result of NamespacedName.String.
func (q *TimedWorkerQueue[T]) CancelWork(logger klog.Logger, key string) bool {
	q.Lock()
	defer q.Unlock()
	item, found := q.items[key]
	result := false
	if found {
		logger.V(4).Info("Cancelling TimedWorkerQueue item", "item", key, "time", time.Now())
		if item.Progressing() {
			result = true
		}
		q.workDoneUnsafe(key)
	}
	return result
}

// GetWorkerUnsafe returns a TimedWorker corresponding to the given key.
// Unsafe method - workers have attached goroutines which can fire after this function is called.
func (q *TimedWorkerQueue[T]) GetWorkerUnsafe(key string) (WorkItem[T], bool) {
	q.Lock()
	defer q.Unlock()
	item, ok := q.items[key]
	return item, ok
}

func (q *TimedWorkerQueue[T]) addWorkUnsafe(key string, args T, createdAt time.Time, fireAt time.Time) {
	q.items[key] = WorkItem[T]{
		Args:      args,
		CreatedAt: createdAt,
		FireAt:    fireAt,
	}
	q.queue.AddAfter(key, fireAt.Sub(createdAt))
}

func (q *TimedWorkerQueue[T]) workDoneUnsafe(key string) {
	q.queue.Done(key)
	delete(q.items, key)
}
