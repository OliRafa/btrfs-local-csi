package kube

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestAnnotations(t *testing.T) {
	client := New(fake.NewClientset(&corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "comics-library",
			Namespace:   "bookstore",
			Annotations: map[string]string{"btrfs-local-csi/uid": "3000"},
		},
	}))

	annotations, err := client.Annotations(t.Context(), "bookstore", "comics-library")
	if err != nil {
		t.Fatalf("Annotations: %v", err)
	}
	if got := annotations["btrfs-local-csi/uid"]; got != "3000" {
		t.Errorf("uid annotation = %q, want %q", got, "3000")
	}
}

func TestAnnotationsOnMissingClaim(t *testing.T) {
	client := New(fake.NewClientset())

	if _, err := client.Annotations(t.Context(), "bookstore", "gone"); err == nil {
		t.Fatal("Annotations for a missing claim = nil, want an error")
	}
}

// Running outside a cluster is not an error; the driver simply loses per-PVC
// overrides.
func TestNewInClusterOutsideACluster(t *testing.T) {
	client, err := NewInCluster()
	if err != nil {
		t.Fatalf("NewInCluster: %v", err)
	}
	if client != nil {
		t.Error("expected a nil client when not running in a cluster")
	}
}
