package driver

import (
	"context"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

type identity struct {
	csi.UnimplementedIdentityServer
	version string
}

func (i identity) GetPluginInfo(context.Context, *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	return &csi.GetPluginInfoResponse{Name: Name, VendorVersion: i.version}, nil
}

func (i identity) GetPluginCapabilities(context.Context, *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	return &csi.GetPluginCapabilitiesResponse{
		Capabilities: []*csi.PluginCapability{
			serviceCapability(csi.PluginCapability_Service_CONTROLLER_SERVICE),
			serviceCapability(csi.PluginCapability_Service_VOLUME_ACCESSIBILITY_CONSTRAINTS),
		},
	}, nil
}

func (i identity) Probe(context.Context, *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	return &csi.ProbeResponse{Ready: wrapperspb.Bool(true)}, nil
}

func serviceCapability(t csi.PluginCapability_Service_Type) *csi.PluginCapability {
	return &csi.PluginCapability{
		Type: &csi.PluginCapability_Service_{
			Service: &csi.PluginCapability_Service{Type: t},
		},
	}
}
