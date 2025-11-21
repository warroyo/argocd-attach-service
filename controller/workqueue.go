package main

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
)

func (c *Controller) processNextWorkItem() bool {
	// Get() blocks until an item is available.
	obj, shutdown := c.Queue.Get()

	if shutdown {
		return false
	}

	// Tell the queue that we are done with processing this key, even if it failed.
	// This ensures that if we handle the item successfully, it will be forgotten.
	defer c.Queue.Done(obj)

	key, ok := obj.(string)
	if !ok {
		// Item in queue was not a string. Log error and forget item.
		c.Queue.Forget(obj)
		fmt.Printf("Expected string in workqueue but got %#v\n", obj)
		return true
	}

	// Run the core reconcile logic.
	// We use the key (namespace/name) to re-fetch the resource in Reconcile.
	if err := c.reconcileByKey(key); err != nil {
		// If an error occurred during processing, use the ratelimiter to requeue the item.
		if c.Queue.NumRequeues(key) < 10 { // Limit retries to prevent runaway loops
			fmt.Printf("Failed to reconcile %s: %v. Retrying...\n", key, err)
			c.Queue.AddRateLimited(key)
			return true
		}

		// Max retries reached, log the final error and forget the item.
		c.Queue.Forget(obj)
		fmt.Printf("Max retries exceeded for %s: %v. Forgetting item.\n", key, err)
		return true
	}

	// If successful, forget the item's rate limit history.
	c.Queue.Forget(obj)
	return true
}

// reconcileByKey is a wrapper that fetches the object before calling the main Reconcile logic.
func (c *Controller) reconcileByKey(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return fmt.Errorf("invalid resource key: %s", key)
	}

	// We must get the latest object from the API to start the reconciliation,
	// instead of passing the potentially stale object from the informer handler.
	// This ensures Level-Triggering.
	u, err := c.client.Resource(c.gvr).Namespace(namespace).Get(context.TODO(), name, metav1.GetOptions{})

	if err != nil {
		if apierrors.IsNotFound(err) {
			// Resource is gone. This is expected. Stop processing.
			return nil
		}
		// Transient API error. Return error for retry.
		return fmt.Errorf("failed to fetch resource %s: %w", key, err)
	}

	return c.Reconcile(u)
}

// runWorker is a single goroutine that continually processes items from the workqueue.
func (c *Controller) runWorker(ctx context.Context) {
	// Loop forever until the context is cancelled.
	for c.processNextWorkItem() {
	}
}

// Run starts the controller's worker pool.
func (c *Controller) Run(ctx context.Context, workers int) {
	defer c.Queue.ShutDown()

	// Start the consumer workers
	for i := 0; i < workers; i++ {
		go c.runWorker(ctx)
	}

	// Wait until the context is cancelled.
	<-ctx.Done()
}
