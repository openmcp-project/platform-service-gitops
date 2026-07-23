//go:build e2e

package e2e

import (
	"context"
	"encoding/base64"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/openmcp-project/openmcp-testing/pkg/clusterutils"
	openmcpconditions "github.com/openmcp-project/openmcp-testing/pkg/conditions"
	"github.com/openmcp-project/openmcp-testing/pkg/providers"
	"github.com/openmcp-project/openmcp-testing/pkg/resources"
)

const mcpName = "test-mcp"

// templateData holds the values substituted into YAML fixture templates.
type templateData struct {
	AppIDBase64      string
	URLBase64        string
	PrivateKeyBase64 string
	GitHubOrg        string
	RepoURL          string
}

func b64(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

func testData() templateData {
	return templateData{
		AppIDBase64:      b64(mustEnv("GITHUB_APP_ID")),
		URLBase64:        b64(mustEnv("GITHUB_URL")),
		PrivateKeyBase64: b64(mustEnv("GITHUB_APP_PRIVATE_KEY")),
		GitHubOrg:        mustEnv("GITHUB_ORG"),
		RepoURL:          mustEnv("GITHUB_REPO_URL"),
	}
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		panic("required environment variable not set: " + key)
	}
	return v
}

func TestGitRepositoryPropagation(t *testing.T) {
	data := testData()

	f := features.New("GitRepository propagateTo creates token Secret in MCP").
		Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			// Ensure the controller namespace exists before creating the credential Secret.
			ns := &corev1.Namespace{}
			ns.SetName("platform-service-gitops-system")
			if err := c.Client().Resources().Create(ctx, ns); err != nil {
				t.Logf("namespace may already exist: %v", err)
			}

			// Secret is a template (contains Base64 fields) — apply separately.
			platformCfg := envconf.NewWithKubeConfig(c.KubeconfigFile()).WithNamespace("platform-service-gitops-system")
			if _, err := resources.CreateObjectsFromTemplateFile(ctx, platformCfg, "platform/secret.yaml", data); err != nil {
				t.Fatalf("creating credential secret: %v", err)
			}
			// GitHubInstance is a plain YAML — apply via dir (only non-template files).
			if _, err := resources.CreateObjectsFromDir(ctx, c, "platform/static"); err != nil {
				t.Fatalf("creating platform static resources: %v", err)
			}
			return ctx
		}).
		Setup(providers.CreateMCP(mcpName)).
		Assess("AppInstallation reports App as installed", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			onboardingCfg, err := clusterutils.OnboardingConfig()
			if err != nil {
				t.Fatalf("getting onboarding config: %v", err)
			}

			if _, err := resources.CreateObjectsFromTemplateFile(ctx, onboardingCfg, "onboarding/appinstallation.yaml", data); err != nil {
				t.Fatalf("creating AppInstallation: %v", err)
			}

			ai := &unstructured.Unstructured{}
			ai.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   "github.gitops.open-control-plane.io",
				Version: "v1alpha1",
				Kind:    "AppInstallation",
			})
			ai.SetName("app-connection")
			ai.SetNamespace("default")
			if err := wait.For(
				openmcpconditions.Match(ai, onboardingCfg, "AppInstalled", corev1.ConditionTrue),
				wait.WithTimeout(3*time.Minute),
			); err != nil {
				t.Fatalf("AppInstallation not ready: %v", err)
			}
			return ctx
		}).
		Assess("GitRepository propagates token Secret to MCP", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			onboardingCfg, err := clusterutils.OnboardingConfig()
			if err != nil {
				t.Fatalf("getting onboarding config: %v", err)
			}

			if _, err := resources.CreateObjectsFromTemplateFile(ctx, onboardingCfg, "onboarding/gitrepository.yaml", data); err != nil {
				t.Fatalf("creating GitRepository: %v", err)
			}

			gr := &unstructured.Unstructured{}
			gr.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   "gitops.open-control-plane.io",
				Version: "v1alpha1",
				Kind:    "GitRepository",
			})
			gr.SetName("test-repo")
			gr.SetNamespace("default")
			if err := wait.For(
				openmcpconditions.Match(gr, onboardingCfg, "Ready", corev1.ConditionTrue),
				wait.WithTimeout(3*time.Minute),
			); err != nil {
				t.Fatalf("GitRepository not ready: %v", err)
			}
			return ctx
		}).
		Assess("token Secret exists in MCP flux-system namespace", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			mcpCfg, err := clusterutils.MCPConfig(ctx, c, mcpName)
			if err != nil {
				t.Fatalf("getting MCP config: %v", err)
			}

			// Ensure flux-system namespace exists on MCP.
			ns := &corev1.Namespace{}
			ns.SetName("flux-system")
			if err := mcpCfg.Client().Resources().Create(ctx, ns); err != nil {
				t.Logf("flux-system namespace (may already exist): %v", err)
			}

			tokenSecret := &corev1.Secret{}
			tokenSecret.SetName("test-repo-token")
			tokenSecret.SetNamespace("flux-system")
			if err := wait.For(
				conditions.New(mcpCfg.Client().Resources()).ResourcesFound(&corev1.SecretList{
					Items: []corev1.Secret{*tokenSecret},
				}),
				wait.WithTimeout(3*time.Minute),
			); err != nil {
				t.Fatalf("token Secret not found in MCP flux-system: %v", err)
			}
			return ctx
		}).
		Teardown(providers.DeleteMCP(mcpName, wait.WithTimeout(5*time.Minute)))

	testenv.Test(t, f.Feature())
}
