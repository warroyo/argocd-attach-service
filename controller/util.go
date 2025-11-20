package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

const StatusFieldManager = "status-controller"

func convertObj(obj any) (ArgoCluster, error) {
	argoCluster := ArgoCluster{}
	unstructuredObj, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return argoCluster, fmt.Errorf("unable to convert to unstuctured object")
	}
	err := runtime.DefaultUnstructuredConverter.FromUnstructured(unstructuredObj.Object, &argoCluster)
	if err != nil {
		return argoCluster, err
	}
	return argoCluster, nil
}

func convertNs(obj any) (ArgoNamespace, error) {
	argoNs := ArgoNamespace{}
	unstructuredObj, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return argoNs, fmt.Errorf("unable to convert to unstuctured object")
	}
	err := runtime.DefaultUnstructuredConverter.FromUnstructured(unstructuredObj.Object, &argoNs)
	if err != nil {
		return argoNs, err
	}
	return argoNs, nil
}

func toUnstructured(obj interface{}) (*unstructured.Unstructured, error) {
	if runtimeObj, ok := obj.(runtime.Object); ok {
		return runtimeObj.(*unstructured.Unstructured), nil
	}
	return nil, fmt.Errorf("object is not an unstructured object")
}

func patchStatus(ctx context.Context, client dynamic.Interface, gvr schema.GroupVersionResource, obj *unstructured.Unstructured, statusData map[string]interface{}) error {

	statusObj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": obj.GetAPIVersion(),
			"kind":       obj.GetKind(),
			"metadata": map[string]interface{}{
				"name":      obj.GetName(),
				"namespace": obj.GetNamespace(),
			},
			"status": statusData,
		},
	}

	const maxRetries = 5
	var lastError error

	for i := 0; i < maxRetries; i++ {

		_, err := client.Resource(gvr).Namespace(obj.GetNamespace()).ApplyStatus(
			ctx,
			statusObj.GetName(),
			statusObj,
			metav1.ApplyOptions{FieldManager: StatusFieldManager, Force: true},
		)

		if err == nil {
			return nil
		}

		if apierrors.IsNotFound(err) {
			fmt.Printf("Warning: Skipped status patch for deleted resource %s/%s\n", obj.GetNamespace(), obj.GetName())
			return nil
		}

		if apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) {
			lastError = fmt.Errorf("status apply conflict encountered: %w", err)

			fmt.Printf("Status apply conflict (%d/%d). Retrying...\n", i+1, maxRetries)
			time.Sleep(100 * time.Millisecond)
			continue
		}

		return fmt.Errorf("failed to apply status for %s/%s after %d tries: %w", obj.GetName(), obj.GetNamespace(), i+1, err)
	}

	return fmt.Errorf("failed to apply status after %d attempts: %w", maxRetries, lastError)
}

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
