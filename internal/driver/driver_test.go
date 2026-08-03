package driver

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestServesIdentityOverUnixSocket(t *testing.T) {
	endpoint := "unix://" + filepath.Join(t.TempDir(), "csi.sock")

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- Run(ctx, Config{Endpoint: endpoint, NodeID: "test-node", Version: "v0.0.0-test"}) }()
	t.Cleanup(func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("Run: %v", err)
		}
	})

	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial %s: %v", endpoint, err)
	}
	t.Cleanup(func() { conn.Close() })

	client := csi.NewIdentityClient(conn)
	callCtx, callCancel := context.WithTimeout(ctx, 10*time.Second)
	defer callCancel()

	// WaitForReady rides out the gap between dialling and Run reaching Serve.
	info, err := client.GetPluginInfo(callCtx, &csi.GetPluginInfoRequest{}, grpc.WaitForReady(true))
	if err != nil {
		t.Fatalf("GetPluginInfo: %v", err)
	}
	if info.GetName() != Name {
		t.Errorf("plugin name = %q, want %q", info.GetName(), Name)
	}
	if info.GetVendorVersion() != "v0.0.0-test" {
		t.Errorf("vendor version = %q, want %q", info.GetVendorVersion(), "v0.0.0-test")
	}

	probe, err := client.Probe(callCtx, &csi.ProbeRequest{})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !probe.GetReady().GetValue() {
		t.Error("Probe reported not ready")
	}
}

func TestListenRejectsUnknownScheme(t *testing.T) {
	if _, err := listen("http://127.0.0.1:0"); err == nil {
		t.Fatal("expected an error for an unsupported scheme")
	}
}
