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
	"sigs.k8s.io/yaml"
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

// CreateSimLink creates a SimLink CRD connecting two SimNode interfaces.
func (c *CXRuntime) CreateSimLink(ctx context.Context, link SimLinkSpec) error {
	// Generate a unique name for the SimLink
	// Format: <nodeA>-<ifaceA>-<nodeB>-<ifaceB>
	linkName := fmt.Sprintf("%s-%s-%s-%s",
		link.EndpointA.Node, sanitizeInterfaceName(link.EndpointA.Interface),
		link.EndpointB.Node, sanitizeInterfaceName(link.EndpointB.Interface))
	linkName = strings.ToLower(linkName)

	log.Infof("Creating SimLink: %s", linkName)

	// Generate interface resource names
	// Format: <node>-<interface>
	localInterfaceResource := fmt.Sprintf("%s-%s",
		link.EndpointA.Node, sanitizeInterfaceName(link.EndpointA.Interface))
	simInterfaceResource := fmt.Sprintf("%s-%s",
		link.EndpointB.Node, sanitizeInterfaceName(link.EndpointB.Interface))

	// Build the SimLink manifest
	// SimLink connects two SimNodes using local and sim sections
	simLink := map[string]interface{}{
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

	manifest, err := yaml.Marshal(simLink)
	if err != nil {
		return fmt.Errorf("failed to marshal SimLink: %w", err)
	}

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

	selector := fmt.Sprintf("clab-lab-name=%s", c.labName)
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

// sanitizeInterfaceName converts interface names to valid Kubernetes resource name parts.
// e.g., "eth1_1" -> "eth1-1", "Ethernet1/1" -> "ethernet1-1"
func sanitizeInterfaceName(name string) string {
	name = strings.ToLower(name)
	name = strings.ReplaceAll(name, "_", "-")
	name = strings.ReplaceAll(name, "/", "-")
	name = strings.ReplaceAll(name, ":", "-")
	return name
}
