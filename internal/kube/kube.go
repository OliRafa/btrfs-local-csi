// Package kube reads PersistentVolumeClaim annotations, which is how a claim
// overrides the ownership and name its StorageClass would otherwise decide.
package kube

import (
	"context"
	"errors"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type Client struct {
	clientset kubernetes.Interface
}

// NewInCluster returns a client, or nil when the process is not running inside
// a cluster. A nil client is not an error: the driver still provisions, it just
// cannot honour per-PVC overrides, which is exactly the situation under
// csi-sanity and in local tests.
func NewInCluster() (*Client, error) {
	config, err := rest.InClusterConfig()
	if errors.Is(err, rest.ErrNotInCluster) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("build in-cluster config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("build clientset: %w", err)
	}
	return &Client{clientset: clientset}, nil
}

// New wraps an existing clientset, for tests.
func New(clientset kubernetes.Interface) *Client {
	return &Client{clientset: clientset}
}

func (c *Client) Annotations(ctx context.Context, namespace, name string) (map[string]string, error) {
	claim, err := c.clientset.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get persistentvolumeclaim %s/%s: %w", namespace, name, err)
	}
	return claim.Annotations, nil
}
