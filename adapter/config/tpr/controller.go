// Copyright 2017 Istio Authors
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

package tpr

import (
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/golang/glog"
	"github.com/golang/protobuf/proto"
	multierror "github.com/hashicorp/go-multierror"

	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/pkg/api"
	"k8s.io/client-go/tools/cache"

	"istio.io/pilot/model"
	"istio.io/pilot/platform/kube"
)

// controller is a collection of synchronized resource watchers.
// Caches are thread-safe
type controller struct {
	client *Client
	queue  kube.Queue
	kinds  map[string]cacheHandler
}

type cacheHandler struct {
	informer cache.SharedIndexInformer
	handler  *kube.ChainHandler
}

// NewController creates a new Kubernetes controller for TPRs
func NewController(client *Client, resyncPeriod time.Duration) model.ConfigStoreCache {
	// Queue requires a time duration for a retry delay after a handler error
	out := &controller{
		client: client,
		queue:  kube.NewQueue(1 * time.Second),
		kinds:  make(map[string]cacheHandler),
	}

	// add stores for TPR kinds
	for _, kind := range []string{IstioKind} {
		out.kinds[kind] = out.createInformer(&Config{}, resyncPeriod,
			func(opts meta_v1.ListOptions) (result runtime.Object, err error) {
				result = &ConfigList{}
				err = client.dynamic.Get().
					Namespace(client.namespace).
					Resource(kind+"s").
					VersionedParams(&opts, api.ParameterCodec).
					Do().
					Into(result)
				return
			},
			func(opts meta_v1.ListOptions) (watch.Interface, error) {
				return client.dynamic.Get().
					Prefix("watch").
					Namespace(client.namespace).
					Resource(kind+"s").
					VersionedParams(&opts, api.ParameterCodec).
					Watch()
			})
	}

	return out
}

// notify is the first handler in the handler chain.
// Returning an error causes repeated execution of the entire chain.
func (c *controller) notify(obj interface{}, event model.Event) error {
	if !c.HasSynced() {
		return errors.New("waiting till full synchronization")
	}
	k, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		glog.V(2).Infof("Error retrieving key: %v", err)
	} else {
		glog.V(2).Infof("Event %s: key %#v", event, k)
	}
	return nil
}

func (c *controller) createInformer(
	o runtime.Object,
	resyncPeriod time.Duration,
	lf cache.ListFunc,
	wf cache.WatchFunc) cacheHandler {
	handler := &kube.ChainHandler{}
	handler.Append(c.notify)

	// TODO: finer-grained index (perf)
	informer := cache.NewSharedIndexInformer(
		&cache.ListWatch{ListFunc: lf, WatchFunc: wf}, o,
		resyncPeriod, cache.Indexers{})

	informer.AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			// TODO: filtering functions to skip over un-referenced resources (perf)
			AddFunc: func(obj interface{}) {
				c.queue.Push(kube.NewTask(handler.Apply, obj, model.EventAdd))
			},
			UpdateFunc: func(old, cur interface{}) {
				if !reflect.DeepEqual(old, cur) {
					c.queue.Push(kube.NewTask(handler.Apply, cur, model.EventUpdate))
				}
			},
			DeleteFunc: func(obj interface{}) {
				c.queue.Push(kube.NewTask(handler.Apply, obj, model.EventDelete))
			},
		})

	return cacheHandler{informer: informer, handler: handler}
}

func (c *controller) RegisterEventHandler(typ string, f func(model.Config, model.Event)) {
	c.kinds[IstioKind].handler.Append(func(obj interface{}, ev model.Event) error {
		tpr, ok := obj.(*Config)
		if ok {
			config, err := c.client.convertConfig(tpr)
			if config.Type == typ {
				if err == nil {
					f(config, ev)
				} else {
					// Do not trigger re-application of handlers
					glog.Warningf("cannot convert kind %s to a config object", typ)
				}
			}
		}
		return nil
	})
}

func (c *controller) HasSynced() bool {
	for kind, ctl := range c.kinds {
		if !ctl.informer.HasSynced() {
			glog.V(2).Infof("controller %q is syncing...", kind)
			return false
		}
	}
	return true
}

func (c *controller) Run(stop <-chan struct{}) {
	go c.queue.Run(stop)

	for _, ctl := range c.kinds {
		go ctl.informer.Run(stop)
	}

	<-stop
	glog.V(2).Info("controller terminated")
}

func (c *controller) ConfigDescriptor() model.ConfigDescriptor {
	return c.client.ConfigDescriptor()
}

func (c *controller) Get(typ, key string) (proto.Message, bool, string) {
	schema, exists := c.client.ConfigDescriptor().GetByType(typ)
	if !exists {
		return nil, false, ""
	}

	store := c.kinds[IstioKind].informer.GetStore()
	data, exists, err := store.GetByKey(kube.KeyFunc(configKey(typ, key), c.client.namespace))
	if !exists {
		return nil, false, ""
	}
	if err != nil {
		glog.Warning(err)
		return nil, false, ""
	}

	config, ok := data.(*Config)
	if !ok {
		glog.Warning("Cannot convert to config from store")
		return nil, false, ""
	}

	out, err := schema.FromJSONMap(config.Spec)
	if err != nil {
		glog.Warning(err)
		return nil, false, ""
	}
	return out, true, config.Metadata.ResourceVersion
}

func (c *controller) Post(val proto.Message) (string, error) {
	return c.client.Post(val)
}

func (c *controller) Put(val proto.Message, revision string) (string, error) {
	return c.client.Put(val, revision)
}

func (c *controller) Delete(typ, key string) error {
	return c.client.Delete(typ, key)
}

func (c *controller) List(typ string) ([]model.Config, error) {
	if _, ok := c.client.ConfigDescriptor().GetByType(typ); !ok {
		return nil, fmt.Errorf("missing type %q", typ)
	}

	var errs error
	out := make([]model.Config, 0)
	for _, data := range c.kinds[IstioKind].informer.GetStore().List() {
		item, ok := data.(*Config)
		if ok {
			config, err := c.client.convertConfig(item)
			if config.Type == typ {
				if err != nil {
					errs = multierror.Append(errs, err)
				} else {
					out = append(out, config)
				}
			}
		}
	}
	return out, errs
}
