// Copyright 2024 Nokia
// Licensed under the BSD 3-Clause License.
// SPDX-License-Identifier: BSD-3-Clause

package cx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/charmbracelet/log"
	clabexec "github.com/srl-labs/containerlab/exec"
	clabruntime "github.com/srl-labs/containerlab/runtime"
	clabtypes "github.com/srl-labs/containerlab/types"
	"sigs.k8s.io/yaml"
)

const (
	RuntimeName    = "cx"
	defaultTimeout = 120 * time.Second

	// Default namespaces for EDA
	defaultTopoNamespace = "eda"
	defaultCoreNamespace = "eda-system"

	// Label for containerlab-managed sim resources
	simTopologyKey = "eda.nokia.com/simtopology"
)

// extractShortName extracts the short SimNode name from a containerlab long name.
// Long name format: clab-<labname>-<nodename> -> returns <nodename>
// If it's already a short name, returns it unchanged.
func extractShortName(cID string) string {
	parts := strings.Split(cID, "-")
	if len(parts) > 3 && parts[0] == "clab" {
		// Long name format: clab-<labname>-<nodename>
		return parts[len(parts)-1]
	}
	return cID
}

func init() {
	clabruntime.Register(RuntimeName, func() clabruntime.ContainerRuntime {
		return &CXRuntime{
			mgmt: new(clabtypes.MgmtNet),
		}
	})
}

// CXRuntime implements the ContainerRuntime interface for EDA CX backend.
type CXRuntime struct {
	config        clabruntime.RuntimeConfig
	mgmt          *clabtypes.MgmtNet
	topoNamespace string
	coreNamespace string
	labName       string
}

func (c *CXRuntime) Init(opts ...clabruntime.RuntimeOption) error {
	log.Debug("Runtime: CX (EDA)")

	c.topoNamespace = defaultTopoNamespace
	c.coreNamespace = defaultCoreNamespace

	for _, o := range opts {
		o(c)
	}

	if c.config.Timeout <= 0 {
		c.config.Timeout = defaultTimeout
	}

	return nil
}

func (c *CXRuntime) WithKeepMgmtNet() {
	c.config.KeepMgmtNet = true
}

func (*CXRuntime) GetName() string                     { return RuntimeName }
func (c *CXRuntime) Config() clabruntime.RuntimeConfig { return c.config }
func (c *CXRuntime) Mgmt() *clabtypes.MgmtNet          { return c.mgmt }

func (c *CXRuntime) WithConfig(cfg *clabruntime.RuntimeConfig) {
	c.config.Timeout = cfg.Timeout
	c.config.Debug = cfg.Debug
	c.config.GracefulShutdown = cfg.GracefulShutdown
	if c.config.Timeout <= 0 {
		c.config.Timeout = defaultTimeout
	}
}

func (c *CXRuntime) WithMgmtNet(n *clabtypes.MgmtNet) {
	c.mgmt = n
}

// CreateNet is a no-op for CX runtime as networking is managed by Kubernetes/EDA.
func (c *CXRuntime) CreateNet(ctx context.Context) error {
	log.Debug("CX runtime: CreateNet is a no-op (managed by Kubernetes)")
	return nil
}

// DeleteNet is a no-op for CX runtime.
func (c *CXRuntime) DeleteNet(ctx context.Context) error {
	log.Debug("CX runtime: DeleteNet is a no-op (managed by Kubernetes)")
	return nil
}

// PullImage is a no-op for CX runtime as images are pulled by Kubernetes.
func (c *CXRuntime) PullImage(ctx context.Context, imageName string, pullPolicy clabtypes.PullPolicyValue) error {
	log.Debugf("CX runtime: PullImage is handled by Kubernetes for %s", imageName)
	return nil
}

// CreateContainer creates a SimNode CRD in EDA.
func (c *CXRuntime) CreateContainer(ctx context.Context, node *clabtypes.NodeConfig) (string, error) {
	log.Info("Creating SimNode", "name", node.ShortName)

	// Store lab name for later use
	if c.labName == "" && node.Labels != nil {
		if labName, ok := node.Labels["containerlab"]; ok {
			c.labName = labName
		}
	}

	simNode := c.buildSimNode(node)

	manifest, err := yaml.Marshal(simNode)
	if err != nil {
		return "", fmt.Errorf("failed to marshal SimNode: %w", err)
	}

	log.Debugf("SimNode manifest:\n%s", string(manifest))

	if err := c.applyManifest(ctx, manifest); err != nil {
		return "", fmt.Errorf("failed to create SimNode %s: %w", node.ShortName, err)
	}

	return node.ShortName, nil
}

// StartContainer waits for the SimNode pod to be ready.
func (c *CXRuntime) StartContainer(ctx context.Context, cID string, node clabruntime.Node) (any, error) {
	nodeCfg := node.Config()
	log.Debugf("Starting SimNode: %s", nodeCfg.ShortName)

	// Check if this is a cEOS image that needs bootstrapping
	isCeos := IsCeosImage(nodeCfg.Image)
	if isCeos {
		log.Infof("Detected cEOS image for %s, applying bootstrap patches first", nodeCfg.ShortName)
		// For cEOS, we must apply bootstrap patches BEFORE the pod can start
		// The cEOS image has no ENTRYPOINT/CMD and needs special setup
		if err := c.BootstrapCeos(ctx, nodeCfg.ShortName); err != nil {
			return nil, fmt.Errorf("failed to bootstrap cEOS for %s: %w", nodeCfg.ShortName, err)
		}
	}

	// Wait for the pod to be ready
	podName, err := c.waitForSimPod(ctx, nodeCfg.ShortName)
	if err != nil {
		return nil, fmt.Errorf("failed to wait for SimNode pod %s: %w", nodeCfg.ShortName, err)
	}

	log.Infof("SimNode %s is ready (pod: %s)", nodeCfg.ShortName, podName)

	// Get the pod IP and update the node config
	ip, err := c.getSimNodeIP(ctx, nodeCfg.ShortName)
	if err != nil {
		log.Warnf("Failed to get IP for SimNode %s: %v", nodeCfg.ShortName, err)
	} else {
		nodeCfg.MgmtIPv4Address = ip
	}

	// Create SimLinks for this node's endpoints
	// We create the SimLink from this node's perspective; kubectl apply is idempotent
	// so creating the same link twice (from both endpoints) is fine
	for _, ep := range node.GetEndpoints() {
		link := ep.GetLink()
		if link == nil {
			continue
		}

		endpoints := link.GetEndpoints()
		if len(endpoints) != 2 {
			log.Warnf("Link has %d endpoints, expected 2", len(endpoints))
			continue
		}

		// Get the node names from the endpoints
		epA := endpoints[0]
		epB := endpoints[1]

		// Only create link if this node is the "first" endpoint (alphabetically)
		// This avoids duplicate creation attempts
		nodeA := epA.GetNode()
		nodeB := epB.GetNode()
		if nodeA == nil || nodeB == nil {
			log.Warnf("Link endpoint has nil node")
			continue
		}

		nodeAName := nodeA.GetShortName()
		nodeBName := nodeB.GetShortName()

		// Only create the link from the alphabetically first node
		if nodeCfg.ShortName != nodeAName && nodeCfg.ShortName != nodeBName {
			continue
		}
		if nodeAName > nodeBName {
			// Swap so we always process from the "first" node
			if nodeCfg.ShortName == nodeBName {
				continue // Let the other node create it
			}
		} else {
			if nodeCfg.ShortName == nodeAName {
				// We're the first node, create the link
			} else {
				continue // Let the other node create it
			}
		}

		simLink := SimLinkSpec{
			EndpointA: SimLinkEndpoint{
				Node:      nodeAName,
				Interface: epA.GetIfaceName(),
			},
			EndpointB: SimLinkEndpoint{
				Node:      nodeBName,
				Interface: epB.GetIfaceName(),
			},
		}

		if err := c.CreateSimLink(ctx, simLink); err != nil {
			log.Warnf("Failed to create SimLink for %s: %v", nodeCfg.ShortName, err)
			// Continue with other links, don't fail the whole deployment
		}
	}

	return nil, nil
}

// StopContainer is a no-op for CX runtime (container lifecycle managed by K8s).
func (c *CXRuntime) StopContainer(ctx context.Context, cID string) error {
	log.Debugf("CX runtime: StopContainer is a no-op for %s", cID)
	return nil
}

// PauseContainer is not supported by CX runtime.
func (c *CXRuntime) PauseContainer(ctx context.Context, cID string) error {
	return fmt.Errorf("PauseContainer is not supported by CX runtime")
}

// UnpauseContainer is not supported by CX runtime.
func (c *CXRuntime) UnpauseContainer(ctx context.Context, cID string) error {
	return fmt.Errorf("UnpauseContainer is not supported by CX runtime")
}

// ListContainers lists SimNode pods matching the given filters.
func (c *CXRuntime) ListContainers(ctx context.Context, gfilters []*clabtypes.GenericFilter) ([]clabruntime.GenericContainer, error) {
	log.Debug("CX runtime: ListContainers")

	// Extract name filter and label selector from filters
	var nameFilter string
	labelSelector := c.buildLabelSelector(gfilters)

	for _, f := range gfilters {
		if f.FilterType == "name" {
			nameFilter = f.Match
			break
		}
	}

	// If we have a name filter, get that specific SimNode
	if nameFilter != "" {
		// The name filter from containerlab is the long name (e.g., clab-cx-multitool-server1)
		// but our SimNode uses the short name (e.g., server1)
		// Try multiple name variations to find the SimNode

		namesToTry := []string{nameFilter}

		// Extract potential short name from long name format: clab-<labname>-<nodename>
		parts := strings.Split(nameFilter, "-")
		if len(parts) >= 3 {
			// Try the last part as short name
			namesToTry = append(namesToTry, parts[len(parts)-1])
		}

		for _, name := range namesToTry {
			simNode, err := c.getSimNodeByName(ctx, name)
			if err == nil {
				container, err := c.simNodeToGenericContainer(ctx, simNode)
				if err != nil {
					return nil, err
				}
				container.SetRuntime(c)
				return []clabruntime.GenericContainer{container}, nil
			}
		}

		// Not found with any name variation
		return nil, nil
	}

	// Get SimNodes with label selector
	simNodes, err := c.getSimNodes(ctx, labelSelector)
	if err != nil {
		return nil, fmt.Errorf("failed to list SimNodes: %w", err)
	}

	var containers []clabruntime.GenericContainer
	for _, sn := range simNodes {
		container, err := c.simNodeToGenericContainer(ctx, sn)
		if err != nil {
			log.Warnf("Failed to convert SimNode %s to container: %v", sn.Name, err)
			continue
		}
		container.SetRuntime(c)
		containers = append(containers, container)
	}

	return containers, nil
}

// GetNSPath returns the network namespace path for a container.
func (c *CXRuntime) GetNSPath(ctx context.Context, cID string) (string, error) {
	// Extract the short SimNode name from the long containerlab name
	shortName := extractShortName(cID)

	// Get the pod name for this SimNode
	podName, err := c.getPodName(ctx, shortName)
	if err != nil {
		return "", err
	}

	// Get the pod's PID via crictl or kubectl
	pid, err := c.getPodPID(ctx, podName)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("/proc/%d/ns/net", pid), nil
}

// Exec executes a command inside the SimNode pod.
func (c *CXRuntime) Exec(ctx context.Context, cID string, execCmd *clabexec.ExecCmd) (*clabexec.ExecResult, error) {
	// Extract the short SimNode name from the long containerlab name
	shortName := extractShortName(cID)

	podName, err := c.getPodName(ctx, shortName)
	if err != nil {
		return nil, err
	}

	args := []string{
		"-n", c.coreNamespace,
		"exec", podName,
		"-c", shortName,
		"--",
	}
	args = append(args, execCmd.GetCmd()...)

	cmd := exec.CommandContext(ctx, "kubectl", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err = cmd.Run()

	execResult := clabexec.NewExecResult(execCmd)
	execResult.SetStdOut(stdout.Bytes())
	execResult.SetStdErr(stderr.Bytes())

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			execResult.SetReturnCode(exitErr.ExitCode())
		} else {
			execResult.SetReturnCode(1)
		}
	} else {
		execResult.SetReturnCode(0)
	}

	return execResult, nil
}

// ExecNotWait executes a command without waiting for output.
func (c *CXRuntime) ExecNotWait(ctx context.Context, cID string, execCmd *clabexec.ExecCmd) error {
	// Extract the short SimNode name from the long containerlab name
	shortName := extractShortName(cID)

	podName, err := c.getPodName(ctx, shortName)
	if err != nil {
		return err
	}

	args := []string{
		"-n", c.coreNamespace,
		"exec", podName,
		"-c", shortName,
		"--",
	}
	args = append(args, execCmd.GetCmd()...)

	cmd := exec.CommandContext(ctx, "kubectl", args...)
	return cmd.Start()
}

// DeleteContainer deletes the SimNode CRD and associated SimLinks.
func (c *CXRuntime) DeleteContainer(ctx context.Context, cID string) error {
	// The cID is typically the long name (e.g., clab-cx-multitool-server1)
	// but our SimNode uses the short name (e.g., server1)
	// Try to extract the short name from the long name
	simNodeName := cID
	parts := strings.Split(cID, "-")
	if len(parts) > 3 {
		// Long name format: clab-<labname>-<nodename>
		simNodeName = parts[len(parts)-1]
	}

	// Try to get the lab name from the SimNode's labels if not already set
	if c.labName == "" {
		if simNode, err := c.getSimNodeByName(ctx, simNodeName); err == nil {
			if labName, ok := simNode.Labels["containerlab"]; ok {
				c.labName = labName
			}
		}
	}

	// Delete SimLinks for this lab (only once, on first node deletion)
	// Using label selector ensures we only delete our lab's SimLinks
	if err := c.DeleteSimLinksForLab(ctx); err != nil {
		log.Warnf("Failed to delete SimLinks: %v", err)
		// Continue with SimNode deletion
	}

	log.Infof("Deleting SimNode: %s", simNodeName)

	args := []string{
		"-n", c.topoNamespace,
		"delete", "simnode", simNodeName,
		"--ignore-not-found",
	}

	cmd := exec.CommandContext(ctx, "kubectl", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to delete SimNode %s: %w, output: %s", simNodeName, err, string(output))
	}

	log.Info("Deleted SimNode", "name", simNodeName)
	return nil
}

// GetHostsPath returns the hosts file path (not applicable for CX runtime).
func (c *CXRuntime) GetHostsPath(ctx context.Context, cID string) (string, error) {
	return "", fmt.Errorf("GetHostsPath is not supported by CX runtime")
}

// GetContainerStatus returns the status of a SimNode.
func (c *CXRuntime) GetContainerStatus(ctx context.Context, cID string) clabruntime.ContainerStatus {
	args := []string{
		"-n", c.topoNamespace,
		"get", "simnode", cID,
		"-o", "jsonpath={.status.phase}",
	}

	cmd := exec.CommandContext(ctx, "kubectl", args...)
	output, err := cmd.Output()
	if err != nil {
		return clabruntime.NotFound
	}

	phase := strings.TrimSpace(string(output))
	switch strings.ToLower(phase) {
	case "running", "ready":
		return clabruntime.Running
	case "pending", "creating":
		return clabruntime.Stopped
	default:
		return clabruntime.NotFound
	}
}

// IsHealthy checks if the SimNode pod is healthy.
func (c *CXRuntime) IsHealthy(ctx context.Context, cID string) (bool, error) {
	shortName := extractShortName(cID)
	podName, err := c.getPodName(ctx, shortName)
	if err != nil {
		return false, err
	}

	args := []string{
		"-n", c.coreNamespace,
		"get", "pod", podName,
		"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}",
	}

	cmd := exec.CommandContext(ctx, "kubectl", args...)
	output, err := cmd.Output()
	if err != nil {
		return false, err
	}

	return strings.TrimSpace(string(output)) == "True", nil
}

// WriteToStdinNoWait is not supported by CX runtime.
func (c *CXRuntime) WriteToStdinNoWait(ctx context.Context, cID string, data []byte) error {
	return fmt.Errorf("WriteToStdinNoWait is not supported by CX runtime")
}

// CheckConnection verifies connectivity to the Kubernetes cluster.
func (c *CXRuntime) CheckConnection(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "kubectl", "cluster-info")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to connect to Kubernetes cluster: %w", err)
	}
	return nil
}

// GetRuntimeSocket returns empty string as CX runtime doesn't use a socket.
func (c *CXRuntime) GetRuntimeSocket() (string, error) {
	return "", nil
}

// GetCooCBindMounts returns empty binds as CX runtime doesn't need them.
func (c *CXRuntime) GetCooCBindMounts() clabtypes.Binds {
	return nil
}

// StreamLogs streams logs from the SimNode pod.
func (c *CXRuntime) StreamLogs(ctx context.Context, containerName string) (io.ReadCloser, error) {
	shortName := extractShortName(containerName)
	podName, err := c.getPodName(ctx, shortName)
	if err != nil {
		return nil, err
	}

	args := []string{
		"-n", c.coreNamespace,
		"logs", "-f", podName,
		"-c", shortName,
	}

	cmd := exec.CommandContext(ctx, "kubectl", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	return stdout, nil
}

// StreamEvents is not fully supported by CX runtime.
func (c *CXRuntime) StreamEvents(ctx context.Context, opts clabruntime.EventStreamOptions) (<-chan clabruntime.ContainerEvent, <-chan error, error) {
	eventCh := make(chan clabruntime.ContainerEvent)
	errCh := make(chan error)

	// Close channels immediately as we don't support event streaming
	close(eventCh)
	close(errCh)

	return eventCh, errCh, nil
}

// InspectImage is not supported by CX runtime.
func (c *CXRuntime) InspectImage(ctx context.Context, imageName string) (*clabruntime.ImageInspect, error) {
	return nil, fmt.Errorf("InspectImage is not supported by CX runtime")
}

// Helper methods

func (c *CXRuntime) buildSimNode(node *clabtypes.NodeConfig) map[string]interface{} {
	labels := map[string]string{
		simTopologyKey: "true",
	}
	annotations := map[string]string{}

	// Add containerlab labels
	// Labels with values containing invalid K8s characters go to annotations instead
	if node.Labels != nil {
		for k, v := range node.Labels {
			if strings.ContainsAny(v, "/\\:") {
				// Put path-like values in annotations (no character restrictions)
				annotations[k] = v
			} else {
				labels[k] = v
			}
		}
	}

	spec := map[string]interface{}{
		"containerImage":   node.Image,
		"operatingSystem":  "linux",
		"dhcp":             map[string]interface{}{},
		"port":             57400,
		"serialNumberPath": "",
		"versionPath":      "",
	}

	// Note: Environment variables are not directly supported in SimNode CRD
	// They need to be handled via deployment patching after creation

	metadata := map[string]interface{}{
		"name":      node.ShortName,
		"namespace": c.topoNamespace,
		"labels":    labels,
	}

	if len(annotations) > 0 {
		metadata["annotations"] = annotations
	}

	return map[string]interface{}{
		"apiVersion": "core.eda.nokia.com/v1",
		"kind":       "SimNode",
		"metadata":   metadata,
		"spec":       spec,
	}
}

func (c *CXRuntime) applyManifest(ctx context.Context, manifest []byte) error {
	cmd := exec.CommandContext(ctx, "kubectl", "apply", "-f", "-")
	cmd.Stdin = bytes.NewReader(manifest)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("kubectl apply failed: %w, output: %s", err, string(output))
	}

	log.Debugf("kubectl apply output: %s", string(output))
	return nil
}

func (c *CXRuntime) waitForSimPod(ctx context.Context, simNodeName string) (string, error) {
	selector := fmt.Sprintf("cx-pod-name=%s", simNodeName)
	deadline := time.Now().Add(3 * time.Minute)

	for time.Now().Before(deadline) {
		args := []string{
			"-n", c.coreNamespace,
			"get", "pod",
			"-l", selector,
			"-o", "jsonpath={.items[0].metadata.name}",
		}

		cmd := exec.CommandContext(ctx, "kubectl", args...)
		output, err := cmd.Output()
		podName := strings.TrimSpace(string(output))

		if err == nil && podName != "" {
			// Wait for pod to be ready
			waitArgs := []string{
				"-n", c.coreNamespace,
				"wait", "pod", podName,
				"--for=condition=Ready",
				"--timeout=120s",
			}

			waitCmd := exec.CommandContext(ctx, "kubectl", waitArgs...)
			if err := waitCmd.Run(); err == nil {
				return podName, nil
			}
		}

		time.Sleep(2 * time.Second)
	}

	return "", fmt.Errorf("timed out waiting for SimNode pod %s", simNodeName)
}

func (c *CXRuntime) getSimNodeIP(ctx context.Context, simNodeName string) (string, error) {
	args := []string{
		"-n", c.topoNamespace,
		"get", "simnode", simNodeName,
		"-o", "jsonpath={.status.ipAddress}",
	}

	cmd := exec.CommandContext(ctx, "kubectl", args...)
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(string(output)), nil
}

func (c *CXRuntime) getSimNodeByName(ctx context.Context, name string) (SimNodeInfo, error) {
	args := []string{
		"-n", c.topoNamespace,
		"get", "simnode", name,
		"-o", "json",
	}

	cmd := exec.CommandContext(ctx, "kubectl", args...)
	output, err := cmd.Output()
	if err != nil {
		return SimNodeInfo{}, fmt.Errorf("SimNode %s not found: %w", name, err)
	}

	var result struct {
		Metadata struct {
			Name        string            `json:"name"`
			Namespace   string            `json:"namespace"`
			Labels      map[string]string `json:"labels"`
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
		Spec struct {
			ContainerImage string `json:"containerImage"`
		} `json:"spec"`
		Status struct {
			Phase     string `json:"phase"`
			IPAddress string `json:"ipAddress"`
		} `json:"status"`
	}

	if err := json.Unmarshal(output, &result); err != nil {
		return SimNodeInfo{}, err
	}

	return SimNodeInfo{
		Name:        result.Metadata.Name,
		Namespace:   result.Metadata.Namespace,
		Image:       result.Spec.ContainerImage,
		Labels:      result.Metadata.Labels,
		Annotations: result.Metadata.Annotations,
		Status:      result.Status.Phase,
		IPAddress:   result.Status.IPAddress,
	}, nil
}

func (c *CXRuntime) getPodName(ctx context.Context, simNodeName string) (string, error) {
	selector := fmt.Sprintf("cx-pod-name=%s", simNodeName)
	args := []string{
		"-n", c.coreNamespace,
		"get", "pod",
		"-l", selector,
		"-o", "jsonpath={.items[0].metadata.name}",
	}

	cmd := exec.CommandContext(ctx, "kubectl", args...)
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get pod for SimNode %s: %w", simNodeName, err)
	}

	podName := strings.TrimSpace(string(output))
	if podName == "" {
		return "", fmt.Errorf("no pod found for SimNode %s", simNodeName)
	}

	return podName, nil
}

func (c *CXRuntime) getPodPID(ctx context.Context, podName string) (int, error) {
	// Get the container ID first
	args := []string{
		"-n", c.coreNamespace,
		"get", "pod", podName,
		"-o", "jsonpath={.status.containerStatuses[0].containerID}",
	}

	cmd := exec.CommandContext(ctx, "kubectl", args...)
	output, err := cmd.Output()
	if err != nil {
		return 0, err
	}

	containerID := strings.TrimSpace(string(output))
	// containerID format: containerd://abc123...
	containerID = strings.TrimPrefix(containerID, "containerd://")
	containerID = strings.TrimPrefix(containerID, "docker://")

	// Use crictl to get the PID
	crictlCmd := exec.CommandContext(ctx, "crictl", "inspect", "--output", "go-template", "--template", "{{.info.pid}}", containerID)
	pidOutput, err := crictlCmd.Output()
	if err != nil {
		// Fallback: try to get PID via kubectl exec
		return 0, fmt.Errorf("failed to get PID for container %s: %w", containerID, err)
	}

	var pid int
	if _, err := fmt.Sscanf(string(pidOutput), "%d", &pid); err != nil {
		return 0, err
	}

	return pid, nil
}

func (c *CXRuntime) buildLabelSelector(gfilters []*clabtypes.GenericFilter) string {
	var selectors []string

	for _, f := range gfilters {
		if f.FilterType == "label" {
			if f.Operator == "exists" {
				selectors = append(selectors, f.Field)
			} else {
				selectors = append(selectors, fmt.Sprintf("%s%s%s", f.Field, f.Operator, f.Match))
			}
		}
	}

	return strings.Join(selectors, ",")
}

// SimNodeInfo represents the relevant fields from a SimNode CRD.
type SimNodeInfo struct {
	Name        string
	Namespace   string
	Image       string
	Labels      map[string]string
	Annotations map[string]string
	Status      string
	IPAddress   string
}

func (c *CXRuntime) getSimNodes(ctx context.Context, labelSelector string) ([]SimNodeInfo, error) {
	args := []string{
		"-n", c.topoNamespace,
		"get", "simnodes",
		"-o", "json",
	}

	if labelSelector != "" {
		args = append(args, "-l", labelSelector)
	}

	cmd := exec.CommandContext(ctx, "kubectl", args...)
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var result struct {
		Items []struct {
			Metadata struct {
				Name        string            `json:"name"`
				Namespace   string            `json:"namespace"`
				Labels      map[string]string `json:"labels"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
			Spec struct {
				ContainerImage string `json:"containerImage"`
			} `json:"spec"`
			Status struct {
				Phase     string `json:"phase"`
				IPAddress string `json:"ipAddress"`
			} `json:"status"`
		} `json:"items"`
	}

	if err := json.Unmarshal(output, &result); err != nil {
		return nil, err
	}

	var simNodes []SimNodeInfo
	for _, item := range result.Items {
		simNodes = append(simNodes, SimNodeInfo{
			Name:        item.Metadata.Name,
			Namespace:   item.Metadata.Namespace,
			Image:       item.Spec.ContainerImage,
			Labels:      item.Metadata.Labels,
			Annotations: item.Metadata.Annotations,
			Status:      item.Status.Phase,
			IPAddress:   item.Status.IPAddress,
		})
	}

	return simNodes, nil
}

func (c *CXRuntime) simNodeToGenericContainer(ctx context.Context, sn SimNodeInfo) (clabruntime.GenericContainer, error) {
	state := "created"
	status := "Created"

	switch strings.ToLower(sn.Status) {
	case "running", "ready":
		state = "running"
		status = "Running"
	case "pending":
		state = "created"
		status = "Pending"
	}

	// Merge labels and annotations (annotations were used for path-like values)
	// Containerlab expects all metadata as labels
	labels := make(map[string]string)
	for k, v := range sn.Labels {
		labels[k] = v
	}
	for k, v := range sn.Annotations {
		labels[k] = v
	}

	container := clabruntime.GenericContainer{
		Names:       []string{sn.Name},
		ID:          sn.Name,
		ShortID:     sn.Name,
		Image:       sn.Image,
		State:       state,
		Status:      status,
		Labels:      labels,
		NetworkName: c.topoNamespace,
		NetworkSettings: clabruntime.GenericMgmtIPs{
			IPv4addr: sn.IPAddress,
			IPv4pLen: 32,
		},
	}

	// Try to get the pod PID
	podName, err := c.getPodName(ctx, sn.Name)
	if err == nil {
		pid, err := c.getPodPID(ctx, podName)
		if err == nil {
			container.Pid = pid
		}
	}

	return container, nil
}
