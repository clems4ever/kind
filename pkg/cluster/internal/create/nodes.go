/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package create

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/pkg/errors"

	"sigs.k8s.io/kind/pkg/cluster/config"
	"sigs.k8s.io/kind/pkg/cluster/constants"
	"sigs.k8s.io/kind/pkg/cluster/nodes"
	"sigs.k8s.io/kind/pkg/container/cri"
	logutil "sigs.k8s.io/kind/pkg/log"
)

// provisioning order for nodes by role
var defaultRoleOrder = []string{
	constants.ExternalLoadBalancerNodeRoleValue,
	constants.ExternalEtcdNodeRoleValue,
	constants.ControlPlaneNodeRoleValue,
	constants.WorkerNodeRoleValue,
}

// sorts nodes for provisioning
func sortNodes(nodes []config.Node, roleOrder []string) {
	roleToOrder := makeRoleToOrder(roleOrder)
	sort.SliceStable(nodes, func(i, j int) bool {
		return roleToOrder(string(nodes[i].Role)) < roleToOrder(string(nodes[j].Role))
	})
}

// helper to convert an ordered slice of roles to a mapping of provisioning
// role to provisioning order
func makeRoleToOrder(roleOrder []string) func(string) int {
	orderMap := make(map[string]int)
	for i, role := range roleOrder {
		orderMap[role] = i
	}
	return func(role string) int {
		p, ok := orderMap[role]
		if !ok {
			return 10000
		}
		return p
	}
}

// TODO(bentheelder): eliminate this when we have v1alpha3
func convertReplicas(nodes []config.Node) []config.Node {
	out := []config.Node{}
	for _, node := range nodes {
		replicas := int32(1)
		if node.Replicas != nil {
			replicas = *node.Replicas
		}
		for i := int32(0); i < replicas; i++ {
			outNode := node.DeepCopy()
			outNode.Replicas = nil
			out = append(out, *outNode)
		}
	}
	return out
}

// provisionNodes takes care of creating all the containers
// that will host `kind` nodes
func provisionNodes(
	status *logutil.Status, cfg *config.Config, clusterName, clusterLabel string,
) error {
	defer status.End(false)

	_, err := createNodeContainers(status, cfg, clusterName, clusterLabel)
	if err != nil {
		return err
	}

	status.End(true)
	return nil
}

func createNodeContainers(
	status *logutil.Status, cfg *config.Config, clusterName, clusterLabel string,
) ([]nodes.Node, error) {
	defer status.End(false)

	// create all of the node containers, concurrently
	desiredNodes := nodesToCreate(cfg, clusterName)
	status.Start("Preparing nodes " + strings.Repeat("📦", len(desiredNodes)))
	nodeChan := make(chan *nodes.Node, len(desiredNodes))
	errChan := make(chan error)
	defer close(nodeChan)
	defer close(errChan)
	for _, desiredNode := range desiredNodes {
		desiredNode := desiredNode // capture loop variable
		go func() {
			// create the node into a container (docker run, but it is paused, see createNode)
			node, err := desiredNode.Create(clusterLabel)
			if err != nil {
				errChan <- err
				return
			}
			err = fixupNode(node)
			if err != nil {
				errChan <- err
				return
			}
			nodeChan <- node
		}()
	}

	// collect nodes
	allNodes := []nodes.Node{}
	for {
		select {
		case node := <-nodeChan:
			// TODO(bentheelder): nodes should maybe not be pointers /shrug
			allNodes = append(allNodes, *node)
			if len(allNodes) == len(desiredNodes) {
				status.End(true)
				return allNodes, nil
			}
		case err := <-errChan:
			return nil, err
		}
	}
}

func fixupNode(node *nodes.Node) error {
	// we need to change a few mounts once we have the container
	// we'd do this ahead of time if we could, but --privileged implies things
	// that don't seem to be configurable, and we need that flag
	if err := node.FixMounts(); err != nil {
		// TODO(bentheelder): logging here
		return err
	}

	if nodes.NeedProxy() {
		if err := node.SetProxy(); err != nil {
			// TODO: logging here
			return errors.Wrapf(err, "failed to set proxy for node %s", node.Name())
		}
	}

	// signal the node container entrypoint to continue booting into systemd
	if err := node.SignalStart(); err != nil {
		// TODO(bentheelder): logging here
		return err
	}

	// wait for docker to be ready
	if !node.WaitForDocker(time.Now().Add(time.Second * 30)) {
		// TODO(bentheelder): logging here
		return errors.Errorf("timed out waiting for docker to be ready on node %s", node.Name())
	}

	// load the docker image artifacts into the docker daemon
	node.LoadImages()

	return nil
}

// nodeSpec describes a node to create purely from the container aspect
// this does not inlude eg starting kubernetes (see actions for that)
type nodeSpec struct {
	Name        string
	Role        string
	Image       string
	ExtraMounts []cri.Mount
}

func nodesToCreate(cfg *config.Config, clusterName string) []nodeSpec {
	desiredNodes := []nodeSpec{}

	// nodes are named based on the cluster name and their role, with a counter
	nameNode := makeNodeNamer(clusterName)

	// convert replicas to normal nodes
	// TODO(bentheelder): eliminate this when we have v1alpha3 ?
	configNodes := convertReplicas(cfg.Nodes)

	// TODO(bentheelder): allow overriding defaultRoleOrder
	sortNodes(configNodes, defaultRoleOrder)

	for _, configNode := range configNodes {
		role := string(configNode.Role)
		desiredNodes = append(desiredNodes, nodeSpec{
			Name:        nameNode(role),
			Image:       configNode.Image,
			Role:        role,
			ExtraMounts: configNode.ExtraMounts,
		})
	}

	// TODO(bentheelder): handle implicit nodes as well

	return desiredNodes
}

func (d *nodeSpec) Create(clusterLabel string) (node *nodes.Node, err error) {
	// create the node into a container (docker run, but it is paused, see createNode)
	// TODO(bentheelder): decouple from config objects further
	switch d.Role {
	case constants.ExternalLoadBalancerNodeRoleValue:
		node, err = nodes.CreateExternalLoadBalancerNode(d.Name, d.Image, clusterLabel)
	case constants.ControlPlaneNodeRoleValue:
		node, err = nodes.CreateControlPlaneNode(d.Name, d.Image, clusterLabel, d.ExtraMounts)
	case constants.WorkerNodeRoleValue:
		node, err = nodes.CreateWorkerNode(d.Name, d.Image, clusterLabel, d.ExtraMounts)
	default:
		return nil, errors.Errorf("unknown node role: %s", d.Role)
	}
	return node, err
}

// makeNodeNamer returns a func(role string)(nodeName string)
// used to name nodes based on their role and the clusterName
func makeNodeNamer(clusterName string) func(string) string {
	counter := make(map[string]int)
	return func(role string) string {
		count := 1
		suffix := ""
		if v, ok := counter[role]; ok {
			count += v
			suffix = fmt.Sprintf("%d", count)
		}
		counter[role] = count
		return fmt.Sprintf("%s-%s%s", clusterName, role, suffix)
	}
}
