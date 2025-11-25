// Copyright 2024 Nokia
// Licensed under the BSD 3-Clause License.
// SPDX-License-Identifier: BSD-3-Clause

package cx

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/charmbracelet/log"
	goyaml "gopkg.in/yaml.v3"
)

// SimLinkEndpoint represents one side of a SimLink.
type SimLinkEndpoint struct {
	Node      string
	Interface string
}

// SimLinkSpec represents a link between two SimNodes.
type SimLinkSpec struct {
	EndpointA SimLinkEndpoint
	EndpointB SimLinkEndpoint
}

// buildSimLinkManifest builds a SimLink manifest without applying it.
func (c *CXRuntime) buildSimLinkManifest(link SimLinkSpec) map[string]interface{} {
	// Generate a unique name for the SimLink
	// Format: <nodeA>-<ifaceA>-<nodeB>-<ifaceB>
	linkName := fmt.Sprintf("%s-%s-%s-%s",
		link.EndpointA.Node, sanitizeInterfaceName(link.EndpointA.Interface),
		link.EndpointB.Node, sanitizeInterfaceName(link.EndpointB.Interface))
	linkName = strings.ToLower(linkName)

	// Generate interface resource names
	// Format: <node>-<interface>
	localInterfaceResource := fmt.Sprintf("%s-%s",
		link.EndpointA.Node, sanitizeInterfaceName(link.EndpointA.Interface))
	simInterfaceResource := fmt.Sprintf("%s-%s",
		link.EndpointB.Node, sanitizeInterfaceName(link.EndpointB.Interface))

	// Build the SimLink manifest
	// SimLink connects two SimNodes using local and sim sections
	return map[string]interface{}{
		"apiVersion": "core.eda.nokia.com/v1",
		"kind":       "SimLink",
		"metadata": map[string]interface{}{
			"name":      linkName,
			"namespace": c.topoNamespace,
			"labels": map[string]string{
				simTopologyKey:  "true",
				"clab-lab-name": c.labName,
			},
		},
		"spec": map[string]interface{}{
			"links": []map[string]interface{}{
				{
					"local": map[string]interface{}{
						"node":              link.EndpointA.Node,
						"interface":         link.EndpointA.Interface,
						"interfaceResource": localInterfaceResource,
					},
					"sim": map[string]interface{}{
						"node":              link.EndpointB.Node,
						"interface":         link.EndpointB.Interface,
						"interfaceResource": simInterfaceResource,
					},
				},
			},
		},
	}
}

// AddSimLink adds a SimLink manifest to pending manifests for batch apply.
func (c *CXRuntime) AddSimLink(link SimLinkSpec) {
	simLink := c.buildSimLinkManifest(link)
	linkName := simLink["metadata"].(map[string]interface{})["name"].(string)
	log.Infof("Adding SimLink to batch: %s", linkName)
	c.addPendingManifest(simLink)
}

// CreateSimLink creates a SimLink CRD connecting two SimNode interfaces (direct apply).
func (c *CXRuntime) CreateSimLink(ctx context.Context, link SimLinkSpec) error {
	simLink := c.buildSimLinkManifest(link)
	linkName := simLink["metadata"].(map[string]interface{})["name"].(string)

	log.Infof("Creating SimLink: %s", linkName)

	var buf bytes.Buffer
	encoder := goyaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(simLink); err != nil {
		return fmt.Errorf("failed to marshal SimLink: %w", err)
	}
	encoder.Close()
	manifest := buf.Bytes()

	log.Debugf("SimLink manifest:\n%s", string(manifest))

	cmd := exec.CommandContext(ctx, "kubectl", "apply", "-f", "-")
	cmd.Stdin = bytes.NewReader(manifest)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to create SimLink %s: %w, output: %s", linkName, err, string(output))
	}

	log.Debugf("SimLink created: %s", string(output))
	return nil
}

// DeleteSimLink deletes a SimLink CRD.
func (c *CXRuntime) DeleteSimLink(ctx context.Context, linkName string) error {
	log.Infof("Deleting SimLink: %s", linkName)

	cmd := exec.CommandContext(ctx, "kubectl",
		"-n", c.topoNamespace,
		"delete", "simlink", linkName,
		"--ignore-not-found",
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to delete SimLink %s: %w, output: %s", linkName, err, string(output))
	}

	log.Debugf("SimLink deleted: %s", string(output))
	return nil
}

// DeleteSimLinksForLab deletes all SimLinks associated with a lab.
func (c *CXRuntime) DeleteSimLinksForLab(ctx context.Context) error {
	if c.labName == "" {
		return nil
	}

	log.Infof("Deleting SimLinks for lab: %s", c.labName)

	// First get the list of SimLinks for this lab
	selector := fmt.Sprintf("clab-lab-name=%s", c.labName)
	listCmd := exec.CommandContext(ctx, "kubectl",
		"-n", c.topoNamespace,
		"get", "simlink",
		"-l", selector,
		"-o", "jsonpath={.items[*].metadata.name}",
	)
	output, err := listCmd.Output()
	if err != nil {
		log.Debugf("No SimLinks found for lab %s", c.labName)
		return nil
	}

	simLinkNames := strings.Fields(string(output))
	if len(simLinkNames) == 0 {
		log.Debug("No SimLinks to delete")
		return nil
	}

	// Try to use edactl for deletion to keep state in sync
	toolboxPod, err := c.getToolboxPodName(ctx)
	if err != nil {
		// Fallback to kubectl if toolbox not found
		log.Warnf("Toolbox not found, using kubectl delete: %v", err)
		cmd := exec.CommandContext(ctx, "kubectl",
			"-n", c.topoNamespace,
			"delete", "simlink",
			"-l", selector,
			"--ignore-not-found",
		)
		output, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("failed to delete SimLinks for lab %s: %w, output: %s", c.labName, err, string(output))
		}
		log.Debugf("SimLinks deleted: %s", string(output))
		return nil
	}

	// Delete each SimLink via edactl using file-based delete
	for _, name := range simLinkNames {
		log.Debugf("Deleting SimLink via edactl: %s", name)

		deleteYAML := fmt.Sprintf(`apiVersion: core.eda.nokia.com/v1
kind: SimLink
metadata:
  name: %s
  namespace: %s
`, name, c.topoNamespace)

		remotePath := fmt.Sprintf("/tmp/clab-delete-simlink-%s.yaml", name)
		writeCmd := exec.CommandContext(ctx, "kubectl", "exec", "-i", "-n", c.coreNamespace, toolboxPod,
			"--", "bash", "-c", fmt.Sprintf("cat > %s", remotePath))
		writeCmd.Stdin = strings.NewReader(deleteYAML)
		if output, err := writeCmd.CombinedOutput(); err != nil {
			log.Warnf("Failed to write delete file: %v, output: %s", err, string(output))
			continue
		}

		deleteCmd := exec.CommandContext(ctx, "kubectl", "exec", "-n", c.coreNamespace, toolboxPod,
			"--", "edactl", "delete", "-f", remotePath)
		if output, err := deleteCmd.CombinedOutput(); err != nil {
			log.Warnf("edactl delete simlink %s failed: %v, output: %s", name, err, string(output))
		}

		// Cleanup
		cleanupCmd := exec.CommandContext(ctx, "kubectl", "exec", "-n", c.coreNamespace, toolboxPod,
			"--", "rm", "-f", remotePath)
		_ = cleanupCmd.Run()
	}

	log.Debugf("SimLinks deleted for lab: %s", c.labName)
	return nil
}

// sanitizeInterfaceName converts interface names to valid Kubernetes resource name parts.
// e.g., "eth1_1" -> "eth1-1", "Ethernet1/1" -> "ethernet1-1"
func sanitizeInterfaceName(name string) string {
	name = strings.ToLower(name)
	name = strings.ReplaceAll(name, "_", "-")
	name = strings.ReplaceAll(name, "/", "-")
	name = strings.ReplaceAll(name, ":", "-")
	return name
}
