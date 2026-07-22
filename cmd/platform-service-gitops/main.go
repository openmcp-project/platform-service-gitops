// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and Open Control Plane contributors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"os"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"github.com/spf13/cobra"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"

	"github.com/openmcp-project/controller-utils/pkg/clusters"
	"github.com/openmcp-project/controller-utils/pkg/logging"
	clustersv1alpha1 "github.com/openmcp-project/openmcp-operator/api/clusters/v1alpha1"
	openmcpconsts "github.com/openmcp-project/openmcp-operator/api/constants"
	"github.com/openmcp-project/openmcp-operator/lib/clusteraccess"

	"github.com/openmcp-project/platform-service-gitops/internal/controller/core"
	githubcontroller "github.com/openmcp-project/platform-service-gitops/internal/controller/github"
	"github.com/openmcp-project/platform-service-gitops/internal/mcpaccess"
	"github.com/openmcp-project/platform-service-gitops/internal/scheme"
	// +kubebuilder:scaffold:imports
)

const controllerName = "gitops.open-control-plane.io"

var logger logging.Logger

func main() {
	rootCmd := &cobra.Command{
		Use:   "platform-service-gitops",
		Short: "GitOps platform service for OpenControlPlane",
	}

	runCmd := &cobra.Command{
		Use:   "run",
		Short: "Run the platform-service-gitops controller manager",
		RunE:  runCommand,
	}
	addCommonFlags(runCmd)
	addServerFlags(runCmd)

	rootCmd.AddCommand(runCmd)

	var err error
	logger, err = logging.GetLogger()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to get logger: %v\n", err)
		os.Exit(1)
	}
	ctrl.SetLogger(logger.Logr())

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func addCommonFlags(cmd *cobra.Command) {
	cmd.Flags().String("credential-namespace", "platform-service-gitops-system",
		"Default namespace for GitHub App credential Secrets referenced by GitHubInstance resources.")
	cmd.Flags().String("flux-namespace", "flux-system",
		"Namespace inside each MCP where Flux Secrets and GitRepository resources are written.")
	cmd.Flags().Duration("token-renew-buffer", 15*time.Minute,
		"How long before token expiry to rotate it (e.g. 15m). The actual expiry comes from GitHub.")
}

func addServerFlags(cmd *cobra.Command) {
	cmd.Flags().String("metrics-bind-address", "0",
		"The address the metrics endpoint binds to. Use :8443 for HTTPS or :8080 for HTTP.")
	cmd.Flags().String("health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	cmd.Flags().Bool("leader-elect", false, "Enable leader election for controller manager.")
	cmd.Flags().Bool("metrics-secure", true, "Serve metrics over HTTPS.")
	cmd.Flags().Bool("enable-http2", false, "Enable HTTP/2 for the metrics and webhook servers.")
	cmd.Flags().String("webhook-cert-path", "", "Directory containing the webhook certificate.")
	cmd.Flags().String("webhook-cert-name", "tls.crt", "Webhook certificate filename.")
	cmd.Flags().String("webhook-cert-key", "tls.key", "Webhook key filename.")
	cmd.Flags().String("metrics-cert-path", "", "Directory containing the metrics server certificate.")
	cmd.Flags().String("metrics-cert-name", "tls.crt", "Metrics server certificate filename.")
	cmd.Flags().String("metrics-cert-key", "tls.key", "Metrics server key filename.")
}

func initializePlatformCluster() (*clusters.Cluster, error) {
	platformCluster := clusters.New("platform").WithRESTConfig(ctrl.GetConfigOrDie())
	if err := platformCluster.InitializeClient(scheme.Platform); err != nil {
		return nil, fmt.Errorf("failed to initialize platform cluster client: %w", err)
	}
	return platformCluster, nil
}

// nolint:gocyclo
func runCommand(cmd *cobra.Command, _ []string) error {
	metricsAddr, _ := cmd.Flags().GetString("metrics-bind-address")
	probeAddr, _ := cmd.Flags().GetString("health-probe-bind-address")
	enableLeaderElection, _ := cmd.Flags().GetBool("leader-elect")
	secureMetrics, _ := cmd.Flags().GetBool("metrics-secure")
	enableHTTP2, _ := cmd.Flags().GetBool("enable-http2")
	webhookCertPath, _ := cmd.Flags().GetString("webhook-cert-path")
	webhookCertName, _ := cmd.Flags().GetString("webhook-cert-name")
	webhookCertKey, _ := cmd.Flags().GetString("webhook-cert-key")
	metricsCertPath, _ := cmd.Flags().GetString("metrics-cert-path")
	metricsCertName, _ := cmd.Flags().GetString("metrics-cert-name")
	metricsCertKey, _ := cmd.Flags().GetString("metrics-cert-key")
	credentialNamespace, _ := cmd.Flags().GetString("credential-namespace")
	fluxNamespace, _ := cmd.Flags().GetString("flux-namespace")
	tokenRenewBuffer, _ := cmd.Flags().GetDuration("token-renew-buffer")

	var tlsOpts []func(*tls.Config)
	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, func(c *tls.Config) {
			logger.Info("disabling HTTP/2")
			c.NextProtos = []string{"http/1.1"}
		})
	}

	platformCluster, err := initializePlatformCluster()
	if err != nil {
		return fmt.Errorf("failed to initialize platform cluster: %w", err)
	}

	ctx := context.Background()
	podNamespace := os.Getenv(openmcpconsts.EnvVariablePodNamespace)

	clusterAccessMgr := clusteraccess.NewClusterAccessManager(platformCluster.Client(), controllerName, podNamespace).
		WithLogger(&logger).
		WithInterval(10 * time.Second).
		WithTimeout(30 * time.Minute)

	onboardingCluster, err := clusterAccessMgr.CreateAndWaitForCluster(ctx, "onboarding",
		clustersv1alpha1.PURPOSE_ONBOARDING, scheme.Onboarding,
		[]clustersv1alpha1.PermissionsRequest{
			{
				Rules: []rbacv1.PolicyRule{
					{
						APIGroups: []string{"gitops.open-control-plane.io", "github.gitops.open-control-plane.io"},
						Resources: []string{"gitrepositories", "appinstallations", "kustomizations"},
						Verbs:     []string{"get", "list", "watch"},
					},
					{
						APIGroups: []string{"gitops.open-control-plane.io"},
						Resources: []string{"gitrepositories/status", "kustomizations/status"},
						Verbs:     []string{"get", "update", "patch"},
					},
				},
			},
		})
	if err != nil {
		return fmt.Errorf("failed to obtain onboarding cluster access: %w", err)
	}

	webhookTLSOpts := tlsOpts
	if len(webhookCertPath) > 0 {
		logger.Info("Using provided webhook certificate", "path", webhookCertPath)
	}

	webhookServer := webhook.NewServer(webhook.Options{
		TLSOpts:  webhookTLSOpts,
		CertDir:  webhookCertPath,
		CertName: webhookCertName,
		KeyName:  webhookCertKey,
	})

	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}
	if secureMetrics {
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}
	if len(metricsCertPath) > 0 {
		logger.Info("Using provided metrics certificate", "path", metricsCertPath)
		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	mgr, err := ctrl.NewManager(onboardingCluster.RESTConfig(), ctrl.Options{
		Scheme:                 scheme.Onboarding,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "4f40d865.openmcp.cloud",
	})
	if err != nil {
		return fmt.Errorf("unable to start manager: %w", err)
	}

	// Add the platform cluster to the manager so its cache is started.
	// Required for GitHubInstanceReconciler's source.Kind watch on the platform cluster.
	if err := mgr.Add(platformCluster.Cluster()); err != nil {
		return fmt.Errorf("unable to add platform cluster to manager: %w", err)
	}

	// Register Kustomization scheme on the manager scheme so the kustomization controller works.
	if err := kustomizev1.AddToScheme(mgr.GetScheme()); err != nil {
		return fmt.Errorf("unable to add kustomizev1 scheme: %w", err)
	}

	gitRepoReconciler := core.NewGitRepositoryReconciler(
		onboardingCluster.Client(),
		platformCluster.Client(),
		mcpaccess.NewResolver(platformCluster.Client(), podNamespace),
		credentialNamespace,
		fluxNamespace,
		tokenRenewBuffer,
	)
	if err := gitRepoReconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("failed to create controller gitrepository: %w", err)
	}

	ghInstance := githubcontroller.NewGitHubInstanceReconciler(
		platformCluster.Client(), platformCluster.Cluster(), credentialNamespace,
	)
	if err := ghInstance.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("failed to create controller githubinstance: %w", err)
	}

	appInstall := githubcontroller.NewAppInstallationReconciler(platformCluster.Client(), credentialNamespace)
	if err := appInstall.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("failed to create controller appinstallation: %w", err)
	}

	if err := core.NewKustomizationReconciler(onboardingCluster.Client()).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("failed to create controller kustomization: %w", err)
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("unable to set up health check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("unable to set up ready check: %w", err)
	}

	logger.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		return fmt.Errorf("problem running manager: %w", err)
	}
	return nil
}
