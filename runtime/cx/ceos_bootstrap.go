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
	"time"

	"github.com/charmbracelet/log"
	"sigs.k8s.io/yaml"
)

// ceosIfWaitScript is the script that maintains network connectivity for cEOS containers.
// It works around the issue where EOS systemd units reset Linux networking during boot,
// causing the CNI routes to disappear.
const ceosIfWaitScript = `#!/bin/bash
set -euo pipefail

log() {
  echo "$(date -Iseconds) [if-wait] $*"
}

IFACE="${MGMT_INTF:-eth0}"
READY_FLAG="/run/ceos-if-wait.ready"

log "waiting for interface ${IFACE}"
for _ in $(seq 1 60); do
  if ip link show "$IFACE" >/dev/null 2>&1; then
    break
  fi
  sleep 1
done

get_pod_cidr() {
  ip -o -4 addr show "$IFACE" 2>/dev/null | awk '{print $4; exit}'
}

POD_CIDR="$(get_pod_cidr)"
ATTEMPTS=0
while [ -z "$POD_CIDR" ] && [ $ATTEMPTS -lt 30 ]; do
  sleep 1
  POD_CIDR="$(get_pod_cidr)"
  ATTEMPTS=$((ATTEMPTS+1))
done

if [ -n "$POD_CIDR" ]; then
  POD_IP="${POD_CIDR%/*}"
  POD_PREFIX="${POD_CIDR#*/}"
else
  POD_IP="${POD_IP:-}"
  POD_PREFIX="${POD_PREFIX:-24}"
  POD_CIDR="${POD_IP}/${POD_PREFIX}"
fi

mapfile -t ORIG_ROUTES < <(ip route show table main)

POD_GW="$(ip route show default 2>/dev/null | awk '/default/ {print $3; exit}')"
if [ -z "$POD_GW" ]; then
  POD_GW="${POD_GATEWAY:-}"
fi

apply_routes() {
  if [ ${#ORIG_ROUTES[@]} -gt 0 ]; then
    for entry in "${ORIG_ROUTES[@]}"; do
      ip route replace $entry || true
    done
  fi
}

apply_net() {
  if [ -n "$POD_CIDR" ]; then
    log "ensuring ${IFACE} has ${POD_CIDR}"
    ip addr replace "$POD_CIDR" dev "$IFACE"
  fi
  sysctl -w "net.ipv4.conf.${IFACE}.arp_ignore=0" >/dev/null
  sysctl -w "net.ipv4.conf.${IFACE}.rp_filter=0" >/dev/null
  sysctl -w "net.ipv4.conf.${IFACE}.accept_local=1" >/dev/null
  apply_routes
}

apply_net
touch "$READY_FLAG"

while true; do
  sleep 5
  ip -o -4 addr show "$IFACE" | grep -q "$POD_CIDR" || apply_net
done
`

// ceosCommand is the command used to start cEOS with the if-wait helper
const ceosCommand = `bash -c 'rm -f /run/ceos-if-wait.ready; setsid /mnt/flash/if-wait.sh >/var/log/if-wait.log 2>&1 & while [ ! -f /run/ceos-if-wait.ready ]; do sleep 1; done; exec /sbin/init systemd.setenv=CEOS=1 systemd.setenv=EOS_PLATFORM=ceoslab systemd.setenv=container=docker systemd.setenv=ETBA=1 systemd.setenv=SKIP_ZEROTOUCH_BARRIER_IN_SYSDBINIT=1 systemd.setenv=INTFTYPE=eth systemd.setenv=MAPETH0=1 systemd.setenv=MGMT_INTF=eth0'`

// ceosEnvVars are the environment variables needed for cEOS
var ceosEnvVars = map[string]string{
	"CEOS":                              "1",
	"EOS_PLATFORM":                      "ceoslab",
	"container":                         "docker",
	"ETBA":                              "1",
	"SKIP_ZEROTOUCH_BARRIER_IN_SYSDBINIT": "1",
	"INTFTYPE":                          "eth",
	"MAPETH0":                           "1",
	"MGMT_INTF":                         "eth0",
}

// IsCeosImage checks if the given image is a cEOS image
func IsCeosImage(image string) bool {
	imageLower := strings.ToLower(image)
	return strings.Contains(imageLower, "ceos") ||
		strings.Contains(imageLower, "arista")
}

// BootstrapCeos performs the necessary setup for cEOS containers in EDA.
// This includes:
// 1. Creating the if-wait ConfigMap
// 2. Waiting for the deployment to be created by the SimNode controller
// 3. Patching the deployment with the proper command, volumes, and env vars
// 4. Waiting for the pod to be ready
// 5. Configuring admin credentials
// 6. Restarting linked peer pods so their cxdp reconnects
func (c *CXRuntime) BootstrapCeos(ctx context.Context, simNodeName string) error {
	log.Infof("Bootstrapping cEOS for SimNode %s", simNodeName)

	deploymentName := fmt.Sprintf("cx-eda--%s-sim", simNodeName)

	// Step 1: Ensure the if-wait ConfigMap exists
	if err := c.ensureCeosConfigMap(ctx); err != nil {
		return fmt.Errorf("failed to create cEOS ConfigMap: %w", err)
	}

	// Step 2: Wait for the deployment to be created by the SimNode controller
	if err := c.waitForDeploymentExists(ctx, deploymentName); err != nil {
		return fmt.Errorf("failed waiting for deployment to exist: %w", err)
	}

	// Step 3: Patch the deployment
	if err := c.patchCeosDeployment(ctx, deploymentName, simNodeName); err != nil {
		return fmt.Errorf("failed to patch cEOS deployment: %w", err)
	}

	// Step 4: Wait for rollout
	if err := c.waitForDeploymentRollout(ctx, deploymentName); err != nil {
		return fmt.Errorf("failed to wait for deployment rollout: %w", err)
	}

	// Step 5: Configure admin credentials
	if err := c.configureCeosAdmin(ctx, simNodeName); err != nil {
		return fmt.Errorf("failed to configure cEOS admin: %w", err)
	}

	// Step 6: Configure iptables to allow cxdp IPC ports
	// cEOS has strict iptables rules that block ports 50123 and 50128 by default
	if err := c.configureCeosIptables(ctx, simNodeName); err != nil {
		log.Warnf("Failed to configure cEOS iptables: %v", err)
		// Don't fail - try to continue
	}

	// Step 7: Restart linked peer pods so their cxdp can reconnect
	// The cEOS bootstrap causes the pod to restart, which breaks existing
	// cxdp connections. We need to restart peers so they reconnect.
	if err := c.restartLinkedPeers(ctx, simNodeName); err != nil {
		log.Warnf("Failed to restart linked peers for %s: %v", simNodeName, err)
		// Don't fail the bootstrap - connectivity might still work
	}

	// Step 8: Ensure dataplane interfaces are up on all linked nodes
	// After pod restarts, the -cx veth interfaces may be DOWN
	if err := c.ensureDataplaneInterfacesUp(ctx, simNodeName); err != nil {
		log.Warnf("Failed to bring up dataplane interfaces: %v", err)
	}

	log.Infof("cEOS bootstrap completed for %s", simNodeName)
	return nil
}

func (c *CXRuntime) ensureCeosConfigMap(ctx context.Context) error {
	log.Debug("Ensuring cEOS if-wait ConfigMap exists")

	configMap := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]interface{}{
			"name":      "ceos-if-wait",
			"namespace": c.coreNamespace,
		},
		"data": map[string]interface{}{
			"if-wait.sh": ceosIfWaitScript,
		},
	}

	manifest, err := yaml.Marshal(configMap)
	if err != nil {
		return err
	}

	cmd := exec.CommandContext(ctx, "kubectl", "apply", "-f", "-")
	cmd.Stdin = bytes.NewReader(manifest)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to apply ConfigMap: %w, output: %s", err, string(output))
	}

	log.Debugf("ConfigMap apply output: %s", string(output))
	return nil
}

func (c *CXRuntime) patchCeosDeployment(ctx context.Context, deploymentName, containerName string) error {
	log.Debugf("Patching cEOS deployment %s", deploymentName)

	// Check if volume already exists
	checkVolumeCmd := exec.CommandContext(ctx, "kubectl",
		"get", "deployment", deploymentName,
		"-n", c.coreNamespace,
		"-o", "jsonpath={range .spec.template.spec.volumes[*]}{.name} {end}",
	)
	volumeOutput, _ := checkVolumeCmd.Output()
	hasVolume := strings.Contains(string(volumeOutput), "ceos-if-wait")

	// Add volume if not present
	if !hasVolume {
		volumePatch := `[{"op":"add","path":"/spec/template/spec/volumes/-","value":{"name":"ceos-if-wait","configMap":{"name":"ceos-if-wait","defaultMode":493}}}]`
		if err := c.patchDeployment(ctx, deploymentName, volumePatch); err != nil {
			return fmt.Errorf("failed to add volume: %w", err)
		}
	}

	// Check if volume mount already exists
	checkMountCmd := exec.CommandContext(ctx, "kubectl",
		"get", "deployment", deploymentName,
		"-n", c.coreNamespace,
		"-o", fmt.Sprintf("jsonpath={range .spec.template.spec.containers[?(@.name==\"%s\")].volumeMounts[*]}{.name} {end}", containerName),
	)
	mountOutput, _ := checkMountCmd.Output()
	hasMount := strings.Contains(string(mountOutput), "ceos-if-wait")

	// Add volume mount if not present
	if !hasMount {
		mountPatch := `[{"op":"add","path":"/spec/template/spec/containers/0/volumeMounts/-","value":{"name":"ceos-if-wait","mountPath":"/mnt/flash/if-wait.sh","subPath":"if-wait.sh"}}]`
		if err := c.patchDeployment(ctx, deploymentName, mountPatch); err != nil {
			return fmt.Errorf("failed to add volume mount: %w", err)
		}
	}

	// Set the command
	commandPatch := fmt.Sprintf(`[{"op":"replace","path":"/spec/template/spec/containers/0/command","value":["bash","-c","rm -f /run/ceos-if-wait.ready; setsid /mnt/flash/if-wait.sh >/var/log/if-wait.log 2>&1 & while [ ! -f /run/ceos-if-wait.ready ]; do sleep 1; done; exec /sbin/init systemd.setenv=CEOS=1 systemd.setenv=EOS_PLATFORM=ceoslab systemd.setenv=container=docker systemd.setenv=ETBA=1 systemd.setenv=SKIP_ZEROTOUCH_BARRIER_IN_SYSDBINIT=1 systemd.setenv=INTFTYPE=eth systemd.setenv=MAPETH0=1 systemd.setenv=MGMT_INTF=eth0"]}]`)
	if err := c.patchDeployment(ctx, deploymentName, commandPatch); err != nil {
		return fmt.Errorf("failed to set command: %w", err)
	}

	// Set environment variables
	envArgs := []string{
		"set", "env", "deployment", deploymentName,
		"-n", c.coreNamespace,
		fmt.Sprintf("--containers=%s", containerName),
		"--overwrite",
	}
	for k, v := range ceosEnvVars {
		envArgs = append(envArgs, fmt.Sprintf("%s=%s", k, v))
	}

	envCmd := exec.CommandContext(ctx, "kubectl", envArgs...)
	if output, err := envCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to set env vars: %w, output: %s", err, string(output))
	}

	return nil
}

func (c *CXRuntime) patchDeployment(ctx context.Context, deploymentName, patch string) error {
	cmd := exec.CommandContext(ctx, "kubectl",
		"patch", "deployment", deploymentName,
		"-n", c.coreNamespace,
		"--type=json",
		"-p", patch,
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("patch failed: %w, output: %s", err, string(output))
	}

	log.Debugf("Patch output: %s", string(output))
	return nil
}

func (c *CXRuntime) waitForDeploymentExists(ctx context.Context, deploymentName string) error {
	log.Debugf("Waiting for deployment %s to be created", deploymentName)

	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		cmd := exec.CommandContext(ctx, "kubectl",
			"get", "deployment", deploymentName,
			"-n", c.coreNamespace,
			"-o", "name",
		)

		if output, err := cmd.CombinedOutput(); err == nil {
			log.Debugf("Deployment found: %s", strings.TrimSpace(string(output)))
			return nil
		}

		time.Sleep(2 * time.Second)
	}

	return fmt.Errorf("timed out waiting for deployment %s to be created", deploymentName)
}

func (c *CXRuntime) waitForDeploymentRollout(ctx context.Context, deploymentName string) error {
	log.Debugf("Waiting for deployment %s rollout", deploymentName)

	cmd := exec.CommandContext(ctx, "kubectl",
		"rollout", "status", "deployment", deploymentName,
		"-n", c.coreNamespace,
		"--timeout=300s",
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("rollout status failed: %w, output: %s", err, string(output))
	}

	log.Debugf("Rollout status: %s", string(output))
	return nil
}

func (c *CXRuntime) configureCeosAdmin(ctx context.Context, simNodeName string) error {
	log.Debugf("Configuring admin credentials for %s", simNodeName)

	podName, err := c.getPodName(ctx, simNodeName)
	if err != nil {
		return err
	}

	// Wait for EOS to be ready and configure admin
	fastCliConfig := `enable
configure terminal
username admin privilege 15 secret admin
management ssh
end
write memory
`

	deadline := time.Now().Add(150 * time.Second)
	for time.Now().Before(deadline) {
		cmd := exec.CommandContext(ctx, "kubectl",
			"-n", c.coreNamespace,
			"exec", podName,
			"-c", simNodeName,
			"--",
			"bash", "-lc", fmt.Sprintf("FastCli -p 15 <<'__FCLI__'\n%s__FCLI__\n", fastCliConfig),
		)

		output, err := cmd.CombinedOutput()
		if err == nil {
			log.Debugf("FastCli output: %s", string(output))
			return nil
		}

		log.Debugf("FastCli attempt failed (retrying): %v, output: %s", err, string(output))
		time.Sleep(10 * time.Second)
	}

	return fmt.Errorf("timed out configuring cEOS admin credentials")
}

// configureCeosIptables adds iptables rules to allow cxdp IPC ports.
// cEOS has strict iptables rules with default DROP policy that block the
// cxdp inter-node communication ports (50123 and 50128).
func (c *CXRuntime) configureCeosIptables(ctx context.Context, simNodeName string) error {
	log.Debugf("Configuring iptables for cxdp IPC ports on %s", simNodeName)

	podName, err := c.getPodName(ctx, simNodeName)
	if err != nil {
		return err
	}

	// Ports used by cxdp for inter-node communication
	cxdpPorts := []string{"50123", "50128"}

	for _, port := range cxdpPorts {
		cmd := exec.CommandContext(ctx, "kubectl",
			"-n", c.coreNamespace,
			"exec", podName,
			"-c", simNodeName,
			"--",
			"iptables", "-I", "EOS_INPUT", "1", "-p", "tcp", "--dport", port, "-j", "ACCEPT",
		)

		if output, err := cmd.CombinedOutput(); err != nil {
			log.Warnf("Failed to add iptables rule for port %s: %v, output: %s", port, err, string(output))
			// Continue trying other ports
		} else {
			log.Debugf("Added iptables rule to allow TCP port %s", port)
		}
	}

	return nil
}

// restartLinkedPeers finds all SimLinks involving the given node and restarts
// the peer deployments so their cxdp can establish new connections.
func (c *CXRuntime) restartLinkedPeers(ctx context.Context, nodeName string) error {
	log.Debugf("Finding linked peers for %s", nodeName)

	// Get all SimLinks in the namespace
	cmd := exec.CommandContext(ctx, "kubectl",
		"get", "simlinks",
		"-n", c.topoNamespace,
		"-o", "jsonpath={range .items[*]}{.metadata.name},{.spec.links[0].local.node},{.spec.links[0].sim.node}{\"\\n\"}{end}",
	)

	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("failed to get SimLinks: %w", err)
	}

	// Find peers linked to this node
	peers := make(map[string]bool)
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) != 3 {
			continue
		}
		localNode := parts[1]
		simNode := parts[2]

		if localNode == nodeName && simNode != nodeName {
			peers[simNode] = true
		}
		if simNode == nodeName && localNode != nodeName {
			peers[localNode] = true
		}
	}

	if len(peers) == 0 {
		log.Debug("No linked peers found")
		return nil
	}

	// Restart each peer deployment
	timestamp := time.Now().Format(time.RFC3339)
	for peer := range peers {
		log.Infof("Restarting linked peer deployment: %s", peer)
		deploymentName := fmt.Sprintf("cx-eda--%s-sim", peer)

		// Use annotation patch to trigger pod restart
		annotationPatch := fmt.Sprintf(
			`{"spec":{"template":{"metadata":{"annotations":{"clab-restart-trigger":"%s"}}}}}`,
			timestamp,
		)

		restartCmd := exec.CommandContext(ctx, "kubectl",
			"patch", "deployment", deploymentName,
			"-n", c.coreNamespace,
			"--type=merge",
			"-p", annotationPatch,
		)

		if output, err := restartCmd.CombinedOutput(); err != nil {
			log.Warnf("Failed to restart peer %s: %v, output: %s", peer, err, string(output))
			continue
		}

		// Wait for the deployment to roll out
		waitCmd := exec.CommandContext(ctx, "kubectl",
			"rollout", "status", "deployment", deploymentName,
			"-n", c.coreNamespace,
			"--timeout=120s",
		)
		if output, err := waitCmd.CombinedOutput(); err != nil {
			log.Warnf("Failed to wait for peer %s rollout: %v, output: %s", peer, err, string(output))
		}
	}

	return nil
}

// ensureDataplaneInterfacesUp brings up the -cx veth interfaces on all linked nodes.
// After pod restarts, these interfaces may be in DOWN state which breaks connectivity.
func (c *CXRuntime) ensureDataplaneInterfacesUp(ctx context.Context, nodeName string) error {
	log.Infof("Ensuring dataplane interfaces are up for %s and its peers", nodeName)

	// Wait a bit for cxdp to create the interfaces after pod restart
	time.Sleep(5 * time.Second)

	// Get all SimLinks in the namespace
	cmd := exec.CommandContext(ctx, "kubectl",
		"get", "simlinks",
		"-n", c.topoNamespace,
		"-o", "jsonpath={range .items[*]}{.metadata.name},{.spec.links[0].local.node},{.spec.links[0].local.interface},{.spec.links[0].sim.node},{.spec.links[0].sim.interface}{\"\\n\"}{end}",
	)

	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("failed to get SimLinks: %w", err)
	}

	// Process each link involving this node
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) != 5 {
			continue
		}
		localNode := parts[1]
		localIface := parts[2]
		simNode := parts[3]
		simIface := parts[4]

		// Check if this link involves our node
		if localNode != nodeName && simNode != nodeName {
			continue
		}

		// Bring up interfaces on both sides of the link
		// Local side
		if err := c.bringUpCxInterface(ctx, localNode, localIface); err != nil {
			log.Warnf("Failed to bring up %s:%s-cx: %v", localNode, localIface, err)
		}

		// Sim side
		if err := c.bringUpCxInterface(ctx, simNode, simIface); err != nil {
			log.Warnf("Failed to bring up %s:%s-cx: %v", simNode, simIface, err)
		}
	}

	return nil
}

// bringUpCxInterface brings up both the main interface and the -cx veth interface
func (c *CXRuntime) bringUpCxInterface(ctx context.Context, nodeName, ifaceName string) error {
	// Get the pod name for this node
	podName, err := c.getPodNameForNode(ctx, nodeName)
	if err != nil {
		return fmt.Errorf("failed to get pod for node %s: %w", nodeName, err)
	}

	// Interface names
	cxIfaceName := ifaceName + "-cx"

	log.Debugf("Bringing up interfaces %s and %s on pod %s", ifaceName, cxIfaceName, podName)

	// Bring up the main interface first
	bringUpMainCmd := exec.CommandContext(ctx, "kubectl",
		"exec", "-n", c.coreNamespace, podName,
		"-c", nodeName,
		"--", "ip", "link", "set", ifaceName, "up",
	)
	if output, err := bringUpMainCmd.CombinedOutput(); err != nil {
		log.Debugf("Failed to bring up %s (may not exist yet): %v, output: %s", ifaceName, err, string(output))
	}

	// Then bring up the -cx interface
	bringUpCxCmd := exec.CommandContext(ctx, "kubectl",
		"exec", "-n", c.coreNamespace, podName,
		"-c", nodeName,
		"--", "ip", "link", "set", cxIfaceName, "up",
	)
	if output, err := bringUpCxCmd.CombinedOutput(); err != nil {
		log.Debugf("Failed to bring up %s (may not exist yet): %v, output: %s", cxIfaceName, err, string(output))
	}

	return nil
}

// getPodNameForNode gets the pod name for a given SimNode
func (c *CXRuntime) getPodNameForNode(ctx context.Context, nodeName string) (string, error) {
	selector := fmt.Sprintf("cx-pod-name=%s", nodeName)
	cmd := exec.CommandContext(ctx, "kubectl",
		"get", "pods",
		"-n", c.coreNamespace,
		"-l", selector,
		"-o", "jsonpath={.items[0].metadata.name}",
	)

	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get pod for node %s: %w", nodeName, err)
	}

	podName := strings.TrimSpace(string(output))
	if podName == "" {
		return "", fmt.Errorf("no pod found for node %s", nodeName)
	}

	return podName, nil
}
