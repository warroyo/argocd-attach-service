package main

import (
	"context"
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

// patchFinalizer adds or removes the finalizer using a JSON Merge Patch.
// It uses the actual dynamic.Interface to perform the API call.
func patchFinalizer(ctx context.Context, client dynamic.Interface, gvr schema.GroupVersionResource, obj *unstructured.Unstructured, finalizerName string, finalizers []string) error {

	// A JSON Merge Patch for metadata.finalizers requires the *entire* list to be sent,
	// as it replaces the array.
	patchPayload := map[string]interface{}{
		"metadata": map[string]interface{}{
			"finalizers": finalizers,
		},
	}

	patchData, err := json.Marshal(patchPayload)
	if err != nil {
		return fmt.Errorf("failed to marshal patch payload: %w", err)
	}

	// Dynamic Client Patch call using JSON Merge Patch type
	_, err = client.Resource(gvr).Namespace(obj.GetNamespace()).Patch(
		ctx,
		obj.GetName(),
		types.MergePatchType, // Use MergePatchType for simplicity with Unstructured objects
		patchData,
		metav1.PatchOptions{},
	)
	if err != nil {
		// This is a real API call now, handle the error as a patch failure.
		return fmt.Errorf("failed to patch resource %s/%s with finalizer %s: %w", obj.GetName(), obj.GetNamespace(), finalizerName, err)
	}

	return nil
}

// containsFinalizer checks if the given finalizerName is present in the object's finalizers list.
func containsFinalizer(obj *unstructured.Unstructured, finalizerName string) bool {
	for _, f := range obj.GetFinalizers() {
		if f == finalizerName {
			return true
		}
	}
	return false
}

// removeString removes a single string from a slice of strings.
func removeString(slice []string, s string) (result []string) {
	for _, item := range slice {
		if item == s {
			continue
		}
		result = append(result, item)
	}
	return
}
