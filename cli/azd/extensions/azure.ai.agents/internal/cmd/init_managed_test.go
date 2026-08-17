// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package cmd

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"azureaiagent/internal/project"

	"github.com/azure/azure-dev/cli/azd/pkg/azdext"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

func TestScaffoldPromptConventionFolders_CreatesLayout(t *testing.T) {
	dir := t.TempDir()

	if err := scaffoldPromptConventionFolders(dir, "You are a triage assistant."); err != nil {
		t.Fatalf("scaffoldPromptConventionFolders: %v", err)
	}

	// instructions.md carries the provided instructions.
	content, err := os.ReadFile(filepath.Join(dir, "instructions.md"))
	if err != nil {
		t.Fatalf("read instructions.md: %v", err)
	}
	if string(content) != "You are a triage assistant.\n" {
		t.Errorf("instructions.md content: got %q", string(content))
	}

	// skills/ exists with a .gitkeep placeholder.
	for _, sub := range []string{"skills"} {
		info, statErr := os.Stat(filepath.Join(dir, sub))
		if statErr != nil || !info.IsDir() {
			t.Errorf("%s/ should be a directory: %v", sub, statErr)
		}
		if _, keepErr := os.Stat(filepath.Join(dir, sub, ".gitkeep")); keepErr != nil {
			t.Errorf("%s/.gitkeep should exist: %v", sub, keepErr)
		}
	}

	// files/ is intentionally not scaffolded: file search is not supported for
	// managed (prompt) agents.
	if _, statErr := os.Stat(filepath.Join(dir, "files")); !os.IsNotExist(statErr) {
		t.Errorf("files/ should not be created, got stat err: %v", statErr)
	}
}

func TestScaffoldPromptConventionFolders_DefaultInstructions(t *testing.T) {
	dir := t.TempDir()
	if err := scaffoldPromptConventionFolders(dir, "   "); err != nil {
		t.Fatalf("scaffoldPromptConventionFolders: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(dir, "instructions.md"))
	if err != nil {
		t.Fatalf("read instructions.md: %v", err)
	}
	if string(content) != "You are a helpful AI assistant.\n" {
		t.Errorf("default instructions: got %q", string(content))
	}
}

func TestScaffoldPromptConventionFolders_DoesNotOverwriteInstructions(t *testing.T) {
	dir := t.TempDir()
	existing := "MY EDITED INSTRUCTIONS\n"
	if err := os.WriteFile(filepath.Join(dir, "instructions.md"), []byte(existing), 0o600); err != nil {
		t.Fatalf("seed instructions.md: %v", err)
	}

	if err := scaffoldPromptConventionFolders(dir, "should be ignored"); err != nil {
		t.Fatalf("scaffoldPromptConventionFolders: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(dir, "instructions.md"))
	if err != nil {
		t.Fatalf("read instructions.md: %v", err)
	}
	if string(content) != existing {
		t.Errorf("existing instructions.md should be preserved, got %q", string(content))
	}
}

// TestAddPromptAgentService_DeploymentProvisioningGate is a regression test for
// the prompt-agent (managed harness) model-selection bug: choosing an existing
// model deployment must NOT declare it under the service's deployments (which
// `azd provision` would try to (re)create). Only a newly-configured deployment
// (isNewDeployment=true) is declared for provisioning; a reused deployment is
// referenced by name only.
func TestAddPromptAgentService_DeploymentProvisioningGate(t *testing.T) {
	t.Parallel()

	deployment := &project.Deployment{
		Name: "gpt-4o-mini",
		Model: project.DeploymentModel{
			Name:    "gpt-4o-mini",
			Format:  "OpenAI",
			Version: "2024-07-18",
		},
		Sku: project.DeploymentSku{
			Name:     "GlobalStandard",
			Capacity: 50,
		},
	}

	tests := []struct {
		name            string
		deployment      *project.Deployment
		isNewDeployment bool
		wantDeployments int
	}{
		{
			name:            "new deployment is declared for provisioning",
			deployment:      deployment,
			isNewDeployment: true,
			wantDeployments: 1,
		},
		{
			name:            "existing deployment is referenced, not provisioned",
			deployment:      deployment,
			isNewDeployment: false,
			wantDeployments: 0,
		},
		{
			name:            "nil deployment declares nothing",
			deployment:      nil,
			isNewDeployment: true,
			wantDeployments: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			projectServer := &recordingProjectServer{}
			azdClient := newProjectOnlyAzdClient(t, projectServer)

			settings := project.DefaultPromptAgentSettings()
			err := addPromptAgentService(
				t.Context(), azdClient, "my-agent", ".", &settings, tt.deployment, tt.isNewDeployment,
			)
			require.NoError(t, err)

			require.Len(t, projectServer.added, 1)
			added := projectServer.added[0]
			assert.Equal(t, "my-agent", added.Name)
			assert.Equal(t, AiAgentHost, added.Host)

			var cfg project.ServiceTargetAgentConfig
			require.NoError(t, project.UnmarshalStruct(added.Config, &cfg))
			require.NotNil(t, cfg.PromptAgent, "prompt agent settings must always be recorded")
			assert.Len(t, cfg.Deployments, tt.wantDeployments)
			if tt.wantDeployments == 1 {
				assert.Equal(t, tt.deployment.Name, cfg.Deployments[0].Name)
			}
		})
	}
}

// newProjectOnlyAzdClient stands up a gRPC server exposing only the Project
// service (backed by the supplied server) and returns a client wired to it.
func newProjectOnlyAzdClient(t *testing.T, projectServer azdext.ProjectServiceServer) *azdext.AzdClient {
	t.Helper()

	grpcServer := grpc.NewServer()
	azdext.RegisterProjectServiceServer(grpcServer, projectServer)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(func() {
		grpcServer.Stop()
		_ = listener.Close()
	})

	azdClient, err := azdext.NewAzdClient(azdext.WithAddress(listener.Addr().String()))
	require.NoError(t, err)
	t.Cleanup(func() { azdClient.Close() })

	return azdClient
}
