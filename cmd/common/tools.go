package common

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/log"
	"github.com/srl-labs/containerlab/clab"
	clabels "github.com/srl-labs/containerlab/labels"
	"github.com/srl-labs/containerlab/runtime"
	"github.com/srl-labs/containerlab/types"
)

// createLabels creates container labels
func CreateLabels(labName, containerName, owner, toolType string) map[string]string {
	shortName := strings.Replace(containerName, "clab-"+labName+"-", "", 1)

	labels := map[string]string{
		"containerlab":       labName,
		"clab-node-name":     shortName,
		"clab-node-longname": containerName,
		"clab-node-kind":     "linux",
		"clab-node-group":    "",
		"clab-node-type":     "tool",
		"tool-type":          toolType,
	}

	// Add topology file path
	if Topo != "" {
		absPath, err := filepath.Abs(Topo)
		if err == nil {
			labels["clab-topo-file"] = absPath
		} else {
			labels["clab-topo-file"] = Topo
		}

		// Set node lab directory
		baseDir := filepath.Dir(Topo)
		labels["clab-node-lab-dir"] = filepath.Join(baseDir, "clab-"+labName, shortName)
	}

	// Add owner label if available
	if owner != "" {
		labels[clabels.Owner] = owner
	}

	return labels
}

// GetOwner determines the owner name from a provided parameter or environment variables.
// It first checks the provided owner parameter, then falls back to SUDO_USER environment
// variable, and finally the USER environment variable.
func GetOwner(owner string) string {
	// If owner is explicitly provided, use it
	if owner != "" {
		return owner
	}

	// Check for SUDO_USER first (when commands are run with sudo)
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" {
		return sudoUser
	}

	// Fall back to USER environment variable
	return os.Getenv("USER")
}

// GetLabConfig gets lab configuration and returns lab name, network name and containerlab instance
func GetLabConfig(ctx context.Context, toolLabName string) (string, string, *clab.CLab, error) {
	var labName string
	var c *clab.CLab
	var err error

	// If lab name is provided directly, use it
	if toolLabName != "" {
		labName = toolLabName
	}

	// If topo file is provided or discovered
	if Topo == "" && labName == "" {
		// Auto-discover topology files in current directory
		cwd, err := os.Getwd()
		if err == nil {
			entries, err := os.ReadDir(cwd)
			if err == nil {
				for _, entry := range entries {
					if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".clab.yml") {
						// Found a topology file
						Topo = filepath.Join(cwd, entry.Name())
						log.Debugf("Found topology file: %s", Topo)
						break
					}
				}
			}
		}
	}

	// If we have lab name but no topo file, try to find it from containers
	if labName != "" && Topo == "" {
		_, rinit, err := clab.RuntimeInitializer(Runtime)
		if err != nil {
			return "", "", nil, err
		}

		rt := rinit()
		err = rt.Init(runtime.WithConfig(&runtime.RuntimeConfig{Timeout: Timeout}))
		if err != nil {
			return "", "", nil, err
		}

		// Find containers for this lab
		filter := []*types.GenericFilter{
			{
				FilterType: "label",
				Field:      "containerlab",
				Operator:   "=",
				Match:      labName,
			},
		}
		containers, err := rt.ListContainers(ctx, filter)
		if err != nil {
			return "", "", nil, err
		}

		if len(containers) == 0 {
			return "", "", nil, fmt.Errorf("lab '%s' not found - no running containers", labName)
		}

		// Get topo file from container labels
		topoFile := containers[0].Labels["clab-topo-file"]
		if topoFile == "" {
			return "", "", nil, fmt.Errorf("could not determine topology file from container labels")
		}

		log.Debugf("Found topology file for lab %s: %s", labName, topoFile)
		Topo = topoFile
	}

	// Create a single containerlab instance
	opts := []clab.ClabOption{
		clab.WithTimeout(Timeout),
		clab.WithRuntime(Runtime, &runtime.RuntimeConfig{
			Debug:            Debug,
			Timeout:          Timeout,
			GracefulShutdown: Graceful,
		}),
		clab.WithDebug(Debug),
	}

	if Topo != "" {
		opts = append(opts, clab.WithTopoPath(Topo, VarsFile))
	} else {
		return "", "", nil, fmt.Errorf("no topology file found or provided")
	}

	c, err = clab.NewContainerLab(opts...)
	if err != nil {
		return "", "", nil, fmt.Errorf("failed to create containerlab instance: %w", err)
	}

	if c.Config == nil {
		return "", "", nil, fmt.Errorf("failed to load lab configuration")
	}

	// Get lab name if not provided
	if labName == "" {
		labName = c.Config.Name
	}

	// Get network name
	networkName := c.Config.Mgmt.Network
	if networkName == "" {
		networkName = "clab-" + c.Config.Name
	}

	return labName, networkName, c, nil
}
