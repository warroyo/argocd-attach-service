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
)

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

type Controller struct {
	client        *dynamic.DynamicClient
	gvr           schema.GroupVersionResource
	finalizerName string
	provisionFunc func(*dynamic.DynamicClient, interface{}, []string) // Function to run during normal operation
	cleanupFunc   func(*dynamic.DynamicClient, interface{}) error     // Function to run during cleanup
	namespaces    []string
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
	ClusterLabels  map[string]string `json:"clusterLabels,omitempty"`
	Project        string            `json:"project"`
	ServiceAccount string            `json:"serviceAccount"`
}
type ArgoClusterSpec struct {
	ClusterName   string            `json:"clusterName"`
	ArgoNamespace string            `json:"argoNamespace"`
	ClusterLabels map[string]string `json:"clusterLabels,omitempty"`
	Project       string            `json:"project"`
}

type ArgoClusterSecret struct {
	Name             string `json:"name,omitempty"`
	Server           string `json:"server,omitempty"`
	Config           string `json:"config,omitempty"`
	Project          string `json:"project,omitempty"`
	ClusterResources string `json:"clusterResources,omitempty"`
	Namespaces       string `json:"namespaces,omitempty"`
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

func toUnstructured(obj interface{}) (*unstructured.Unstructured, error) {
	if runtimeObj, ok := obj.(runtime.Object); ok {
		return runtimeObj.(*unstructured.Unstructured), nil
	}
	return nil, fmt.Errorf("object is not an unstructured object")
}

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

func applyArgoNamespace(client *dynamic.DynamicClient, obj interface{}, namespaces []string) {

	argoNs, err := convertNs(obj)
	if err != nil {
		log.Printf("unable to convert object to structured argocd namespace: %v", err)
		return
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
			return
		}
	} else {
		//get existing service account token
		token, err = getSAToken(client, argoNs.Namespace, argoNs.Spec.ServiceAccount)
		if err != nil {
			log.Printf("unable to get svc account token for %s: %v", argoNs.Spec.ServiceAccount, err)
			return
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
	}
	secretData := &ArgoClusterSecret{
		Name:       base64.StdEncoding.EncodeToString([]byte(clusterName)),
		Server:     base64.StdEncoding.EncodeToString([]byte("https://kubernetes.default.svc.cluster.local:443")),
		Project:    base64.StdEncoding.EncodeToString([]byte(project)),
		Config:     base64.StdEncoding.EncodeToString([]byte(string(jsonConfig))),
		Namespaces: base64.StdEncoding.EncodeToString([]byte(string(argoNs.Namespace))),
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
		return
	}
	secretName := fmt.Sprintf("%s-argo-cluster", clusterName)
	log.Printf("succesfully created or update argo cluster secret %s", secretName)

}

func applyArgoCluster(client *dynamic.DynamicClient, obj interface{}, namespaces []string) {
	argoCluster, err := convertObj(obj)
	if err != nil {
		log.Printf("unable to convert object to structured argocd cluster: %v", err)
		return
	}

	namespace := argoCluster.ObjectMeta.Namespace
	clusterName := argoCluster.Spec.ClusterName
	project := argoCluster.Spec.Project
	argoNamespace := argoCluster.Spec.ArgoNamespace

	if slices.Contains(namespaces, argoNamespace) {
		log.Printf("argoNamespace is in the list of blocked namespaces, not creating secret: %v", namespaces)
		return
	}

	kubeconfigUns, err := getSecret(client, namespace, clusterName)
	if err != nil {
		log.Printf("unable to retrieve kubeconfig secret: %v", err)
		return
	}

	kubeconfig, found, err := unstructured.NestedStringMap(kubeconfigUns.Object, "data")
	if err != nil || !found {
		log.Printf("cannot get secret data: %v", err)
		return
	}

	encodedSecret, ok := kubeconfig["value"]
	if !ok {
		log.Println("value does not exist in kubeconfig secret")
		return
	}

	decoded, err := base64.StdEncoding.DecodeString(encodedSecret)
	if err != nil {
		log.Printf("failed to decode value: %v", err)
		return
	}

	config, err := clientcmd.Load(decoded)
	if err != nil {
		log.Printf("failed to read kubconfig data: %v", err)
		return
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
	secretData := &ArgoClusterSecret{
		Name:             base64.StdEncoding.EncodeToString([]byte(clusterName)),
		Server:           base64.StdEncoding.EncodeToString([]byte(config.Clusters[clusterName].Server)),
		ClusterResources: base64.StdEncoding.EncodeToString([]byte("true")),
		Project:          base64.StdEncoding.EncodeToString([]byte(project)),
		Config:           base64.StdEncoding.EncodeToString([]byte(string(jsonConfig))),
	}

	err = applySecret(client, &argoCluster, secretData)
	if err != nil {
		log.Printf("unable to create or update argo cluster secret %v", err)
		return
	}
	secretName := fmt.Sprintf("%s-argo-cluster", clusterName)
	log.Printf("succesfully created or update argo cluster secret %s", secretName)

}

func applySecret(client *dynamic.DynamicClient, argoCluster *ArgoCluster, secretData *ArgoClusterSecret) error {
	labels := argoCluster.Spec.ClusterLabels
	clusterName := argoCluster.Spec.ClusterName
	argoNamespace := argoCluster.Spec.ArgoNamespace
	labels["argocd.argoproj.io/secret-type"] = "cluster"
	secretName := fmt.Sprintf("%s-argo-cluster", clusterName)
	secret := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata": map[string]interface{}{
				"name":   secretName,
				"labels": labels,
			},
			"type": "Opaque",
			"data": secretData,
		},
	}

	_, err := client.Resource(secretGVR).Namespace(argoNamespace).Apply(context.TODO(), secretName, secret, metav1.ApplyOptions{FieldManager: "argo-attach-controller"})
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

	_, err := client.Resource(secretGVR).Namespace(namespace).Apply(context.TODO(), secretName, token, metav1.ApplyOptions{FieldManager: "argo-attach-controller"})
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
	return returnToken, nil
}

func (c *Controller) Reconcile(obj interface{}) error {
	u, err := toUnstructured(obj)
	if err != nil {
		return fmt.Errorf("error converting to unstructured: %w", err)
	}

	name := u.GetName()
	namespace := u.GetNamespace()
	kind := u.GetKind()
	logPrefix := fmt.Sprintf("[%s/%s/%s]", kind, namespace, name)

	if !u.GetDeletionTimestamp().IsZero() {
		log.Printf("%s DeletionTimestamp detected. Initiating finalization.\n", logPrefix)

		if containsFinalizer(u, c.finalizerName) {
			log.Printf("%s Finalizer %s is present. Starting cleanup...\n", logPrefix, c.finalizerName)

			if cleanupErr := c.cleanupFunc(c.client, obj); cleanupErr != nil {
				log.Printf("%s CLEANUP FAILED: %v. Will retry on next sync.\n", logPrefix, cleanupErr)
				return cleanupErr
			}

			currentFinalizers := u.GetFinalizers()
			updatedFinalizers := removeString(currentFinalizers, c.finalizerName)

			if err := patchFinalizer(context.TODO(), c.client, c.gvr, u, c.finalizerName, updatedFinalizers); err != nil {
				log.Printf("%s ERROR patching to remove finalizer: %v\n", logPrefix, err)
				return err
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
			return err
		}

		log.Printf("%s Finalizer added. Requeuing for main reconciliation.\n", logPrefix)
		return nil
	}

	log.Printf("%s Finalizer is present. Running normal reconciliation.\n", logPrefix)
	c.provisionFunc(c.client, obj, c.namespaces)

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
			log.Printf("\n--- %s ADD EVENT DETECTED ---\n", controller.gvr.Resource)
			if err := controller.Reconcile(obj); err != nil {
				log.Printf("ERROR in %s AddFunc reconciliation: %v\n", controller.gvr.Resource, err)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			log.Printf("\n--- %s UPDATE EVENT DETECTED ---\n", controller.gvr.Resource)
			if err := controller.Reconcile(newObj); err != nil {
				log.Printf("ERROR in %s UpdateFunc reconciliation: %v\n", controller.gvr.Resource, err)
			}
		},
		DeleteFunc: func(obj interface{}) {
			log.Printf("\n--- %s DELETE EVENT DETECTED (Final Removal) ---\n", controller.gvr.Resource)
		},
	})
	return informer
}

func main() {
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

	argoClusterGVR := schema.GroupVersionResource{Group: "field.vmware.com", Version: "v1", Resource: "argoclusters"}
	argoClusterFinalizer := "field.vmware.com/argo-attach-cluster-cleanup"

	argoClusterController := &Controller{
		client:        dynClient,
		gvr:           argoClusterGVR,
		finalizerName: argoClusterFinalizer,
		provisionFunc: applyArgoCluster,
		cleanupFunc:   deleteClusterCleanup,
		namespaces:    namespaces,
	}

	argoNamespaceGVR := schema.GroupVersionResource{Group: "field.vmware.com", Version: "v1", Resource: "argonamespaces"}
	argoNamespaceFinalizer := "field.vmware.com/argo-attach-ns-cleanup"

	argoNamespaceController := &Controller{
		client:        dynClient,
		gvr:           argoNamespaceGVR,
		finalizerName: argoNamespaceFinalizer,
		provisionFunc: applyArgoNamespace,
		cleanupFunc:   deleteNamespaceCleanup,
		namespaces:    []string{},
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

	<-stop
}
