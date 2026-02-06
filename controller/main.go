package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"k8s.io/client-go/util/workqueue"
)

const numWorkers = 2

var resync *int64 = new(int64)
var secretGVR = schema.GroupVersionResource{
	Group:    "",
	Version:  "v1",
	Resource: "secrets",
}

var saGVR = schema.GroupVersionResource{
	Group:    "",
	Version:  "v1",
	Resource: "serviceaccounts",
}

var rbGVR = schema.GroupVersionResource{
	Group:    "rbac.authorization.k8s.io",
	Version:  "v1",
	Resource: "rolebindings",
}

type StringSlice []string

func (s *StringSlice) String() string {
	return fmt.Sprintf("%v", *s)
}

func (s *StringSlice) Set(value string) error {
	*s = append(*s, value)
	return nil
}

type StatusUpdater func(*unstructured.Unstructured, bool, error) map[string]interface{}

type Controller struct {
	client           *dynamic.DynamicClient
	gvr              schema.GroupVersionResource
	finalizerName    string
	provisionFunc    func(*dynamic.DynamicClient, interface{}, []string) error // Function to run during normal operation
	cleanupFunc      func(*dynamic.DynamicClient, interface{}) error           // Function to run during cleanup
	updateStatusFunc StatusUpdater
	namespaces       []string

	Queue    workqueue.RateLimitingInterface
	Informer cache.SharedIndexInformer
}

type ArgoNamespace struct {
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              *ArgoNamespaceSpec `json:"spec,omitempty"`
}

type ArgoCluster struct {
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              *ArgoClusterSpec `json:"spec,omitempty"`
}

type ArgoNamespaceSpec struct {
	ClusterName    string            `json:"clusterName"`
	ArgoNamespace  string            `json:"argoNamespace"`
	ClusterLabels  map[string]string `json:"clusterLabels"`
	Project        string            `json:"project"`
	ServiceAccount string            `json:"serviceAccount"`
}
type ArgoClusterSpec struct {
	ClusterName   string            `json:"clusterName"`
	ArgoNamespace string            `json:"argoNamespace"`
	ClusterLabels map[string]string `json:"clusterLabels"`
	Project       string            `json:"project"`
}

type ArgoConfig struct {
	TLSClientConfig *TLSClientConfig `json:"tlsClientConfig,omitempty"`
	BearerToken     string           `json:"bearerToken,omitempty"`
}

type TLSClientConfig struct {
	CAData     string `json:"caData,omitempty"`     // base64-encoded PEM
	CertData   string `json:"certData,omitempty"`   // base64-encoded PEM
	KeyData    string `json:"keyData,omitempty"`    // base64-encoded PEM
	Insecure   bool   `json:"insecure,omitempty"`   // skip TLS verification
	ServerName string `json:"serverName,omitempty"` // optional SNI
}

func getSecret(client *dynamic.DynamicClient, namespace string, name string) (*unstructured.Unstructured, error) {

	kubeconfigSecret, err := client.Resource(secretGVR).Namespace(namespace).Get(context.TODO(), fmt.Sprintf("%s-kubeconfig", name), metav1.GetOptions{})
	if err != nil {
		log.Printf("unable to retrive kubeconfig secret %v", err)
		return nil, err
	}
	return kubeconfigSecret, nil

}

func updateGenericStatus(u *unstructured.Unstructured, success bool, reconcileErr error) map[string]interface{} {
	if reconcileErr != nil {
		return map[string]interface{}{
			"state":       "Failed",
			"message":     fmt.Sprintf("Reconciliation failed: %s", reconcileErr.Error()),
			"ready":       false,
			"lastUpdated": metav1.Now(),
		}
	}
	if success {
		return map[string]interface{}{
			"state":       "Ready",
			"message":     "Resource provisioned successfully.",
			"ready":       true,
			"lastUpdated": metav1.Now(),
		}
	}
	// Default/Pending status when finalizer is added but provisioning hasn't run yet
	return map[string]interface{}{
		"state":       "Pending",
		"message":     "Initializing or waiting for finalizer to be confirmed.",
		"ready":       false,
		"lastUpdated": metav1.Now(),
	}
}

func applyArgoNamespace(client *dynamic.DynamicClient, obj interface{}, namespaces []string) error {
	argoNs, err := convertNs(obj)
	if err != nil {
		log.Printf("unable to convert object to structured argocd namespace: %v", err)
		return fmt.Errorf("conversion error: %w", err)
	}
	clusterName := fmt.Sprintf("supervisor-ns-%s", argoNs.Namespace)
	argoNs.Spec.ClusterName = clusterName
	project := argoNs.Spec.Project
	//do validation[TODO]

	//create the necessary svc account etc.
	token := ""
	if argoNs.Spec.ServiceAccount == "" {
		token, err = createArgoSvcAccount(client, &argoNs)
		if err != nil {
			log.Printf("unable to create svc account for %s: %v", argoNs.Name, err)
			return fmt.Errorf("unable to create svc account for %s: %v", argoNs.Name, err)
		}
	} else {
		//get existing service account token

		token, err = getSAToken(client, argoNs.Namespace, argoNs.Spec.ServiceAccount)
		if err != nil {
			log.Printf("unable to get svc account token for %s: %v", argoNs.Spec.ServiceAccount, err)
			return fmt.Errorf("unable to get svc account token for %s: %v", argoNs.Spec.ServiceAccount, err)
		}
	}

	//create a secret in the correct namespace
	argoConfig := &ArgoConfig{
		BearerToken: token,
		TLSClientConfig: &TLSClientConfig{
			Insecure: true,
		},
	}

	jsonConfig, err := json.Marshal(argoConfig)
	if err != nil {
		log.Printf("unable to encoded argo config: %v", err)
		return fmt.Errorf("unable to encoded argo config: %v", err)
	}

	secretData := map[string]string{
		"name":       clusterName,
		"server":     "https://kubernetes.default.svc.cluster.local:443/?context=" + argoNs.Namespace,
		"project":    project,
		"config":     string(jsonConfig),
		"namespaces": string(argoNs.Namespace),
	}

	cluster := &ArgoCluster{
		ObjectMeta: argoNs.ObjectMeta,
		Spec: &ArgoClusterSpec{
			ClusterName:   argoNs.Spec.ClusterName,
			ArgoNamespace: argoNs.Spec.ArgoNamespace,
			ClusterLabels: argoNs.Spec.ClusterLabels,
			Project:       argoNs.Spec.Project,
		},
	}
	err = applySecret(client, cluster, secretData)
	if err != nil {
		log.Printf("unable to create or update argo cluster secret %v", err)
		return fmt.Errorf("unable to create or update argo cluster secret %v", err)
	}
	secretName := fmt.Sprintf("%s-argo-cluster", clusterName)
	log.Printf("succesfully created or update argo cluster secret %s", secretName)

	return nil
}

func applyArgoCluster(client *dynamic.DynamicClient, obj interface{}, namespaces []string) error {
	argoCluster, err := convertObj(obj)
	if err != nil {
		log.Printf("unable to convert object to structured argocd cluster: %v", err)
		return fmt.Errorf("unable to convert object to structured argocd cluster: %v", err)
	}

	namespace := argoCluster.ObjectMeta.Namespace
	clusterName := argoCluster.Spec.ClusterName
	project := argoCluster.Spec.Project
	argoNamespace := argoCluster.Spec.ArgoNamespace

	if slices.Contains(namespaces, argoNamespace) {
		log.Printf("argoNamespace is in the list of blocked namespaces, not creating secret: %v", namespaces)
		return fmt.Errorf("argoNamespace is in the list of blocked namespaces, not creating secret: %v", namespaces)
	}

	kubeconfigUns, err := getSecret(client, namespace, clusterName)
	if err != nil {
		log.Printf("unable to retrieve kubeconfig secret: %v", err)
		return fmt.Errorf("unable to retrieve kubeconfig secret: %v", err)
	}

	kubeconfig, found, err := unstructured.NestedStringMap(kubeconfigUns.Object, "data")
	if err != nil || !found {
		log.Printf("cannot get secret data: %v", err)
		return fmt.Errorf("cannot get secret data: %v", err)
	}

	encodedSecret, ok := kubeconfig["value"]
	if !ok {
		log.Println("value does not exist in kubeconfig secret")
		return fmt.Errorf("value does not exist in kubeconfig secret")
	}

	decoded, err := base64.StdEncoding.DecodeString(encodedSecret)
	if err != nil {
		log.Printf("failed to decode value: %v", err)
		return fmt.Errorf("failed to decode value: %v", err)
	}

	config, err := clientcmd.Load(decoded)
	if err != nil {
		log.Printf("failed to read kubconfig data: %v", err)
		return fmt.Errorf("failed to read kubconfig data: %v", err)
	}
	argoConfig := &ArgoConfig{
		TLSClientConfig: &TLSClientConfig{
			CAData:   base64.StdEncoding.EncodeToString(config.Clusters[clusterName].CertificateAuthorityData),
			KeyData:  base64.StdEncoding.EncodeToString(config.AuthInfos[fmt.Sprintf("%s-admin", clusterName)].ClientKeyData),
			CertData: base64.StdEncoding.EncodeToString(config.AuthInfos[fmt.Sprintf("%s-admin", clusterName)].ClientCertificateData),
		},
	}

	jsonConfig, err := json.Marshal(argoConfig)
	if err != nil {
		log.Printf("unable to encoded argo config: %v", err)
	}
	secretData := map[string]string{
		"name":             clusterName,
		"server":           config.Clusters[clusterName].Server,
		"clusterresources": "true",
		"project":          project,
		"config":           string(jsonConfig),
	}

	err = applySecret(client, &argoCluster, secretData)
	if err != nil {
		log.Printf("unable to create or update argo cluster secret %v", err)
		return fmt.Errorf("unable to create or update argo cluster secret %v", err)
	}
	secretName := fmt.Sprintf("%s-argo-cluster", clusterName)
	log.Printf("succesfully created or update argo cluster secret %s", secretName)
	return nil

}

func applySecret(client *dynamic.DynamicClient, argoCluster *ArgoCluster, secretData map[string]string) error {
	labels := argoCluster.Spec.ClusterLabels
	clusterName := argoCluster.Spec.ClusterName
	argoNamespace := argoCluster.Spec.ArgoNamespace
	labels["argocd.argoproj.io/secret-type"] = "cluster"
	secretName := fmt.Sprintf("%s-argo-cluster", clusterName)

	secret := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Secret",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: argoNamespace,
			Labels:    labels,
		},
		StringData: secretData,
		Type:       corev1.SecretTypeOpaque,
	}
	secretU, err := runtime.DefaultUnstructuredConverter.ToUnstructured(secret)
	if err != nil {
		return fmt.Errorf("failed to convert secret to unstructured: %w", err)
	}
	secretUnstructured := &unstructured.Unstructured{Object: secretU}

	_, err = client.Resource(secretGVR).Namespace(argoNamespace).Apply(context.TODO(), secretUnstructured.GetName(), secretUnstructured, metav1.ApplyOptions{FieldManager: "argo-attach-controller", Force: true})
	if err != nil {
		return err
	}
	return nil
}

func deleteClusterCleanup(client *dynamic.DynamicClient, obj interface{}) error {
	argoCluster, err := convertObj(obj)
	if err != nil {
		log.Printf("unable to convert object to structured argocd cluster: %v", err)
		return err
	}

	clusterName := argoCluster.Spec.ClusterName
	secretName := fmt.Sprintf("%s-argo-cluster", clusterName)
	argoNamespace := argoCluster.Spec.ArgoNamespace
	err = deleteSecret(client, argoNamespace, secretName)
	if err != nil {
		log.Printf("unable to delete cluster secret: %v", err)
		return err
	}
	return nil
}

func deleteNamespaceCleanup(client *dynamic.DynamicClient, obj interface{}) error {
	argoNs, err := convertNs(obj)
	if err != nil {
		log.Printf("unable to convert object to structured argocd namespace: %v", err)
		return err
	}
	namespace := argoNs.Namespace
	clusterName := fmt.Sprintf("supervisor-ns-%s", argoNs.Namespace)
	secretName := fmt.Sprintf("%s-argo-cluster", clusterName)
	argoNamespace := argoNs.Spec.ArgoNamespace
	saName := "argo-attach-sa"
	if argoNs.Spec.ServiceAccount != "" {
		saName = argoNs.Spec.ServiceAccount
	}
	saToken := fmt.Sprintf("%s-token", saName)

	err = client.Resource(secretGVR).Namespace(namespace).Delete(context.TODO(), saToken, metav1.DeleteOptions{})
	if err != nil {
		log.Printf("unable to delete argo svc account token secret %v", err)
		return err
	}
	log.Printf("succesfully deleted argo svc account token secret")

	if argoNs.Spec.ServiceAccount == "" {
		log.Printf("not using existing service account,cleaning up role and sa")
		err = client.Resource(saGVR).Namespace(namespace).Delete(context.TODO(), saName, metav1.DeleteOptions{})
		if err != nil {
			log.Printf("unable to delete argo svc account %v", err)
			return err
		}
		log.Printf("succesfully deleted argo svc account")

		//role binding
		err = client.Resource(rbGVR).Namespace(namespace).Delete(context.TODO(), saName, metav1.DeleteOptions{})
		if err != nil {
			log.Printf("unable to delete argo svc account role binding %v", err)
			return err
		}
		log.Printf("succesfully deleted argo svc account role binding")
	}
	err = deleteSecret(client, argoNamespace, secretName)
	if err != nil {
		log.Printf("unable to delete argo cluster secret %v", err)
		return err
	}
	return nil
}

func deleteSecret(client *dynamic.DynamicClient, namespace string, secretName string) error {

	err := client.Resource(secretGVR).Namespace(namespace).Delete(context.TODO(), secretName, metav1.DeleteOptions{})
	if err != nil {
		log.Printf("unable to delete argo cluster secret %v", err)
		return err
	}
	log.Printf("succesfully deleted argo cluster secret %s", secretName)
	return nil
}

func createArgoSvcAccount(client *dynamic.DynamicClient, details *ArgoNamespace) (string, error) {
	namespace := details.ObjectMeta.Namespace
	sa := &unstructured.Unstructured{}
	saName := "argo-attach-sa"
	saYaml := fmt.Sprintf(`
apiVersion: v1
kind: ServiceAccount
metadata:
  name: %s
  namespace: %s
`, saName, namespace)

	_ = yaml.Unmarshal([]byte(saYaml), sa)

	_, err := client.Resource(saGVR).Namespace(namespace).Apply(context.TODO(), saName, sa, metav1.ApplyOptions{FieldManager: "argo-attach-controller"})
	if err != nil {
		log.Printf("unable to create or update argo namespace service account %v", err)
		return "", err
	}

	log.Printf("Created ServiceAccount %s in namespace %s", saName, namespace)

	rbYaml := fmt.Sprintf(`
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: %s
  namespace: %s
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: edit
subjects:
- kind: ServiceAccount
  name: %s
  namespace: %s
`, saName, namespace, saName, namespace)

	rb := &unstructured.Unstructured{}
	_ = yaml.Unmarshal([]byte(rbYaml), rb)

	_, err = client.Resource(rbGVR).Namespace(namespace).Apply(context.TODO(), saName, rb, metav1.ApplyOptions{FieldManager: "argo-attach-controller"})
	if err != nil {
		log.Printf("unable to create or update argo namespace service account rolebinding %v", err)
		return "", err
	}

	log.Printf("Created rolebinding %s in namespace %s", saName, namespace)

	returnToken, err := getSAToken(client, namespace, saName)
	if err != nil {
		return "", err
	}
	return returnToken, nil

}

func getSAToken(client *dynamic.DynamicClient, namespace string, saName string) (string, error) {

	_, err := client.Resource(saGVR).Namespace(namespace).Get(context.TODO(), saName, metav1.GetOptions{})

	if err != nil {
		if apierrors.IsNotFound(err) {
			return "", fmt.Errorf("service account %s/%s not found", namespace, saName)
		}
		return "", fmt.Errorf("failed to get service account %s/%s: %w", namespace, saName, err)
	}

	secretName := fmt.Sprintf("%s-token", saName)
	tokenYaml := fmt.Sprintf(`
apiVersion: v1
kind: Secret
metadata:
  annotations:
    kubernetes.io/service-account.name: %s
  name: %s
  namespace: %s
type: kubernetes.io/service-account-token
`, saName, secretName, namespace)

	token := &unstructured.Unstructured{}
	_ = yaml.Unmarshal([]byte(tokenYaml), token)

	_, err = client.Resource(secretGVR).Namespace(namespace).Apply(context.TODO(), secretName, token, metav1.ApplyOptions{FieldManager: "argo-attach-controller"})
	if err != nil {
		log.Printf("unable to create or update argo namespace service account token %v", err)
		return "", err
	}

	log.Printf("Created token %s in namespace %s", saName, namespace)
	log.Printf("retrieving token %s in namespace %s", saName, namespace)
	tokenSecert, err := client.Resource(secretGVR).Namespace(namespace).Get(context.TODO(), secretName, metav1.GetOptions{})
	if err != nil {
		log.Printf("unable to get token secret %v", err)
		return "", err
	}
	var returnToken string
	for i := 0; i < 10; i++ {
		tokenValue, found, _ := unstructured.NestedString(tokenSecert.Object, "data", "token")
		if found && len(tokenValue) > 0 {
			returnToken = tokenValue
			break
		}
		time.Sleep(1 * time.Second)

		tokenSecert, _ = client.Resource(secretGVR).Namespace(namespace).Get(context.TODO(), "argo-attach-sa-token", metav1.GetOptions{})
	}

	if returnToken == "" {
		return "", fmt.Errorf("unable to retrieve token value after waitng 10s")
	}
	decodedBytes, err := base64.StdEncoding.DecodeString(returnToken)
	if err != nil {
		log.Printf("decode error: %v", err)
		return "", err
	}
	decodedToken := string(decodedBytes)
	return decodedToken, nil
}

func (c *Controller) Reconcile(obj interface{}) (reconcileResult error) {
	u, err := toUnstructured(obj)
	if err != nil {
		return fmt.Errorf("error converting to unstructured: %w", err)
	}

	name := u.GetName()
	namespace := u.GetNamespace()
	kind := u.GetKind()
	logPrefix := fmt.Sprintf("[%s/%s/%s]", kind, namespace, name)

	var reconcileErr error
	defer func() {
		if reconcileErr != nil {
			log.Printf("%s DEFER STATUS UPDATE: Attempting to patch status after error: %v\n", logPrefix, reconcileErr)

			latestU, getErr := c.client.Resource(c.gvr).Namespace(namespace).Get(context.TODO(), name, metav1.GetOptions{})
			if getErr != nil {
				if apierrors.IsNotFound(getErr) {
					log.Printf("%s Status patch skipped: Resource was deleted during defer execution.\n", logPrefix)
					reconcileResult = reconcileErr
					return
				}
				log.Printf("%s CRITICAL: Failed to re-fetch object for status patch: %v. Returning original error.\n", logPrefix, getErr)
				reconcileResult = reconcileErr
				return
			}

			statusMap := c.updateStatusFunc(latestU, false, reconcileErr)

			if statusPatchErr := patchStatus(context.TODO(), c.client, c.gvr, latestU, statusMap); statusPatchErr != nil {
				if !apierrors.IsNotFound(statusPatchErr) && !apierrors.IsConflict(statusPatchErr) {
					log.Printf("%s CRITICAL: Failed to patch status after error: %v\n", logPrefix, statusPatchErr)
				}
			}

			reconcileResult = reconcileErr
		}
	}()

	if !u.GetDeletionTimestamp().IsZero() {
		log.Printf("%s DeletionTimestamp detected. Initiating finalization.\n", logPrefix)

		if containsFinalizer(u, c.finalizerName) {
			log.Printf("%s Finalizer %s is present. Starting cleanup...\n", logPrefix, c.finalizerName)

			if cleanupErr := c.cleanupFunc(c.client, obj); cleanupErr != nil {
				log.Printf("%s CLEANUP FAILED: %v. Will retry on next sync.\n", logPrefix, cleanupErr)
				reconcileErr = fmt.Errorf("cleanup failed: %w", cleanupErr)
				return reconcileErr
			}

			currentFinalizers := u.GetFinalizers()
			updatedFinalizers := removeString(currentFinalizers, c.finalizerName)

			if err := patchFinalizer(context.TODO(), c.client, c.gvr, u, c.finalizerName, updatedFinalizers); err != nil {
				log.Printf("%s ERROR patching to remove finalizer: %v\n", logPrefix, err)
				reconcileErr = fmt.Errorf("finalizer removal patch failed: %w", err)
				return reconcileErr
			}
			log.Printf("%s Finalizer %s removed successfully. Deletion will now complete.\n", logPrefix, c.finalizerName)
			return nil
		}

		log.Printf("%s Finalizer not present. Deletion complete/in progress by K8s.\n", logPrefix)
		return nil
	}

	if !containsFinalizer(u, c.finalizerName) {
		log.Printf("%s Finalizer %s missing. Adding it now.\n", logPrefix, c.finalizerName)

		currentFinalizers := u.GetFinalizers()
		updatedFinalizers := append(currentFinalizers, c.finalizerName)

		if err := patchFinalizer(context.TODO(), c.client, c.gvr, u, c.finalizerName, updatedFinalizers); err != nil {
			log.Printf("%s ERROR patching to add finalizer: %v\n", logPrefix, err)
			reconcileErr = fmt.Errorf("finalizer addition patch failed: %w", err)
			return reconcileErr
		}

		statusMap := c.updateStatusFunc(u, false, nil)
		if statusPatchErr := patchStatus(context.TODO(), c.client, c.gvr, u, statusMap); statusPatchErr != nil {
			log.Printf("%s Warning: Failed to patch status after adding finalizer: %v\n", logPrefix, statusPatchErr)
		}

		log.Printf("%s Finalizer added. continuing for main reconciliation.\n", logPrefix)
		// return nil
	}

	log.Printf("%s Finalizer is present. Running normal reconciliation.\n", logPrefix)
	if provisionErr := c.provisionFunc(c.client, obj, c.namespaces); provisionErr != nil {
		reconcileErr = fmt.Errorf("provisioning failed: %w", provisionErr)
		return reconcileErr // Defer handles status update and returns error for retry
	}

	statusMap := c.updateStatusFunc(u, true, nil)
	if statusPatchErr := patchStatus(context.TODO(), c.client, c.gvr, u, statusMap); statusPatchErr != nil {
		fmt.Printf("%s Warning: Failed to patch status after successful provisioning: %v. Requeuing...\n", logPrefix, statusPatchErr)

		return statusPatchErr
	}

	fmt.Printf("%s Normal reconciliation complete and status updated.\n", logPrefix)

	return nil
}

func setupInformer(client dynamic.Interface, gvr schema.GroupVersionResource, controller *Controller, resyncPeriod time.Duration) cache.SharedIndexInformer {
	informer := cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				return client.Resource(gvr).Namespace("").List(context.TODO(), options)
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				return client.Resource(gvr).Namespace("").Watch(context.TODO(), options)
			},
		},
		&unstructured.Unstructured{},
		resyncPeriod,
		cache.Indexers{},
	)

	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(obj)
			if err == nil {
				fmt.Printf("\n--- %s ADD EVENT DETECTED. Queuing %s ---\n", controller.gvr.Resource, key)
				controller.Queue.Add(key)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldU, err := toUnstructured(oldObj)
			if err != nil {
				log.Printf("Error converting old object for update filter: %v\n", err)
				return
			}
			newU, err := toUnstructured(newObj)
			if err != nil {
				log.Printf("Error converting new object for update filter: %v\n", err)
				return
			}

			generationChanged := oldU.GetGeneration() != newU.GetGeneration()
			deletionRequested := !newU.GetDeletionTimestamp().IsZero()
			if generationChanged || deletionRequested {
				key, err := cache.MetaNamespaceKeyFunc(newObj)
				if err == nil {
					reason := "Generation changed"
					if deletionRequested {
						reason = "Deletion requested"
					}
					fmt.Printf("\n--- %s UPDATE EVENT DETECTED (%s). Queuing %s ---\n", controller.gvr.Resource, reason, key)
					controller.Queue.Add(key)
				}
			} else {
				key, _ := cache.MetaNamespaceKeyFunc(newObj)
				fmt.Printf("--- %s UPDATE EVENT IGNORED Generation not changed. Key: %s ---\n", controller.gvr.Resource, key)
			}
		},
		DeleteFunc: func(obj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(obj)
			if err == nil {
				// Deletion processing is essential for cleanup if the finalizer was removed externally.
				fmt.Printf("\n--- %s DELETE EVENT DETECTED. Queuing %s ---\n", controller.gvr.Resource, key)
				controller.Queue.Add(key)
			}
		},
	})
	return informer
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var namespaces StringSlice
	flag.Var(&namespaces, "blocked-ns", "blocked namespaces , these namespaces will not be allowed as argo namespace options in the CR(can be specified multiple times)")
	resync := flag.Int("resync-period", 60, "time in seconds")
	resyncPeriod := time.Duration(*resync) * time.Second

	flag.Parse() // parse flags

	var kubeconfig string
	if home := homedir.HomeDir(); home != "" {
		fmt.Println("using local kubeconfig")
		kubeconfig = filepath.Join(home, ".kube", "config")
	}

	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		fmt.Println("Falling back to in-cluster config")
		config, err = rest.InClusterConfig()
		if err != nil {
			panic(err.Error())
		}
	}

	dynClient, err := dynamic.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	rateLimiter := workqueue.NewItemExponentialFailureRateLimiter(time.Second, 60*time.Second)
	argoClusterGVR := schema.GroupVersionResource{Group: "field.vmware.com", Version: "v1", Resource: "argoclusters"}
	argoClusterFinalizer := "field.vmware.com/argo-attach-cluster-cleanup"

	argoClusterController := &Controller{
		client:           dynClient,
		gvr:              argoClusterGVR,
		finalizerName:    argoClusterFinalizer,
		provisionFunc:    applyArgoCluster,
		cleanupFunc:      deleteClusterCleanup,
		updateStatusFunc: updateGenericStatus,
		namespaces:       namespaces,
		Queue:            workqueue.NewRateLimitingQueue(rateLimiter),
	}

	argoNamespaceGVR := schema.GroupVersionResource{Group: "field.vmware.com", Version: "v1", Resource: "argonamespaces"}
	argoNamespaceFinalizer := "field.vmware.com/argo-attach-ns-cleanup"

	argoNamespaceController := &Controller{
		client:           dynClient,
		gvr:              argoNamespaceGVR,
		finalizerName:    argoNamespaceFinalizer,
		provisionFunc:    applyArgoNamespace,
		cleanupFunc:      deleteNamespaceCleanup,
		updateStatusFunc: updateGenericStatus,
		namespaces:       []string{},
		Queue:            workqueue.NewRateLimitingQueue(rateLimiter),
	}

	clusterInformer := setupInformer(dynClient, argoClusterController.gvr, argoClusterController, resyncPeriod)
	nsInformer := setupInformer(dynClient, argoNamespaceController.gvr, argoNamespaceController, resyncPeriod)

	stop := make(chan struct{})
	defer close(stop)

	go clusterInformer.Run(stop)
	go nsInformer.Run(stop)

	if !cache.WaitForCacheSync(stop, clusterInformer.HasSynced, nsInformer.HasSynced) {
		fmt.Fprintln(os.Stderr, "Error waiting for cache sync")
		os.Exit(1)
	}
	fmt.Println("Argo Cluster Controller started successfully")

	go argoClusterController.Run(ctx, numWorkers)
	go argoNamespaceController.Run(ctx, numWorkers)

	// Block main function until context is cancelled
	<-ctx.Done()
	fmt.Println("Shutting down controller.")
}
