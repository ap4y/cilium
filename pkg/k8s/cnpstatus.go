// Copyright 2019 Authors of Cilium
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	cilium_v2 "github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2"
	"github.com/cilium/cilium/pkg/k8s/types"
	k8sversion "github.com/cilium/cilium/pkg/k8s/version"
	"github.com/cilium/cilium/pkg/kvstore"
	"github.com/cilium/cilium/pkg/kvstore/store"
	"github.com/cilium/cilium/pkg/lock"
	"github.com/cilium/cilium/pkg/logging/logfields"
	"github.com/sirupsen/logrus"

	metaV1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
)

// CNPStatusEventHandler handles status updates events for all CNPs in the
// cluster. Upon creation of CNPs, it will start a controller for that CNP which
// handles sending of updates for that CNP to the Kubernetes API server. Upon
// receiving events from the key-value store, it will send the update for the
// CNP corresponding to the status update to the controller for that CNP.
type CNPStatusEventHandler struct {
	eventMap       *cnpEventMap
	cnpStore       *store.SharedStore
	k8sStore       cache.Store
	updateInterval time.Duration
}

// NodeStatusUpdater handles the lifecycle around sending CNP NodeStatus updates.
type NodeStatusUpdater struct {
	updateChan chan *NodeStatusUpdate
	stopChan   chan struct{}
}

type cnpEventMap struct {
	lock.RWMutex
	eventMap map[string]*NodeStatusUpdater
}

func newCNPEventMap() *cnpEventMap {
	return &cnpEventMap{
		eventMap: make(map[string]*NodeStatusUpdater),
	}
}

func (c *cnpEventMap) lookup(cnpKey string) (*NodeStatusUpdater, bool) {
	c.RLock()
	ch, ok := c.eventMap[cnpKey]
	c.RUnlock()
	return ch, ok
}

func (c *cnpEventMap) createIfNotExist(cnpKey string) (*NodeStatusUpdater, bool) {
	c.Lock()
	defer c.Unlock()
	nsu, ok := c.eventMap[cnpKey]
	// Cannot reinsert into map when active channel present.
	if ok {
		return nsu, ok
	}
	nsu = &NodeStatusUpdater{
		updateChan: make(chan *NodeStatusUpdate, 512),
		stopChan:   make(chan struct{}),
	}
	c.eventMap[cnpKey] = nsu
	return nsu, ok
}

func (c *cnpEventMap) delete(cnpKey string) {
	c.Lock()
	defer c.Unlock()
	nsu, ok := c.eventMap[cnpKey]
	if !ok {
		return
	}
	// Signal that we should stop processing events.
	close(nsu.stopChan)
	delete(c.eventMap, cnpKey)
}

// NewCNPStatusEventHandler returns a new CNPStatusEventHandler.
func NewCNPStatusEventHandler(cnpStore *store.SharedStore, k8sStore cache.Store, updateInterval time.Duration) *CNPStatusEventHandler {
	return &CNPStatusEventHandler{
		eventMap:       newCNPEventMap(),
		cnpStore:       cnpStore,
		k8sStore:       k8sStore,
		updateInterval: updateInterval,
	}
}

// NodeStatusUpdate pairs a CiliumNetworkPolicyNodeStatus to a specific node.
type NodeStatusUpdate struct {
	node string
	*cilium_v2.CiliumNetworkPolicyNodeStatus
}

// WatchForCNPStatusEvents starts a watcher for all the CNP update from the
// key-value store.
func (c *CNPStatusEventHandler) WatchForCNPStatusEvents() {
	watcher := kvstore.Client().ListAndWatch(context.TODO(), "cnpStatusWatcher", CNPStatusesPath, 512)

	// Loop and block for the watcher
	for {
		c.watchForCNPStatusEvents(watcher)
	}
}

// watchForCNPStatusEvents starts responds to the events from the watcher of
// the key-value store.
func (c *CNPStatusEventHandler) watchForCNPStatusEvents(watcher *kvstore.Watcher) {
	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				log.Debugf("%s closed, restarting watch", watcher.String())
				time.Sleep(500 * time.Millisecond)
				return
			}

			switch event.Typ {
			case kvstore.EventTypeListDone, kvstore.EventTypeDelete:
			case kvstore.EventTypeCreate, kvstore.EventTypeModify:
				var cnpStatusUpdate CNPNSWithMeta
				err := json.Unmarshal(event.Value, &cnpStatusUpdate)
				if err != nil {
					log.WithFields(logrus.Fields{"kvstore-event": event.Typ.String(), "key": event.Key}).
						WithError(err).Error("Not updating CNP Status; error unmarshaling data from key-value store")
					continue
				}

				log.WithFields(logrus.Fields{
					"uid":       cnpStatusUpdate.UID,
					"name":      cnpStatusUpdate.Name,
					"namespace": cnpStatusUpdate.Namespace,
					"node":      cnpStatusUpdate.Node,
					"key":       event.Key,
					"type":      event.Typ,
				}).Debug("received event from kvstore")

				// Send the update to the corresponding controller for the
				// CNP which sends all status updates to the K8s apiserver.
				// If the namespace is empty for the status update then the cnpKey
				// will correspond to the ccnpKey.
				cnpKey := generateCNPKey(string(cnpStatusUpdate.UID), cnpStatusUpdate.Namespace, cnpStatusUpdate.Name)
				updater, ok := c.eventMap.lookup(cnpKey)
				if !ok {
					log.WithField("cnp", cnpKey).Debug("received event from kvstore for cnp for which we do not have any updater goroutine")
					continue
				}
				nsu := &NodeStatusUpdate{node: cnpStatusUpdate.Node}
				nsu.CiliumNetworkPolicyNodeStatus = &(cnpStatusUpdate.CiliumNetworkPolicyNodeStatus)

				// Given that select is not deterministic, ensure that we check
				// for shutdown first. If not shut down, then try to send on
				// channel, or wait for shutdown so that we don't block forever
				// in case the channel is full and the updater is stopped.
				select {
				case <-updater.stopChan:
					// This goroutine is the only sender on this channel; we can
					// close safely if the stop channel is closed.
					close(updater.updateChan)

				default:
					select {
					// If the update is sent and we shut down after, the event
					// is 'lost'; we don't care because this means the CNP
					// was deleted anyway.
					case updater.updateChan <- nsu:
					case <-updater.stopChan:
						// This goroutine is the only sender on this channel; we can
						// close safely if the stop channel is closed.
						close(updater.updateChan)
					}
				}
			}
		}
	}
}

func (c *CNPStatusEventHandler) stopStatusHandler(cnp *types.SlimCNP, cnpKey, prefix string) {
	err := kvstore.DeletePrefix(context.TODO(), prefix)
	if err != nil {
		log.WithError(err).WithField("prefix", prefix).Warning("error deleting prefix from kvstore")
	}
	c.eventMap.delete(cnpKey)
}

// StopStatusHandler signals that we need to stop managing the sending of
// status updates to the Kubernetes APIServer for the given CNP. It also cleans
// up all status updates from the key-value store for this CNP.
func (c *CNPStatusEventHandler) StopStatusHandler(cnp *types.SlimCNP) {
	cnpKey := getKeyFromObjectMeta(cnp.ObjectMeta)
	prefix := path.Join(CNPStatusesPath, cnpKey)

	c.stopStatusHandler(cnp, cnpKey, prefix)
}

func (c *CNPStatusEventHandler) runStatusHandler(cnpKey string, cnp *types.SlimCNP, nodeStatusUpdater *NodeStatusUpdater) {
	namespace := cnp.Namespace
	name := cnp.Name
	nodeStatusMap := make(map[string]cilium_v2.CiliumNetworkPolicyNodeStatus)

	scopedLog := log.WithFields(logrus.Fields{
		logfields.K8sNamespace:            namespace,
		logfields.CiliumNetworkPolicyName: name,
	})

	scopedLog.Debug("started status handler")

	// Iterate over the shared-store first. We may have received events for this
	// CNP in the key-value store from nodes which received and processed this
	// CNP and sent status updates for it before the watcher which updates this
	// `CNPStatusEventHandler` did. Given that we have the shared store which
	// caches all keys / values from the kvstore, we iterate and collect said
	// events. Given that this function is called after we have updated the
	// `eventMap` for this `CNPStatusEventHandler`, subsequent key updates from
	// the kvstore are guaranteed to be sent on the channel in the
	// `nodeStatusUpdater`, which we will receive in the for-loop below.
	sharedKeys := c.cnpStore.SharedKeysMap()
	for keyName, storeKey := range sharedKeys {
		// Look for any key which matches this CNP.
		if strings.HasPrefix(keyName, cnpKey) {
			cnpns, ok := storeKey.(*CNPNSWithMeta)
			if !ok {
				scopedLog.Errorf("received unexpected type mapping to key %s in cnp shared store: %T", keyName, storeKey)
				continue
			}
			// extract nodeName from keyName
			nodeStatusMap[cnpns.Node] = cnpns.CiliumNetworkPolicyNodeStatus
		}
	}
	for {
		// Allow for a bunch of different node status updates to come before
		// we break out to avoid jitter in updates across the cluster
		// to affect batching on our end.
		limit := time.After(c.updateInterval)

		// Collect any other events that have come in, but bail out after the
		// above limit is hit so that we can send the updates we have received.
	Loop:
		for {
			select {
			case <-nodeStatusUpdater.stopChan:
				return
			case <-limit:
				if len(nodeStatusMap) == 0 {
					// If nothing to update, wait until we have something to update.
					limit = nil
					continue
				}
				break Loop
			case ev, ok := <-nodeStatusUpdater.updateChan:
				if !ok {
					return
				}
				nodeStatusMap[ev.node] = *ev.CiliumNetworkPolicyNodeStatus
			}
		}

		// Return if we received a request to stop in case we selected on the
		// limit being hit or receiving an update even if this goroutine was
		// stopped, as `select` is nondeterministic in which `case` it hits.
		select {
		case <-nodeStatusUpdater.stopChan:
			return
		default:
		}

		var (
			cnp *types.SlimCNP
			err error
		)

		switch {
		// Patching doesn't need us to get the CNP from
		// the store because we can perform patches without
		// needing the actual CNP object itself.
		case k8sversion.Capabilities().Patch:
		default:
			cnp, err = getUpdatedCNPFromStore(c.k8sStore, namespace, name)
			if err != nil {
				scopedLog.WithError(err).Error("error getting updated cnp from store")
			}
		}

		// Now that we have collected all events for
		// the given CNP, update the status for all nodes
		// which have sent us updates.
		if err = updateStatusesByCapabilities(CiliumClient(), k8sversion.Capabilities(), cnp, namespace, name, nodeStatusMap); err != nil {
			scopedLog.WithError(err).Error("error updating status for CNP")
		}
	}
}

// StartStatusHandler starts the goroutine which sends status updates for the
// given CNP to the Kubernetes APIserver. If a status handler has already been
// started, it is a no-op.
func (c *CNPStatusEventHandler) StartStatusHandler(cnp *types.SlimCNP) {
	cnpKey := generateCNPKey(string(cnp.UID), cnp.Namespace, cnp.Name)
	nodeStatusUpdater, ok := c.eventMap.createIfNotExist(cnpKey)
	if ok {
		return
	}
	go c.runStatusHandler(cnpKey, cnp, nodeStatusUpdater)
}

func getKeyFromObjectMeta(t metaV1.ObjectMeta) string {
	if t.Namespace != "" {
		return path.Join(string(t.UID), t.Namespace, t.Name)
	}

	return path.Join(string(t.UID), t.Name)
}

func getUpdatedCNPFromStore(ciliumStore cache.Store, namespace, name string) (*types.SlimCNP, error) {
	nameNamespace := name
	if namespace != "" {
		nameNamespace = fmt.Sprintf("%s/%s", namespace, name)
	}

	serverRuleStore, exists, err := ciliumStore.GetByKey(nameNamespace)
	if err != nil {
		return nil, fmt.Errorf("unable to find v2.CiliumNetworkPolicy in local cache: %s", err)
	}
	if !exists {
		return nil, errors.New("v2.CiliumNetworkPolicy does not exist in local cache")
	}

	serverRule, ok := serverRuleStore.(*types.SlimCNP)
	if !ok {
		return nil, errors.New("received object of unknown type from API server, expecting v2.CiliumNetworkPolicy")
	}

	return serverRule, nil
}
