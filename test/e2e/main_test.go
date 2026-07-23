//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"k8s.io/klog/v2"
	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"

	"github.com/openmcp-project/openmcp-testing/pkg/providers"
	"github.com/openmcp-project/openmcp-testing/pkg/setup"
)

var testenv env.Environment

func TestMain(m *testing.M) {
	klog.InitFlags(nil)

	version := mustVersion()

	openmcp := setup.OpenMCPSetup{
		Namespace: "openmcp-system",
		Operator: setup.OpenMCPOperatorSetup{
			Name: "openmcp-operator",
			// renovate: datasource=docker depName=ghcr.io/openmcp-project/images/openmcp-operator
			Image:        "ghcr.io/openmcp-project/images/openmcp-operator:v1.3.0",
			Environment:  "debug",
			PlatformName: "platform",
		},
		ClusterProviders: []providers.ClusterProviderSetup{
			{
				Name: "kind",
				// renovate: datasource=docker depName=ghcr.io/openmcp-project/images/cluster-provider-kind
				Image: "ghcr.io/openmcp-project/images/cluster-provider-kind:v0.6.0",
			},
		},
		ServiceProviders: []providers.ServiceProviderSetup{
			{
				Name:               "platform-service-gitops",
				Image:              fmt.Sprintf("ghcr.io/openmcp-project/images/platform-service-gitops:%s", version),
				LoadImageToCluster: true,
			},
		},
	}

	testenv = env.NewWithConfig(envconf.New().WithNamespace(openmcp.Namespace))
	openmcp.Bootstrap(testenv)
	os.Exit(testenv.Run(m))
}

func mustVersion() string {
	cmd := exec.Command("../../hack/common/get-version.sh")
	out, err := cmd.Output()
	if err != nil {
		panic(fmt.Sprintf("failed to get version: %v", err))
	}
	return strings.TrimSpace(string(out))
}
