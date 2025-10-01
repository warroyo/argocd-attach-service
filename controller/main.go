package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"path/filepath"
	"slices"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

var secretGVR = schema.GroupVersionResource{
	Group:    "",
	Version:  "v1",
	Resource: "secrets",
}

type StringSlice []string

func (s *StringSlice) String() string {
	return fmt.Sprintf("%v", *s)
}

func (s *StringSlice) Set(value string) error {
	*s = append(*s, value)
	return nil
}

type ArgoCluster struct {
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              *ArgoClusterSpec `json:"spec,omitempty"`
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
}

type ArgoConfig struct {
	TLSClientConfig *TLSClientConfig `json:"tlsClientConfig,omitempty"`
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

func applySecret(client *dynamic.DynamicClient, obj any, namespaces StringSlice) {
	argoCluster, err := convertObj(obj)
	if err != nil {
		log.Printf("unable to convert object to structured argocd cluster: %v", err)
		return
	}

	namespace := argoCluster.ObjectMeta.Namespace
	clusterName := argoCluster.Spec.ClusterName
	project := argoCluster.Spec.Project
	labels := argoCluster.Spec.ClusterLabels
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

	_, err = client.Resource(secretGVR).Namespace(argoNamespace).Apply(context.TODO(), secretName, secret, metav1.ApplyOptions{FieldManager: "argo-attach-controller"})
	if err != nil {
		log.Printf("unable to create or update argo cluster secret %v", err)
		return
	}
	log.Printf("succesfully created or update argo cluster secret %s", secretName)
}

func deleteSecret(client *dynamic.DynamicClient, obj any) {
	argoCluster, err := convertObj(obj)
	if err != nil {
		log.Printf("unable to convert object to structured argocd cluster: %v", err)
		return
	}

	clusterName := argoCluster.Spec.ClusterName
	secretName := fmt.Sprintf("%s-argo-cluster", clusterName)
	argoNamespace := argoCluster.Spec.ArgoNamespace

	err = client.Resource(secretGVR).Namespace(argoNamespace).Delete(context.TODO(), secretName, metav1.DeleteOptions{})
	if err != nil {
		log.Printf("unable to delete argo cluster secret %v", err)
		return
	}
	log.Printf("succesfully deleted argo cluster secret %s", secretName)

}

func main() {
	var namespaces StringSlice
	flag.Var(&namespaces, "blocked-ns", "blocked namespaces , these namespaces will not be allowed as argo namespace options in the CR(can be specified multiple times)")
	resync := flag.Int("resync-period", 60, "time in seconds")

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

	argoCluster := schema.GroupVersionResource{Group: "field.vmware.com", Version: "v1", Resource: "argoclusters"}

	informer := cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				return dynClient.Resource(argoCluster).Namespace("").List(context.TODO(), options)
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				return dynClient.Resource(argoCluster).Namespace("").Watch(context.TODO(), options)
			},
		},
		&unstructured.Unstructured{},
		time.Duration(*resync)*time.Second,
		cache.Indexers{},
	)

	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			fmt.Println("Add event detected:", obj)
			//create a secret in the correct namespace
			applySecret(dynClient, obj, namespaces)

		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			fmt.Println("Update event detected:", newObj)
			//update secret
			applySecret(dynClient, newObj, namespaces)
		},
		DeleteFunc: func(obj interface{}) {
			fmt.Println("Delete event detected:", obj)
			//delete the secret
			deleteSecret(dynClient, obj)
		},
	})

	stop := make(chan struct{})
	defer close(stop)

	go informer.Run(stop)

	if !cache.WaitForCacheSync(stop, informer.HasSynced) {
		panic("Timeout waiting for cache sync")
	}

	fmt.Println("Argo Cluster Controller started successfully")

	<-stop
}
